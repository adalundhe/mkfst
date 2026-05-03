package tasks

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"mkfst/db"
)

// dialect represents one of the SQL backends supported via
// mkfst's db.Connection. Switched on db.ConnectionInfo.Type.
type dialect int

const (
	dialectSQLite dialect = iota
	dialectPostgres
	dialectMySQL
)

func dialectFor(connType string) (dialect, error) {
	switch strings.ToUpper(connType) {
	case "", "SQLITE":
		return dialectSQLite, nil
	case "POSTGRESQL", "POSTGRES":
		return dialectPostgres, nil
	case "MYSQL":
		return dialectMySQL, nil
	default:
		return 0, fmt.Errorf("tasks.NewSQLStore: unsupported db type %q", connType)
	}
}

// SQLOpts configures NewSQLStore.
type SQLOpts struct {
	// DedupWindow mirrors MemoryOpts.DedupWindow — enforces UniqueKey
	// dedup at the row level via an auxiliary mkfst_dedup table.
	// 0 disables dedup. Default 5 minutes.
	DedupWindow time.Duration
	// TablePrefix is prepended to every table name; useful when
	// sharing a schema across multiple mkfst processes or test
	// suites. Default "mkfst_".
	TablePrefix string
}

// NewSQLStore returns a Store backed by an SQL database accessed
// through mkfst's db.Connection. Supported backends: PostgreSQL,
// MySQL 8.0.1+, SQLite.
//
// On first use, NewSQLStore runs idempotent CREATE TABLE IF NOT
// EXISTS migrations against the supplied connection. Subsequent
// constructions are no-ops on the schema.
//
// PostgreSQL and MySQL use SELECT ... FOR UPDATE SKIP LOCKED for
// concurrent-claim correctness. SQLite serializes writers via
// BEGIN IMMEDIATE — slower under contention but always correct
// (SQLite is a single-writer database by design).
func NewSQLStore(conn *db.Connection, opts SQLOpts) (Store, error) {
	if conn == nil || conn.Conn == nil {
		return nil, errors.New("tasks.NewSQLStore: nil connection")
	}
	d, err := dialectFor(conn.Config.Type)
	if err != nil {
		return nil, err
	}
	if opts.DedupWindow == 0 {
		opts.DedupWindow = 5 * time.Minute
	}
	if opts.TablePrefix == "" {
		opts.TablePrefix = "mkfst_"
	}
	s := &sqlStore{
		db:      conn.Conn,
		dialect: d,
		opts:    opts,
		now:     time.Now,
	}
	if err := s.migrate(context.Background()); err != nil {
		return nil, fmt.Errorf("tasks.NewSQLStore: migrate: %w", err)
	}
	return s, nil
}

type sqlStore struct {
	db      *sql.DB
	dialect dialect
	opts    SQLOpts
	now     func() time.Time
}

// === schema / migrations ===

func (s *sqlStore) tasksTable() string { return s.opts.TablePrefix + "tasks" }
func (s *sqlStore) dedupTable() string { return s.opts.TablePrefix + "dedup" }

// migrate creates the required tables and indices. Idempotent — safe
// to re-run on every NewSQLStore. Per-dialect SQL because column
// types and identifier quoting differ.
func (s *sqlStore) migrate(ctx context.Context) error {
	stmts := s.migrationStatements()
	for _, stmt := range stmts {
		if _, err := s.db.ExecContext(ctx, stmt); err != nil {
			return fmt.Errorf("migrate %q: %w", firstLine(stmt), err)
		}
	}
	return nil
}

