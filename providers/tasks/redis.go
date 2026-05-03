package tasks

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"time"

	rds "github.com/redis/go-redis/v9"
)

// RedisOpts configures NewRedisStore.
type RedisOpts struct {
	// Client is the Redis (or Valkey) client to use. Required.
	// The store reuses this client; close it via your own lifecycle
	// management.
	Client rds.UniversalClient

	// KeyPrefix is prepended to every key the store reads/writes.
	// Lets multiple mkfst processes share a Redis instance without
	// stomping on each other. Default "mkfst:tasks:".
	KeyPrefix string

	// DedupWindow mirrors the other backends — UniqueKey collisions
	// within this window are rejected. 0 disables dedup. Default 5m.
	DedupWindow time.Duration
}

// NewRedisStore returns a Store backed by Redis or Valkey. Uses
// atomic Lua scripts for claim/heartbeat/complete/fail/promote/
// reclaim so concurrent workers can never double-claim a task even
// across processes.
//
// The store does not own the *rds.Client; callers retain lifecycle
// control. Close() is a no-op.
func NewRedisStore(opts RedisOpts) (Store, error) {
	if opts.Client == nil {
		return nil, errors.New("tasks.NewRedisStore: Client is required")
	}
	if opts.KeyPrefix == "" {
		opts.KeyPrefix = "mkfst:tasks:"
	}
	if opts.DedupWindow == 0 {
		opts.DedupWindow = 5 * time.Minute
	}
	return &redisStore{
		client: opts.Client,
		prefix: opts.KeyPrefix,
		opts:   opts,
		now:    time.Now,
	}, nil
}

type redisStore struct {
	client rds.UniversalClient
	prefix string
	opts   RedisOpts
	now    func() time.Time
}

// === key helpers ===

func (s *redisStore) taskKey(id string) string         { return s.prefix + "task:" + id }
func (s *redisStore) pendingKey(queue string) string   { return s.prefix + "queue:" + queue + ":pending" }
func (s *redisStore) scheduledKey(queue string) string { return s.prefix + "queue:" + queue + ":scheduled" }
func (s *redisStore) runningKey(queue string) string   { return s.prefix + "queue:" + queue + ":running" }
func (s *redisStore) dedupKey(uniqueKey string) string { return s.prefix + "dedup:" + uniqueKey }
func (s *redisStore) statsKey(queue string) string     { return s.prefix + "queue:" + queue + ":stats" }

// === scoring ===
//
// Pending sorted set ordered by (priority desc, enqueuedAt asc),
// encoded into a single float64. ZPOPMIN takes the smallest score
// first, so we negate priority — priority 127 → score -127*1e13,
// priority -128 → score 128*1e13. Within the same priority, ties
// break by enqueuedAt offset (ms past epoch).
//
// Float64 has 53 bits of integer precision (~9e15). Worst-case score
// is 128 * 1e13 + ~1.7e12 (current ms-since-2020) = ~1.28e15, well
// within precision.
const epochMs int64 = 1577836800000 // 2020-01-01 UTC

func pendingScore(priority int8, enqueuedAt time.Time) float64 {
	return float64(-int(priority))*1e13 + float64(enqueuedAt.UnixMilli()-epochMs)
}

func msScore(t time.Time) float64 {
	return float64(t.UnixMilli() - epochMs)
}

// === Store impl ===

