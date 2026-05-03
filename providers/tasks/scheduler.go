package tasks

import (
	"context"
	"errors"
	"time"
)

// NewScheduler returns a Scheduler implementation backed by the given
// Store. The scheduler is stateless — it just delegates to the store
// — but having it as a separate type lets callers pass around an
// "enqueue handle" without exposing the full Store surface (Claim,
// Heartbeat, etc. are worker-side concerns the user shouldn't touch).
//
// Optionally pass a Telemetry to record per-enqueue metrics and
// inject the current trace context into Task.Tags so downstream
// claim+execute spans chain through.
func NewScheduler(s Store, telemetry ...*Telemetry) Scheduler {
	var t *Telemetry
	if len(telemetry) > 0 {
		t = telemetry[0]
	}
	return &scheduler{store: s, t: t}
}

type scheduler struct {
	store Store
	t     *Telemetry
}

func (s *scheduler) Enqueue(ctx context.Context, t Task) (Record, error) {
	if s.store == nil {
		return Record{}, errors.New("tasks.Scheduler: nil store")
	}
	// Inject the current span context into Task.Tags so the
	// downstream claim+execute spans chain through. No-op when
	// there's no active trace.
	InjectTraceContext(ctx, &t)
	rec, err := s.store.Enqueue(ctx, t)
	if err == nil {
		s.t.recordEnqueue(ctx, rec.Task)
	}
	return rec, err
}

func (s *scheduler) EnqueueIn(ctx context.Context, delay time.Duration, t Task) (Record, error) {
	t.DelayUntil = time.Now().Add(delay)
	return s.Enqueue(ctx, t)
}

func (s *scheduler) EnqueueAt(ctx context.Context, when time.Time, t Task) (Record, error) {
	t.DelayUntil = when
	return s.Enqueue(ctx, t)
}

func (s *scheduler) Cancel(ctx context.Context, id string) error {
	rec, _ := s.store.Inspect(ctx, id)
	err := s.store.Cancel(ctx, id)
	if err == nil {
		s.t.recordCancel(ctx, rec.Task.Queue)
	}
	return err
}

func (s *scheduler) Inspect(ctx context.Context, id string) (Record, error) {
	return s.store.Inspect(ctx, id)
}