func (s *sqlStore) migrationStatements() []string {
	tt := s.tasksTable()
	dt := s.dedupTable()
	switch s.dialect {
	case dialectPostgres:
		return []string{
			fmt.Sprintf(`CREATE TABLE IF NOT EXISTS %s (
				id            VARCHAR(64) PRIMARY KEY,
				type          VARCHAR(255) NOT NULL,
				payload       BYTEA,
				queue         VARCHAR(255) NOT NULL,
				priority      SMALLINT NOT NULL DEFAULT 0,
				state         VARCHAR(20) NOT NULL,
				attempts      INTEGER NOT NULL DEFAULT 0,
				max_retries   INTEGER,
				timeout_ns    BIGINT NOT NULL DEFAULT 0,
				deadline      TIMESTAMPTZ,
				delay_until   TIMESTAMPTZ,
				enqueued_at   TIMESTAMPTZ NOT NULL,
				started_at    TIMESTAMPTZ,
				completed_at  TIMESTAMPTZ,
				last_error    TEXT,
				next_attempt  TIMESTAMPTZ,
				owner_worker  VARCHAR(128),
				visibility_at TIMESTAMPTZ,
				unique_key    VARCHAR(255),
				tags          TEXT
			)`, tt),
			fmt.Sprintf(`CREATE INDEX IF NOT EXISTS %s_queue_pending ON %s (queue, priority DESC, enqueued_at ASC) WHERE state = 'pending'`, tt, tt),
			fmt.Sprintf(`CREATE INDEX IF NOT EXISTS %s_queue_scheduled ON %s (queue, delay_until) WHERE state = 'scheduled'`, tt, tt),
			fmt.Sprintf(`CREATE INDEX IF NOT EXISTS %s_queue_running ON %s (queue, visibility_at) WHERE state = 'running'`, tt, tt),
			fmt.Sprintf(`CREATE TABLE IF NOT EXISTS %s (
				key         VARCHAR(255) PRIMARY KEY,
				expires_at  TIMESTAMPTZ NOT NULL
			)`, dt),
		}
	case dialectMySQL:
		return []string{
			fmt.Sprintf("CREATE TABLE IF NOT EXISTS %s ("+
				"id VARCHAR(64) PRIMARY KEY, "+
				"type VARCHAR(255) NOT NULL, "+
				"payload BLOB, "+
				"queue VARCHAR(255) NOT NULL, "+
				"priority SMALLINT NOT NULL DEFAULT 0, "+
				"state VARCHAR(20) NOT NULL, "+
				"attempts INT NOT NULL DEFAULT 0, "+
				"max_retries INT, "+
				"timeout_ns BIGINT NOT NULL DEFAULT 0, "+
				"deadline DATETIME(6), "+
				"delay_until DATETIME(6), "+
				"enqueued_at DATETIME(6) NOT NULL, "+
				"started_at DATETIME(6), "+
				"completed_at DATETIME(6), "+
				"last_error TEXT, "+
				"next_attempt DATETIME(6), "+
				"owner_worker VARCHAR(128), "+
				"visibility_at DATETIME(6), "+
				"unique_key VARCHAR(255), "+
				"tags TEXT, "+
				"INDEX idx_queue_state_pri (queue, state, priority, enqueued_at), "+
				"INDEX idx_queue_state_dly (queue, state, delay_until), "+
				"INDEX idx_queue_state_vis (queue, state, visibility_at)"+
				") ENGINE=InnoDB DEFAULT CHARSET=utf8mb4", tt),
			fmt.Sprintf("CREATE TABLE IF NOT EXISTS %s ("+
				"`key` VARCHAR(255) PRIMARY KEY, "+
				"expires_at DATETIME(6) NOT NULL"+
				") ENGINE=InnoDB DEFAULT CHARSET=utf8mb4", dt),
		}
	default: // SQLite
		return []string{
			fmt.Sprintf(`CREATE TABLE IF NOT EXISTS %s (
				id            TEXT PRIMARY KEY,
				type          TEXT NOT NULL,
				payload       BLOB,
				queue         TEXT NOT NULL,
				priority      INTEGER NOT NULL DEFAULT 0,
				state         TEXT NOT NULL,
				attempts      INTEGER NOT NULL DEFAULT 0,
				max_retries   INTEGER,
				timeout_ns    INTEGER NOT NULL DEFAULT 0,
				deadline      INTEGER,
				delay_until   INTEGER,
				enqueued_at   INTEGER NOT NULL,
				started_at    INTEGER,
				completed_at  INTEGER,
				last_error    TEXT,
				next_attempt  INTEGER,
				owner_worker  TEXT,
				visibility_at INTEGER,
				unique_key    TEXT,
				tags          TEXT
			)`, tt),
			fmt.Sprintf(`CREATE INDEX IF NOT EXISTS %s_qsp ON %s (queue, state, priority, enqueued_at)`, tt, tt),
			fmt.Sprintf(`CREATE INDEX IF NOT EXISTS %s_qsd ON %s (queue, state, delay_until)`, tt, tt),
			fmt.Sprintf(`CREATE INDEX IF NOT EXISTS %s_qsv ON %s (queue, state, visibility_at)`, tt, tt),
			fmt.Sprintf(`CREATE TABLE IF NOT EXISTS %s (
				key         TEXT PRIMARY KEY,
				expires_at  INTEGER NOT NULL
			)`, dt),
		}
	}
}