func (s *redisStore) Enqueue(ctx context.Context, t Task) (Record, error) {
	if t.ID == "" {
		t.ID = newID()
	}
	if t.Queue == "" {
		t.Queue = "default"
	}
	now := s.now()

	state := StatePending
	score := pendingScore(t.Priority, now)
	zsetKey := s.pendingKey(t.Queue)
	if !t.DelayUntil.IsZero() && t.DelayUntil.After(now) {
		state = StateScheduled
		score = msScore(t.DelayUntil)
		zsetKey = s.scheduledKey(t.Queue)
	}

	rec := Record{
		Task:       t,
		State:      state,
		EnqueuedAt: now,
	}

	// Dedup acquire: SET key val NX EX. Atomic. Returns OK on
	// successful acquire, nil on collision.
	if t.UniqueKey != "" && s.opts.DedupWindow > 0 {
		set, err := s.client.SetNX(ctx, s.dedupKey(t.UniqueKey), "1", s.opts.DedupWindow).Result()
		if err != nil {
			return Record{}, fmt.Errorf("dedup acquire: %w", err)
		}
		if !set {
			return Record{}, ErrUniqueViolation
		}
	}

	// Pipeline the hash insert + sorted-set add. Not strictly atomic
	// but only one writer per ID and we set the hash before the
	// sorted set, so claimers always see populated metadata.
	hashFields := taskHashFields(&rec, now)
	pipe := s.client.Pipeline()
	pipe.HSet(ctx, s.taskKey(t.ID), hashFields)
	pipe.ZAdd(ctx, zsetKey, rds.Z{Score: score, Member: t.ID})
	if _, err := pipe.Exec(ctx); err != nil {
		return Record{}, fmt.Errorf("enqueue: %w", err)
	}
	return rec, nil
}

func (s *redisStore) ScheduleAt(ctx context.Context, when time.Time, t Task) (Record, error) {
	t.DelayUntil = when
	return s.Enqueue(ctx, t)
}

// claimScript is the atomic-claim Lua. ZPOPMIN one ID from pending,
// move metadata to running state, return the ID. Empty result → no
// task to claim.
var claimScript = rds.NewScript(`
local pending = KEYS[1]
local running = KEYS[2]
local taskKeyPrefix = KEYS[3]
local workerID = ARGV[1]
local visScore = ARGV[2]
local visNanoStr = ARGV[3]
local nowNanoStr = ARGV[4]

local result = redis.call("ZPOPMIN", pending, 1)
if #result == 0 then
    return nil
end

-- ZPOPMIN returns {member1, score1, member2, score2, ...}
-- For LIMIT 1, we get {id, scoreStr}.
local id = result[1]
local taskKey = taskKeyPrefix .. id

redis.call("ZADD", running, visScore, id)
redis.call("HSET", taskKey,
    "state", "running",
    "owner_worker", workerID,
    "visibility_at", visNanoStr)
redis.call("HINCRBY", taskKey, "attempts", 1)

if redis.call("HGET", taskKey, "started_at") == "" or redis.call("HGET", taskKey, "started_at") == false then
    redis.call("HSET", taskKey, "started_at", nowNanoStr)
end

return id
`)

func (s *redisStore) Claim(ctx context.Context, queue, workerID string, visibility time.Duration) (*Record, error) {
	if queue == "" {
		queue = "default"
	}
	now := s.now()
	visUntil := now.Add(visibility)

	res, err := claimScript.Run(ctx,
		s.client,
		[]string{s.pendingKey(queue), s.runningKey(queue), s.prefix + "task:"},
		workerID, msScore(visUntil), visUntil.UnixNano(), now.UnixNano(),
	).Result()
	if err != nil && !errors.Is(err, rds.Nil) {
		return nil, fmt.Errorf("claim script: %w", err)
	}
	if res == nil || errors.Is(err, rds.Nil) {
		return nil, nil
	}
	id, ok := res.(string)
	if !ok || id == "" {
		return nil, nil
	}

	// Read back the full record. Could fold this into the script for
	// fewer RTTs; keeping it separate for code clarity.
	rec, err := s.Inspect(ctx, id)
	if err != nil {
		return nil, err
	}
	return &rec, nil
}

var heartbeatScript = rds.NewScript(`
local taskKey = KEYS[1]
local running = KEYS[2]
local id = KEYS[3]
local workerID = ARGV[1]
local visScore = ARGV[2]
local visNanoStr = ARGV[3]

local owner = redis.call("HGET", taskKey, "owner_worker")
if owner == false then return -1 end
if owner ~= workerID then return 0 end

redis.call("ZADD", running, visScore, id)
redis.call("HSET", taskKey, "visibility_at", visNanoStr)
return 1
`)

func (s *redisStore) Heartbeat(ctx context.Context, id, workerID string, extend time.Duration) error {
	visUntil := s.now().Add(extend)
	res, err := heartbeatScript.Run(ctx,
		s.client,
		[]string{s.taskKey(id), s.runningKey(s.taskQueue(ctx, id)), id},
		workerID, msScore(visUntil), visUntil.UnixNano(),
	).Int()
	if err != nil {
		return fmt.Errorf("heartbeat: %w", err)
	}
	switch res {
	case 1:
		return nil
	case 0:
		return ErrNotOwner
	default:
		return ErrNotFound
	}
}

