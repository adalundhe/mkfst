package workflows

import (
	"encoding/json"
	"fmt"
	"time"
)

// === instance state ===
//
// State is encoded as JSON in the cache so multiple engine processes
// can read each other's writes. JSON over msgpack/protobuf for one
// reason: it survives schema evolution gracefully and the cache
// values are tiny (kilobytes per instance, not megabytes), so the
// overhead is invisible.
//
// Key scheme inside the cache (all under the configured KeyPrefix):
//
//   <prefix>inst:<instanceID>:meta              → instanceMeta JSON
//   <prefix>inst:<instanceID>:node:<node>:state → nodeState JSON
//   <prefix>inst:<instanceID>:node:<node>:out   → raw output bytes
//
// Cleanup at terminal state uses cache.DeletePrefix("<prefix>inst:<id>:")
// in one call, so the schema is intentionally flat under each instance.

// InstanceState enumerates the lifecycle states of a workflow instance.
type InstanceState string

const (
	// InstancePending: created, no nodes started yet.
	InstancePending InstanceState = "pending"
	// InstanceRunning: at least one node has been enqueued.
	InstanceRunning InstanceState = "running"
	// InstanceCompleted: every leaf reached StateCompleted (or
	// FailContinue swallowed the failure).
	InstanceCompleted InstanceState = "completed"
	// InstanceFailed: at least one node failed under FailHaltWorkflow
	// or FailSkipDownstream.
	InstanceFailed InstanceState = "failed"
	// InstanceCancelled: explicitly cancelled before completion.
	InstanceCancelled InstanceState = "cancelled"
)

// NodeState enumerates the per-node states inside an instance.
type NodeState string

const (
	// NodePending: parents not all complete yet; not enqueued.
	NodePending NodeState = "pending"
	// NodeEnqueued: dispatched to providers/tasks; awaiting a worker.
	NodeEnqueued NodeState = "enqueued"
	// NodeCompleted: handler returned nil; output stored.
	NodeCompleted NodeState = "completed"
	// NodeFailed: handler returned an error and retries are exhausted.
	NodeFailed NodeState = "failed"
	// NodeSkipped: a parent failed under FailSkipDownstream so this
	// node will not run.
	NodeSkipped NodeState = "skipped"
)

// Terminal reports whether a node state precludes further work.
func (s NodeState) Terminal() bool {
	return s == NodeCompleted || s == NodeFailed || s == NodeSkipped
}

// instanceMeta is the per-instance header. Stored at
// <prefix>inst:<id>:meta as JSON. Lock-free read-modify-write isn't
// possible across processes, so the engine treats updates to this
// blob as last-writer-wins; the source of truth for "is the
// instance done" is derived by aggregating node states each tick,
// not by trusting the meta blob.
type instanceMeta struct {
	ID         string            `json:"id"`
	Definition string            `json:"definition"`
	State      InstanceState     `json:"state"`
	StartedAt  time.Time         `json:"started_at"`
	EndedAt    time.Time         `json:"ended_at,omitempty"`
	Tags       map[string]string `json:"tags,omitempty"`
}

// nodeStateRecord is the per-node bookkeeping. Stored at
// <prefix>inst:<id>:node:<name>:state as JSON.
type nodeStateRecord struct {
	State     NodeState `json:"state"`
	TaskID    string    `json:"task_id,omitempty"`
	Attempts  int       `json:"attempts,omitempty"`
	StartedAt time.Time `json:"started_at,omitempty"`
	EndedAt   time.Time `json:"ended_at,omitempty"`
	LastError string    `json:"last_error,omitempty"`
}

// === key helpers ===

func instanceMetaKey(prefix, id string) string {
	return prefix + "inst:" + id + ":meta"
}

func nodeStateKey(prefix, id, node string) string {
	return prefix + "inst:" + id + ":node:" + node + ":state"
}

func nodeOutputKey(prefix, id, node string) string {
	return prefix + "inst:" + id + ":node:" + node + ":out"
}

func instancePrefix(prefix, id string) string {
	return prefix + "inst:" + id + ":"
}

// === JSON marshaling ===
//
// Encoded with stdlib encoding/json. The blobs are tiny (bytes to a
// couple of hundred bytes) so the perf cost is irrelevant; using
// stdlib means we don't drag in another JSON library just for state.

func encodeInstanceMeta(m instanceMeta) ([]byte, error) {
	b, err := json.Marshal(m)
	if err != nil {
		return nil, fmt.Errorf("workflows: encode instance meta: %w", err)
	}
	return b, nil
}

func decodeInstanceMeta(b []byte) (instanceMeta, error) {
	var m instanceMeta
	if err := json.Unmarshal(b, &m); err != nil {
		return instanceMeta{}, fmt.Errorf("workflows: decode instance meta: %w", err)
	}
	return m, nil
}

func encodeNodeState(n nodeStateRecord) ([]byte, error) {
	b, err := json.Marshal(n)
	if err != nil {
		return nil, fmt.Errorf("workflows: encode node state: %w", err)
	}
	return b, nil
}

func decodeNodeState(b []byte) (nodeStateRecord, error) {
	var n nodeStateRecord
	if err := json.Unmarshal(b, &n); err != nil {
		return nodeStateRecord{}, fmt.Errorf("workflows: decode node state: %w", err)
	}
	return n, nil
}