// === Store impl ===

func (s *sqlStore) Enqueue(ctx context.Context, t Task) (Record, error) {
	if t.ID == "" {
		t.ID = newID()
	}
	if t.Queue == "" {
		t.Queue = "default"
	}
	now := s.now()

	state := StatePending
	if !t.DelayUntil.IsZero() && t.DelayUntil.After(now) {
		state = StateScheduled
	}

	rec := Record{
		Task:       t,
		State:      state,
		EnqueuedAt: now,
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Record{}, fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	// Dedup acquire (if UniqueKey set).
	if t.UniqueKey != "" && s.opts.DedupWindow > 0 {
		acquired, err := s.acquireDedupTx(ctx, tx, t.UniqueKey, now)
		if err != nil {
			return Record{}, err
		}
		if !acquired {
			return Record{}, ErrUniqueViolation
		}
	}

	tagsJSON, err := encodeTags(t.Tags)
	if err != nil {
		return Record{}, err
	}
	if err := s.insertTaskTx(ctx, tx, &rec, tagsJSON); err != nil {
		return Record{}, err
	}
	if err := tx.Commit(); err != nil {
		return Record{}, fmt.Errorf("commit: %w", err)
	}
	return rec, nil
}

func (s *sqlStore) ScheduleAt(ctx context.Context, when time.Time, t Task) (Record, error) {
	t.DelayUntil = when
	return s.Enqueue(ctx, t)
}

func (s *sqlStore) Claim(ctx context.Context, queue, workerID string, visibility time.Duration) (*Record, error) {
	if queue == "" {
		queue = "default"
	}
	now := s.now()
	visibleUntil := now.Add(visibility)

	// Per-dialect claim strategy:
	//
	//   PG / SQLite: single-statement UPDATE ... WHERE id = (SELECT
	//     ... LIMIT 1) RETURNING. Atomic, no transaction needed,
	//     no concurrent-claim conflict because the inner SELECT is
	//     evaluated under the UPDATE's write lock. PG additionally
	//     uses FOR UPDATE SKIP LOCKED in the inner SELECT so two
	//     concurrent claims pick distinct rows instead of one
	//     blocking the other.
	//
	//   MySQL: no UPDATE ... RETURNING (until 8.0.21+ for limited
	//     forms only). Must use a transaction: SELECT FOR UPDATE SKIP
	//     LOCKED to lock one row, then UPDATE. The transaction's
	//     SKIP LOCKED ensures concurrent claims pick distinct rows.
	id, err := s.claimAtomic(ctx, queue, workerID, now, visibleUntil)
	if err != nil {
		return nil, err
	}
	if id == "" {
		return nil, nil
	}

	rec, err := s.Inspect(ctx, id)
	if err != nil {
		return nil, err
	}
	return &rec, nil
}

// claimAtomic returns the ID of the claimed task, "" if none, or an
// error. Per-dialect implementation; see Claim's header for design.
func (s *sqlStore) claimAtomic(ctx context.Context, queue, workerID string, now, visibleUntil time.Time) (string, error) {
	switch s.dialect {
	case dialectPostgres:
		q := s.rebind(`UPDATE ` + s.tasksTable() + `
			SET state = 'running',
			    attempts = attempts + 1,
			    owner_worker = ?,
			    visibility_at = ?,
			    started_at = COALESCE(started_at, ?)
			WHERE id = (
				SELECT id FROM ` + s.tasksTable() + `
				WHERE queue = ? AND state = 'pending'
				ORDER BY priority DESC, enqueued_at ASC
				LIMIT 1
				FOR UPDATE SKIP LOCKED
			)
			RETURNING id`)
		row := s.db.QueryRowContext(ctx, q, workerID, s.encodeTime(visibleUntil), s.encodeTime(now), queue)
		var id string
		if err := row.Scan(&id); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return "", nil
			}
			return "", fmt.Errorf("claim: %w", err)
		}
		return id, nil

	case dialectSQLite:
		q := `UPDATE ` + s.tasksTable() + `
			SET state = 'running',
			    attempts = attempts + 1,
			    owner_worker = ?,
			    visibility_at = ?,
			    started_at = COALESCE(started_at, ?)
			WHERE id = (
				SELECT id FROM ` + s.tasksTable() + `
				WHERE queue = ? AND state = 'pending'
				ORDER BY priority DESC, enqueued_at ASC
				LIMIT 1
			)
			RETURNING id`
		row := s.db.QueryRowContext(ctx, q, workerID, s.encodeTime(visibleUntil), s.encodeTime(now), queue)
		var id string
		if err := row.Scan(&id); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return "", nil
			}
			return "", fmt.Errorf("claim: %w", err)
		}
		return id, nil

	default: // MySQL
		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			return "", fmt.Errorf("begin: %w", err)
		}
		defer func() { _ = tx.Rollback() }()

		findQ := s.rebind(`SELECT id FROM ` + s.tasksTable() + `
			WHERE queue = ? AND state = 'pending'
			ORDER BY priority DESC, enqueued_at ASC
			LIMIT 1
			FOR UPDATE SKIP LOCKED`)
		var id string
		if err := tx.QueryRowContext(ctx, findQ, queue).Scan(&id); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return "", nil
			}
			return "", fmt.Errorf("claim find: %w", err)
		}
		updQ := s.rebind(`UPDATE ` + s.tasksTable() + `
			SET state = 'running',
			    attempts = attempts + 1,
			    owner_worker = ?,
			    visibility_at = ?,
			    started_at = COALESCE(started_at, ?)
			WHERE id = ?`)
		if _, err := tx.ExecContext(ctx, updQ, workerID, s.encodeTime(visibleUntil), s.encodeTime(now), id); err != nil {
			return "", fmt.Errorf("claim update: %w", err)
		}
		if err := tx.Commit(); err != nil {
			return "", fmt.Errorf("claim commit: %w", err)
		}
		return id, nil
	}
}