var completeScript = rds.NewScript(`
local taskKey = KEYS[1]
local running = KEYS[2]
local id = KEYS[3]
local workerID = ARGV[1]
local nowStr = ARGV[2]

local owner = redis.call("HGET", taskKey, "owner_worker")
if owner == false then return -1 end
if owner ~= workerID then return 0 end
local state = redis.call("HGET", taskKey, "state")
if state ~= "running" then return -2 end

redis.call("ZREM", running, id)
redis.call("HSET", taskKey,
    "state", "completed",
    "completed_at", nowStr,
    "owner_worker", "")
return 1
`)

func (s *redisStore) Complete(ctx context.Context, id, workerID string) error {
	now := s.now()
	res, err := completeScript.Run(ctx,
		s.client,
		[]string{s.taskKey(id), s.runningKey(s.taskQueue(ctx, id)), id},
		workerID, now.UnixNano(),
	).Int()
	if err != nil {
		return fmt.Errorf("complete: %w", err)
	}
	switch res {
	case 1:
		return nil
	case 0:
		return ErrNotOwner
	case -2:
		return ErrAlreadyTerminal
	default:
		return ErrNotFound
	}
}

var failScript = rds.NewScript(`
local taskKey = KEYS[1]
local running = KEYS[2]
local scheduled = KEYS[3]
local id = KEYS[4]
local workerID = ARGV[1]
local errMsg = ARGV[2]
local nowStr = ARGV[3]
local hasRetry = ARGV[4]
local retryScore = ARGV[5]
local retryNanoStr = ARGV[6]
local deadlineNanoStr = ARGV[7]
local nowNumStr = ARGV[8]

local owner = redis.call("HGET", taskKey, "owner_worker")
if owner == false then return -1 end
if owner ~= workerID then return 0 end
local state = redis.call("HGET", taskKey, "state")
if state ~= "running" then return -2 end

redis.call("ZREM", running, id)

local doRetry = (hasRetry == "1")
if doRetry and deadlineNanoStr ~= "" then
    -- Honor deadline: skip retry if it's past the deadline.
    if tonumber(retryNanoStr) >= tonumber(deadlineNanoStr) then
        doRetry = false
    end
end

if doRetry then
    redis.call("ZADD", scheduled, retryScore, id)
    redis.call("HSET", taskKey,
        "state", "scheduled",
        "delay_until", retryNanoStr,
        "next_attempt", retryNanoStr,
        "last_error", errMsg,
        "owner_worker", "")
else
    redis.call("HSET", taskKey,
        "state", "failed",
        "completed_at", nowStr,
        "last_error", errMsg,
        "owner_worker", "")
end
return 1
`)

func (s *redisStore) Fail(ctx context.Context, id, workerID string, errMsg string, nextAttemptAt *time.Time) error {
	now := s.now()
	hasRetry := "0"
	retryScore := float64(0)
	retryNanoStr := ""
	if nextAttemptAt != nil {
		hasRetry = "1"
		retryScore = msScore(*nextAttemptAt)
		retryNanoStr = strconv.FormatInt(nextAttemptAt.UnixNano(), 10)
	}

	// Read deadline & queue first to plug into the script. One round
	// trip + script execution; could fold deadline read into the
	// script via HGET to save one RTT later.
	taskKey := s.taskKey(id)
	deadlineStr, _ := s.client.HGet(ctx, taskKey, "deadline").Result()
	queue := s.taskQueue(ctx, id)

	res, err := failScript.Run(ctx,
		s.client,
		[]string{taskKey, s.runningKey(queue), s.scheduledKey(queue), id},
		workerID, errMsg, now.UnixNano(), hasRetry, retryScore, retryNanoStr, deadlineStr, now.UnixNano(),
	).Int()
	if err != nil {
		return fmt.Errorf("fail: %w", err)
	}
	switch res {
	case 1:
		return nil
	case 0:
		return ErrNotOwner
	case -2:
		return ErrAlreadyTerminal
	default:
		return ErrNotFound
	}
}

