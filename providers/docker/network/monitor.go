package network

import (
	"sync"
	"sync/atomic"
	"time"
)

// === monitor ===
//
// Monitor is the per-stack event stream. It is intentionally lossy:
// when the buffered channel fills, new events are dropped (counted)
// rather than blocking the gateway hot path. This is the right
// trade-off when the consumer isn't keeping up — we'd rather drop
// observability than throttle real traffic.
//
// Ordering: events are emitted from a single emitter goroutine per
// stack. Producers call emit, which sends to a serialization
// channel; the emitter sequences events with monotonic Seq numbers
// and forwards to the user-facing channel. This guarantees that an
// event with Seq=N+1 was produced after the event with Seq=N.

// EventKind enumerates the event types.
type EventKind int

const (
	EventConnectionAccepted EventKind = iota + 1
	EventConnectionDenied
	EventConnectionClosed
	EventProbeFailed
	EventServiceRestarted
	EventStackUp
	EventStackDown
	// EventInternalError is for non-connection-related background
	// errors that the stack handled but operators should see —
	// errgroup failures, hook errors during teardown,
	// best-effort cleanup failures, etc.
	EventInternalError
)

// Event is one observed thing.
type Event struct {
	Seq         uint64
	Kind        EventKind
	At          time.Time
	Service     string
	Replica     int
	ContainerID string
	IngressName string
	SourceAddr  string
	BytesIn     uint64
	BytesOut    uint64
	Duration    time.Duration
	Error       string
	DenyReason  string
}

// Monitor is the per-stack event hub. Subscribers consume from
// Events(); the channel buffer size is configured via
// EngineOpts.MonitorBuffer.
type Monitor struct {
	stackID   string
	stackName string

	in  chan Event // producer-side: emit() sends here
	out chan Event // consumer-side: Events() returns this

	dropped atomic.Uint64
	seq     atomic.Uint64

	stopOnce sync.Once
	stopped  atomic.Bool
	doneCh   chan struct{} // closed when serialize exits
}

func newMonitor(stackID, stackName string, buffer int) *Monitor {
	if buffer <= 0 {
		buffer = 1024
	}
	m := &Monitor{
		stackID:   stackID,
		stackName: stackName,
		in:        make(chan Event, buffer),
		out:       make(chan Event, buffer),
		doneCh:    make(chan struct{}),
	}
	go m.serialize()
	return m
}

// Events returns the receive-only stream. Always the same channel
// for a given Monitor.
func (m *Monitor) Events() <-chan Event { return m.out }

// Dropped returns the number of events dropped because the
// downstream channel was full.
func (m *Monitor) Dropped() uint64 { return m.dropped.Load() }

// emit sends an event from a producer goroutine. Non-blocking on
// the in channel: if even the serialization queue is full, drop
// and bump the counter. The serializer never blocks the consumer
// either (drops at the out channel too).
func (m *Monitor) emit(e Event) {
	if m.stopped.Load() {
		return
	}
	select {
	case m.in <- e:
	default:
		m.dropped.Add(1)
	}
}

// serialize sequences events and forwards. Single goroutine =
// total ordering across producers; closing the in channel ends the
// loop and closes out and signals doneCh.
func (m *Monitor) serialize() {
	defer close(m.doneCh)
	for e := range m.in {
		e.Seq = m.seq.Add(1)
		select {
		case m.out <- e:
		default:
			m.dropped.Add(1)
		}
	}
	close(m.out)
}

// stop signals the emitter to drain and close. Blocks until the
// serializer goroutine has exited. Idempotent.
func (m *Monitor) stop() {
	m.stopOnce.Do(func() {
		m.stopped.Store(true)
		close(m.in)
	})
	<-m.doneCh
}