func (s *sqlStore) Heartbeat(ctx context.Context, id, workerID string, extend time.Duration) error {
	q := s.rebind(`UPDATE ` + s.tasksTable() + `
		SET visibility_at = ?
		WHERE id = ? AND owner_worker = ? AND state = 'running'`)
	res, err := s.db.ExecContext(ctx, q, s.encodeTime(s.now().Add(extend)), id, workerID)
	if err != nil {
		return fmt.Errorf("heartbeat: %w", err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		// Either task doesn't exist or worker no longer owns it.
		return s.classifyOwnershipMissTx(ctx, id)
	}
	return nil
}

func (s *sqlStore) Complete(ctx context.Context, id, workerID string) error {
	now := s.now()
	q := s.rebind(`UPDATE ` + s.tasksTable() + `
		SET state = 'completed', completed_at = ?, owner_worker = NULL
		WHERE id = ? AND owner_worker = ? AND state = 'running'`)
	res, err := s.db.ExecContext(ctx, q, s.encodeTime(now), id, workerID)
	if err != nil {
		return fmt.Errorf("complete: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return s.classifyOwnershipMissTx(ctx, id)
	}
	return nil
}

func (s *sqlStore) Fail(ctx context.Context, id, workerID string, errMsg string, nextAttemptAt *time.Time) error {
	now := s.now()

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	// Need to read deadline to honor it w.r.t. retries. Use a generic
	// scan target so the SQLite-int / PG-time difference is hidden.
	getQ := s.rebind(`SELECT deadline FROM ` + s.tasksTable() + ` WHERE id = ? AND owner_worker = ? AND state = 'running'`)
	var deadlineRaw interface{}
	if err := tx.QueryRowContext(ctx, getQ, id, workerID).Scan(&deadlineRaw); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return s.classifyOwnershipMissTx(ctx, id)
		}
		return fmt.Errorf("fail load: %w", err)
	}
	deadline := s.decodeTime(deadlineRaw)

	if nextAttemptAt != nil && (deadline.IsZero() || nextAttemptAt.Before(deadline)) {
		// Retry: schedule.
		upd := s.rebind(`UPDATE ` + s.tasksTable() + `
			SET state = 'scheduled',
			    delay_until = ?,
			    next_attempt = ?,
			    last_error = ?,
			    owner_worker = NULL
			WHERE id = ?`)
		if _, err := tx.ExecContext(ctx, upd, s.encodeTime(*nextAttemptAt), s.encodeTime(*nextAttemptAt), errMsg, id); err != nil {
			return fmt.Errorf("fail retry: %w", err)
		}
	} else {
		// Terminal failure.
		upd := s.rebind(`UPDATE ` + s.tasksTable() + `
			SET state = 'failed',
			    completed_at = ?,
			    last_error = ?,
			    owner_worker = NULL
			WHERE id = ?`)
		if _, err := tx.ExecContext(ctx, upd, s.encodeTime(now), errMsg, id); err != nil {
			return fmt.Errorf("fail terminal: %w", err)
		}
	}
	return tx.Commit()
}