var cancelScript = rds.NewScript(`
local taskKey = KEYS[1]
local pending = KEYS[2]
local scheduled = KEYS[3]
local id = KEYS[4]
local nowStr = ARGV[1]

local state = redis.call("HGET", taskKey, "state")
if state == false then return -1 end

if state == "pending" then
    redis.call("ZREM", pending, id)
    redis.call("HSET", taskKey, "state", "cancelled", "completed_at", nowStr)
    return 1
elseif state == "scheduled" then
    redis.call("ZREM", scheduled, id)
    redis.call("HSET", taskKey, "state", "cancelled", "completed_at", nowStr)
    return 1
elseif state == "running" then
    redis.call("HSET", taskKey, "max_retries", "0")
    return 1
end
return -2
`)

func (s *redisStore) Cancel(ctx context.Context, id string) error {
	now := s.now()
	queue := s.taskQueue(ctx, id)

	res, err := cancelScript.Run(ctx,
		s.client,
		[]string{s.taskKey(id), s.pendingKey(queue), s.scheduledKey(queue), id},
		now.UnixNano(),
	).Int()
	if err != nil {
		return fmt.Errorf("cancel: %w", err)
	}
	switch res {
	case 1:
		return nil
	case -1:
		return ErrNotFound
	case -2:
		return ErrAlreadyTerminal
	default:
		return nil
	}
}

func (s *redisStore) Inspect(ctx context.Context, id string) (Record, error) {
	fields, err := s.client.HGetAll(ctx, s.taskKey(id)).Result()
	if err != nil {
		return Record{}, fmt.Errorf("inspect: %w", err)
	}
	if len(fields) == 0 {
		return Record{}, ErrNotFound
	}
	return parseTaskHash(fields)
}

func (s *redisStore) QueueStats(ctx context.Context, queue string) (QueueStats, error) {
	if queue == "" {
		queue = "default"
	}
	pipe := s.client.Pipeline()
	pendingCmd := pipe.ZCard(ctx, s.pendingKey(queue))
	scheduledCmd := pipe.ZCard(ctx, s.scheduledKey(queue))
	runningCmd := pipe.ZCard(ctx, s.runningKey(queue))
	if _, err := pipe.Exec(ctx); err != nil {
		return QueueStats{}, fmt.Errorf("stats: %w", err)
	}
	stats := QueueStats{
		Pending:   int(pendingCmd.Val()),
		Scheduled: int(scheduledCmd.Val()),
		Running:   int(runningCmd.Val()),
	}
	// Failed count is not tracked in a sorted set (failed tasks
	// remain only in the hash). Skip for now; add a separate failed
	// set if observability requires it.
	if stats.Pending > 0 {
		// Oldest pending: ZRANGE WITHSCORES first element. The score
		// has priority + ts encoded; we can decode the timestamp by
		// modulo. But cheaper to just HGET its enqueued_at.
		ids, err := s.client.ZRange(ctx, s.pendingKey(queue), 0, 0).Result()
		if err == nil && len(ids) > 0 {
			tsStr, err := s.client.HGet(ctx, s.taskKey(ids[0]), "enqueued_at").Result()
			if err == nil {
				if ns, perr := strconv.ParseInt(tsStr, 10, 64); perr == nil {
					stats.OldestPendingAge = s.now().Sub(time.Unix(0, ns))
				}
			}
		}
	}
	return stats, nil
}

var promoteScheduledScript = rds.NewScript(`
local scheduled = KEYS[1]
local pending = KEYS[2]
local taskKeyPrefix = KEYS[3]
local nowScore = tonumber(ARGV[1])

local promoted = 0
while true do
    local batch = redis.call("ZRANGEBYSCORE", scheduled, "-inf", nowScore, "LIMIT", 0, 100)
    if #batch == 0 then break end
    for _, id in ipairs(batch) do
        redis.call("ZREM", scheduled, id)
        local taskKey = taskKeyPrefix .. id
        local state = redis.call("HGET", taskKey, "state")
        if state == "scheduled" then
            local pri = tonumber(redis.call("HGET", taskKey, "priority")) or 0
            local enqAt = tonumber(redis.call("HGET", taskKey, "enqueued_at")) or 0
            -- pendingScore = -pri * 1e13 + (enqAt_ms - epochMs)
            local enqMs = math.floor(enqAt / 1000000)
            local newScore = (-pri) * 1e13 + (enqMs - 1577836800000)
            redis.call("ZADD", pending, newScore, id)
            redis.call("HSET", taskKey, "state", "pending")
            promoted = promoted + 1
        end
    end
end
return promoted
`)

func (s *redisStore) PromoteScheduled(ctx context.Context, queue string, now time.Time) (int, error) {
	if queue == "" {
		queue = "default"
	}
	res, err := promoteScheduledScript.Run(ctx,
		s.client,
		[]string{s.scheduledKey(queue), s.pendingKey(queue), s.prefix + "task:"},
		msScore(now),
	).Int()
	if err != nil {
		return 0, fmt.Errorf("promote: %w", err)
	}
	return res, nil
}

var reclaimScript = rds.NewScript(`
local running = KEYS[1]
local pending = KEYS[2]
local taskKeyPrefix = KEYS[3]
local nowScore = tonumber(ARGV[1])

local reclaimed = 0
while true do
    local batch = redis.call("ZRANGEBYSCORE", running, "-inf", nowScore, "LIMIT", 0, 100)
    if #batch == 0 then break end
    for _, id in ipairs(batch) do
        redis.call("ZREM", running, id)
        local taskKey = taskKeyPrefix .. id
        local state = redis.call("HGET", taskKey, "state")
        if state == "running" then
            local pri = tonumber(redis.call("HGET", taskKey, "priority")) or 0
            local enqAt = tonumber(redis.call("HGET", taskKey, "enqueued_at")) or 0
            local enqMs = math.floor(enqAt / 1000000)
            local newScore = (-pri) * 1e13 + (enqMs - 1577836800000)
            redis.call("ZADD", pending, newScore, id)
            redis.call("HSET", taskKey, "state", "pending", "owner_worker", "")
            reclaimed = reclaimed + 1
        end
    end
end
return reclaimed
`)

func (s *redisStore) ReclaimExpired(ctx context.Context, queue string, now time.Time) (int, error) {
	if queue == "" {
		queue = "default"
	}
	res, err := reclaimScript.Run(ctx,
		s.client,
		[]string{s.runningKey(queue), s.pendingKey(queue), s.prefix + "task:"},
		msScore(now),
	).Int()
	if err != nil {
		return 0, fmt.Errorf("reclaim: %w", err)
	}
	return res, nil
}

func (s *redisStore) PurgeOlderThan(ctx context.Context, cutoff time.Time) (int, error) {
	// SCAN through task keys, filter by state + completed_at. SCAN is
	// O(n) but cheap in Redis (cursor-based, doesn't block). For
	// large datasets this could be optimized with a separate "done"
	// sorted set keyed by completed_at; deferring until needed.
	var purged int
	var cursor uint64
	pattern := s.prefix + "task:*"
	for {
		keys, next, err := s.client.Scan(ctx, cursor, pattern, 256).Result()
		if err != nil {
			return purged, fmt.Errorf("purge scan: %w", err)
		}
		for _, key := range keys {
			h, err := s.client.HMGet(ctx, key, "state", "completed_at").Result()
			if err != nil {
				continue
			}
			state, _ := h[0].(string)
			if state != "completed" && state != "failed" && state != "cancelled" {
				continue
			}
			completedStr, _ := h[1].(string)
			if completedStr == "" {
				continue
			}
			ns, perr := strconv.ParseInt(completedStr, 10, 64)
			if perr != nil {
				continue
			}
			if time.Unix(0, ns).Before(cutoff) {
				if _, err := s.client.Del(ctx, key).Result(); err == nil {
					purged++
				}
			}
		}
		cursor = next
		if cursor == 0 {
			break
		}
	}
	return purged, nil
}

func (s *redisStore) Close() error {
	// We don't own the client. No-op.
	return nil
}