func (s *sqlStore) Cancel(ctx context.Context, id string) error {
	now := s.now()
	q := s.rebind(`UPDATE ` + s.tasksTable() + `
		SET state = 'cancelled', completed_at = ?
		WHERE id = ? AND state IN ('pending', 'scheduled')`)
	res, err := s.db.ExecContext(ctx, q, s.encodeTime(now), id)
	if err != nil {
		return fmt.Errorf("cancel: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		// Task either doesn't exist or is in running/completed/failed.
		// Mirror memory store: return ErrNotFound or ErrAlreadyTerminal.
		exists, err := s.taskExists(ctx, id)
		if err != nil {
			return err
		}
		if !exists {
			return ErrNotFound
		}
		// Running tasks: zero out max_retries to suppress retry on
		// subsequent Fail (matches memory-store semantics). Surface
		// the error if it fires — silent failure here would let a
		// "cancelled" running task still retry, defeating the cancel.
		zeroQ := s.rebind(`UPDATE ` + s.tasksTable() + ` SET max_retries = 0 WHERE id = ? AND state = 'running'`)
		if _, err := s.db.ExecContext(ctx, zeroQ, id); err != nil {
			return fmt.Errorf("cancel-while-running suppress retry: %w", err)
		}
	}
	return nil
}

func (s *sqlStore) Inspect(ctx context.Context, id string) (Record, error) {
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return Record{}, err
	}
	defer func() { _ = tx.Rollback() }()
	rec, err := s.selectRecordTx(ctx, tx, id)
	if err != nil {
		return Record{}, err
	}
	return rec, nil
}

func (s *sqlStore) QueueStats(ctx context.Context, queue string) (QueueStats, error) {
	if queue == "" {
		queue = "default"
	}
	q := s.rebind(`SELECT state, COUNT(*) FROM ` + s.tasksTable() + ` WHERE queue = ? GROUP BY state`)
	rows, err := s.db.QueryContext(ctx, q, queue)
	if err != nil {
		return QueueStats{}, fmt.Errorf("queue stats: %w", err)
	}
	defer rows.Close()

	var stats QueueStats
	for rows.Next() {
		var state string
		var count int
		if err := rows.Scan(&state, &count); err != nil {
			return QueueStats{}, err
		}
		switch State(state) {
		case StatePending:
			stats.Pending = count
		case StateScheduled:
			stats.Scheduled = count
		case StateRunning:
			stats.Running = count
		case StateFailed:
			stats.Failed = count
		}
	}
	if err := rows.Err(); err != nil {
		return QueueStats{}, err
	}

	// Oldest pending age — separate query, kept simple.
	if stats.Pending > 0 {
		ageQ := s.rebind(`SELECT MIN(enqueued_at) FROM ` + s.tasksTable() + ` WHERE queue = ? AND state = 'pending'`)
		var oldestRaw interface{}
		if err := s.db.QueryRowContext(ctx, ageQ, queue).Scan(&oldestRaw); err == nil {
			if t := s.decodeTime(oldestRaw); !t.IsZero() {
				stats.OldestPendingAge = s.now().Sub(t)
			}
		}
	}
	return stats, nil
}