// taskQueue returns the queue name for a task ID by reading the
// hash's queue field. Best-effort; falls back to "default" on error.
// Used by Heartbeat/Complete/Fail/Cancel to compute key names —
// tasks can be in any queue.
func (s *redisStore) taskQueue(ctx context.Context, id string) string {
	q, err := s.client.HGet(ctx, s.taskKey(id), "queue").Result()
	if err != nil || q == "" {
		return "default"
	}
	return q
}

// === task ↔ hash conversion ===

func taskHashFields(rec *Record, now time.Time) map[string]interface{} {
	out := map[string]interface{}{
		"id":            rec.Task.ID,
		"type":          rec.Task.Type,
		"queue":         rec.Task.Queue,
		"priority":      strconv.Itoa(int(rec.Task.Priority)),
		"state":         string(rec.State),
		"attempts":      "0",
		"timeout_ns":    strconv.FormatInt(int64(rec.Task.Timeout), 10),
		"enqueued_at":   strconv.FormatInt(now.UnixNano(), 10),
		"unique_key":    rec.Task.UniqueKey,
		"started_at":    "",
		"completed_at":  "",
		"last_error":    "",
		"next_attempt":  "",
		"owner_worker":  "",
		"visibility_at": "",
	}
	if len(rec.Task.Payload) > 0 {
		out["payload"] = base64.StdEncoding.EncodeToString(rec.Task.Payload)
	} else {
		out["payload"] = ""
	}
	if rec.Task.MaxRetries != nil {
		out["max_retries"] = strconv.Itoa(*rec.Task.MaxRetries)
	} else {
		out["max_retries"] = ""
	}
	if !rec.Task.Deadline.IsZero() {
		out["deadline"] = strconv.FormatInt(rec.Task.Deadline.UnixNano(), 10)
	} else {
		out["deadline"] = ""
	}
	if !rec.Task.DelayUntil.IsZero() {
		out["delay_until"] = strconv.FormatInt(rec.Task.DelayUntil.UnixNano(), 10)
	} else {
		out["delay_until"] = ""
	}
	if len(rec.Task.Tags) > 0 {
		if b, err := json.Marshal(rec.Task.Tags); err == nil {
			out["tags"] = string(b)
		} else {
			out["tags"] = ""
		}
	} else {
		out["tags"] = ""
	}
	return out
}

func parseTaskHash(fields map[string]string) (Record, error) {
	rec := Record{
		Task: Task{
			ID:        fields["id"],
			Type:      fields["type"],
			Queue:     fields["queue"],
			UniqueKey: fields["unique_key"],
		},
		State:       State(fields["state"]),
		LastError:   fields["last_error"],
		OwnerWorker: fields["owner_worker"],
	}
	if rec.Task.ID == "" {
		return Record{}, ErrNotFound
	}

	if pri, err := strconv.Atoi(fields["priority"]); err == nil {
		rec.Task.Priority = int8(pri)
	}
	if a, err := strconv.Atoi(fields["attempts"]); err == nil {
		rec.Attempts = a
	}
	if t, err := strconv.ParseInt(fields["timeout_ns"], 10, 64); err == nil {
		rec.Task.Timeout = time.Duration(t)
	}
	if mr := fields["max_retries"]; mr != "" {
		if v, err := strconv.Atoi(mr); err == nil {
			rec.Task.MaxRetries = &v
		}
	}
	if p := fields["payload"]; p != "" {
		if b, err := base64.StdEncoding.DecodeString(p); err == nil {
			rec.Task.Payload = b
		}
	}
	rec.Task.Deadline = parseUnixNano(fields["deadline"])
	rec.Task.DelayUntil = parseUnixNano(fields["delay_until"])
	rec.EnqueuedAt = parseUnixNano(fields["enqueued_at"])
	rec.StartedAt = parseUnixNano(fields["started_at"])
	rec.CompletedAt = parseUnixNano(fields["completed_at"])
	rec.NextAttempt = parseUnixNano(fields["next_attempt"])
	rec.VisibilityAt = parseUnixNano(fields["visibility_at"])

	if tags := fields["tags"]; tags != "" {
		_ = json.Unmarshal([]byte(tags), &rec.Task.Tags)
	}
	return rec, nil
}

func parseUnixNano(s string) time.Time {
	if s == "" {
		return time.Time{}
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil || n == 0 {
		return time.Time{}
	}
	return time.Unix(0, n).UTC()
}