func (s *sqlStore) PromoteScheduled(ctx context.Context, queue string, now time.Time) (int, error) {
	if queue == "" {
		queue = "default"
	}
	q := s.rebind(`UPDATE ` + s.tasksTable() + `
		SET state = 'pending', enqueued_at = ?
		WHERE queue = ? AND state = 'scheduled' AND delay_until <= ?`)
	res, err := s.db.ExecContext(ctx, q, s.encodeTime(now), queue, s.encodeTime(now))
	if err != nil {
		return 0, fmt.Errorf("promote: %w", err)
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}

func (s *sqlStore) ReclaimExpired(ctx context.Context, queue string, now time.Time) (int, error) {
	if queue == "" {
		queue = "default"
	}
	q := s.rebind(`UPDATE ` + s.tasksTable() + `
		SET state = 'pending', owner_worker = NULL, enqueued_at = ?
		WHERE queue = ? AND state = 'running' AND visibility_at <= ?`)
	res, err := s.db.ExecContext(ctx, q, s.encodeTime(now), queue, s.encodeTime(now))
	if err != nil {
		return 0, fmt.Errorf("reclaim: %w", err)
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}

func (s *sqlStore) PurgeOlderThan(ctx context.Context, cutoff time.Time) (int, error) {
	q := s.rebind(`DELETE FROM ` + s.tasksTable() + `
		WHERE state IN ('completed', 'failed', 'cancelled') AND completed_at IS NOT NULL AND completed_at < ?`)
	res, err := s.db.ExecContext(ctx, q, s.encodeTime(cutoff))
	if err != nil {
		return 0, fmt.Errorf("purge: %w", err)
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}

func (s *sqlStore) Close() error {
	// We don't own the connection — caller does. No-op.
	return nil
}

// === helpers ===

// rebind converts MySQL/SQLite-style "?" placeholders to PostgreSQL's
// $1, $2 form when needed. Statements throughout the file use "?" for
// portability; rebind runs them through dialect-specific replacement.
func (s *sqlStore) rebind(query string) string {
	if s.dialect != dialectPostgres {
		return query
	}
	var b strings.Builder
	b.Grow(len(query))
	idx := 1
	for _, r := range query {
		if r == '?' {
			fmt.Fprintf(&b, "$%d", idx)
			idx++
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

// acquireDedupTx tries to insert (or refresh-after-expiry) a dedup
// row. Returns true if acquired (caller may proceed), false if a
// non-expired entry blocks the key.
func (s *sqlStore) acquireDedupTx(ctx context.Context, tx *sql.Tx, key string, now time.Time) (bool, error) {
	// Clear expired entry if present (so the upsert can win).
	delQ := s.rebind(`DELETE FROM ` + s.dedupTable() + ` WHERE ` + s.identQuote("key") + ` = ? AND expires_at <= ?`)
	if _, err := tx.ExecContext(ctx, delQ, key, s.encodeTime(now)); err != nil {
		return false, fmt.Errorf("dedup gc: %w", err)
	}
	expires := now.Add(s.opts.DedupWindow)

	var insQ string
	switch s.dialect {
	case dialectPostgres:
		insQ = s.rebind(`INSERT INTO ` + s.dedupTable() + ` (` + s.identQuote("key") + `, expires_at) VALUES (?, ?) ON CONFLICT DO NOTHING`)
	case dialectMySQL:
		insQ = `INSERT IGNORE INTO ` + s.dedupTable() + ` (` + s.identQuote("key") + `, expires_at) VALUES (?, ?)`
	default:
		insQ = `INSERT OR IGNORE INTO ` + s.dedupTable() + ` (` + s.identQuote("key") + `, expires_at) VALUES (?, ?)`
	}
	res, err := tx.ExecContext(ctx, insQ, key, s.encodeTime(expires))
	if err != nil {
		return false, fmt.Errorf("dedup acquire: %w", err)
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// identQuote handles MySQL's reserved-word quoting (`key`) vs PG/SQLite ("key").
// MySQL uses backticks; PG/SQLite use double quotes (or no quoting if not reserved).
func (s *sqlStore) identQuote(name string) string {
	switch s.dialect {
	case dialectMySQL:
		return "`" + name + "`"
	default:
		// "key" is a reserved word in SQL standard; quote on PG too.
		return `"` + name + `"`
	}
}

// insertTaskTx inserts a fresh task row inside an open tx.
func (s *sqlStore) insertTaskTx(ctx context.Context, tx *sql.Tx, rec *Record, tagsJSON sql.NullString) error {
	var maxRetries sql.NullInt64
	if rec.Task.MaxRetries != nil {
		maxRetries = sql.NullInt64{Int64: int64(*rec.Task.MaxRetries), Valid: true}
	}
	var uniqKey sql.NullString
	if rec.Task.UniqueKey != "" {
		uniqKey = sql.NullString{String: rec.Task.UniqueKey, Valid: true}
	}

	q := s.rebind(`INSERT INTO ` + s.tasksTable() + ` (
		id, type, payload, queue, priority, state, attempts,
		max_retries, timeout_ns, deadline, delay_until,
		enqueued_at, unique_key, tags
	) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`)
	_, err := tx.ExecContext(ctx, q,
		rec.Task.ID, rec.Task.Type, rec.Task.Payload, rec.Task.Queue,
		rec.Task.Priority, string(rec.State), 0,
		maxRetries, int64(rec.Task.Timeout),
		s.encodeTime(rec.Task.Deadline), s.encodeTime(rec.Task.DelayUntil),
		s.encodeTime(rec.EnqueuedAt), uniqKey, tagsJSON,
	)
	if err != nil {
		return fmt.Errorf("insert: %w", err)
	}
	return nil
}

// encodeTime adapts a time.Time for the active dialect. SQLite's
// time-storage story varies by driver (modernc vs go-sqlite3), so we
// store unix nanoseconds as INT8 explicitly. PG and MySQL handle
// time.Time natively via their drivers.
func (s *sqlStore) encodeTime(t time.Time) interface{} {
	if t.IsZero() {
		return nil
	}
	if s.dialect == dialectSQLite {
		return t.UnixNano()
	}
	return t
}

// decodeTime is the symmetric reader. For SQLite, scans nullable
// integer (unix nanoseconds) into time.Time. For PG/MySQL, scans
// directly into time.Time via sql.NullTime.
func (s *sqlStore) decodeTime(src interface{}) time.Time {
	if src == nil {
		return time.Time{}
	}
	switch v := src.(type) {
	case time.Time:
		return v
	case int64:
		if v == 0 {
			return time.Time{}
		}
		return time.Unix(0, v).UTC()
	case []byte:
		// Some SQLite drivers return TEXT as []byte. Try RFC3339 parse.
		t, err := time.Parse(time.RFC3339Nano, string(v))
		if err != nil {
			return time.Time{}
		}
		return t
	case string:
		t, err := time.Parse(time.RFC3339Nano, v)
		if err != nil {
			return time.Time{}
		}
		return t
	}
	return time.Time{}
}

// selectRecordTx loads a single Record by ID inside a tx (or from
// db.QueryRow if tx is the read-only reader). Returns ErrNotFound when
// the row doesn't exist.
func (s *sqlStore) selectRecordTx(ctx context.Context, tx *sql.Tx, id string) (Record, error) {
	q := s.rebind(`SELECT
		id, type, payload, queue, priority, state, attempts,
		max_retries, timeout_ns, deadline, delay_until,
		enqueued_at, started_at, completed_at, last_error,
		next_attempt, owner_worker, visibility_at, unique_key, tags
		FROM ` + s.tasksTable() + ` WHERE id = ?`)
	row := tx.QueryRowContext(ctx, q, id)
	return s.scanRecord(row)
}

func (s *sqlStore) taskExists(ctx context.Context, id string) (bool, error) {
	q := s.rebind(`SELECT 1 FROM ` + s.tasksTable() + ` WHERE id = ? LIMIT 1`)
	var x int
	err := s.db.QueryRowContext(ctx, q, id).Scan(&x)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

// classifyOwnershipMissTx maps a "0 rows affected" outcome to either
// ErrNotFound or ErrNotOwner so callers can react correctly. Cheap —
// runs only when ownership ops fail.
func (s *sqlStore) classifyOwnershipMissTx(ctx context.Context, id string) error {
	exists, err := s.taskExists(ctx, id)
	if err != nil {
		return err
	}
	if !exists {
		return ErrNotFound
	}
	return ErrNotOwner
}

// scanRecord materializes a Record from a row scan. Time columns are
// scanned into interface{} and decoded via s.decodeTime so the same
// code path works for SQLite (int64 unix-nanos) and PG/MySQL
// (driver-native time.Time).
func (s *sqlStore) scanRecord(row *sql.Row) (Record, error) {
	var (
		rec        Record
		state      string
		payload    []byte
		maxRetries sql.NullInt64
		timeoutNs  int64
		deadline, delayUntil, enqueued, started, completed, nextAttempt, visibility interface{}
		lastErr, owner, uniqKey, tagsJSON                                           sql.NullString
	)
	err := row.Scan(
		&rec.Task.ID, &rec.Task.Type, &payload, &rec.Task.Queue, &rec.Task.Priority,
		&state, &rec.Attempts, &maxRetries, &timeoutNs, &deadline, &delayUntil,
		&enqueued, &started, &completed, &lastErr, &nextAttempt, &owner,
		&visibility, &uniqKey, &tagsJSON,
	)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Record{}, ErrNotFound
		}
		return Record{}, fmt.Errorf("scan: %w", err)
	}
	rec.Task.Payload = payload
	rec.State = State(state)
	if maxRetries.Valid {
		v := int(maxRetries.Int64)
		rec.Task.MaxRetries = &v
	}
	rec.Task.Timeout = time.Duration(timeoutNs)
	rec.Task.Deadline = s.decodeTime(deadline)
	rec.Task.DelayUntil = s.decodeTime(delayUntil)
	rec.EnqueuedAt = s.decodeTime(enqueued)
	rec.StartedAt = s.decodeTime(started)
	rec.CompletedAt = s.decodeTime(completed)
	rec.NextAttempt = s.decodeTime(nextAttempt)
	rec.VisibilityAt = s.decodeTime(visibility)
	if lastErr.Valid {
		rec.LastError = lastErr.String
	}
	if owner.Valid {
		rec.OwnerWorker = owner.String
	}
	if uniqKey.Valid {
		rec.Task.UniqueKey = uniqKey.String
	}
	if tagsJSON.Valid && tagsJSON.String != "" {
		_ = json.Unmarshal([]byte(tagsJSON.String), &rec.Task.Tags)
	}
	return rec, nil
}

func encodeTags(tags map[string]string) (sql.NullString, error) {
	if len(tags) == 0 {
		return sql.NullString{}, nil
	}
	b, err := json.Marshal(tags)
	if err != nil {
		return sql.NullString{}, fmt.Errorf("encode tags: %w", err)
	}
	return sql.NullString{String: string(b), Valid: true}, nil
}

// firstLine returns the first non-empty trimmed line of s — used in
// error messages so a 30-line CREATE TABLE doesn't dump into the
// error string.
func firstLine(s string) string {
	for _, line := range strings.Split(s, "\n") {
		line = strings.TrimSpace(line)
		if line != "" {
			return line
		}
	}
	return s
}
