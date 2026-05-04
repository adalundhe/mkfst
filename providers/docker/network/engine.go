package network

import (
	"context"
	"errors"
	"sync"

	"github.com/docker/docker/client"

	"mkfst/providers/policy"
)

// === engine ===

// Engine is the network module's per-process owner. It holds the
// docker client, the stack registry, the shared probe scheduler,
// and process-wide configuration.
//
// One Engine per mkfst process is the norm. Construct via NewEngine.
type Engine struct {
	cli  *client.Client
	opts EngineOpts

	// stacks is the live registry: stackID → *Stack. sync.Map for
	// lock-free reads at scale; writes only happen on
	// NewStack/RemoveStack which are infrequent.
	stacks sync.Map // stackID → *Stack

	probeSched *probeScheduler

	closeOnce sync.Once
	closeErr  error
}

// EngineOpts configures NewEngine. The optional Checker is consulted
// at every privileged stack operation (Up/Down/RunOneShot/Exec).
// Nil = no policy enforcement (defaults to permissive).
type EngineOpts struct {
	// EngineID identifies this engine instance for resource
	// labeling. Default: a fresh random hex ID. Stable IDs are
	// useful for crash recovery — set this to your process's
	// persistent identifier so a restarted process can adopt its
	// own prior resources.
	EngineID string

	// MonitorBuffer sets the size of each Stack.Monitor channel.
	// Default 1024. Larger = less drop under bursty traffic but
	// more memory.
	MonitorBuffer int

	// ProbeWorkers caps the number of concurrent probe-execution
	// goroutines across all stacks. Default 256. The min-heap
	// scheduler dispatches probes into a bounded channel
	// consumed by this pool.
	ProbeWorkers int

	// ProbeScheduleResolution is the tick granularity of the
	// scheduler's heap. Default 25ms. Lower = tighter probe
	// timing but more wakeups.
	ProbeScheduleResolution interface{} // time.Duration; interface to allow zero-value detection in tests

	// Policy gates privileged stack operations. nil = pass-through.
	Policy policy.Checker
}

// NewEngine constructs an Engine bound to the docker client.
func NewEngine(cli *client.Client, opts EngineOpts) (*Engine, error) {
	if cli == nil {
		return nil, errors.New("network.NewEngine: nil docker client")
	}
	if opts.EngineID == "" {
		id, err := newID()
		if err != nil {
			return nil, err
		}
		opts.EngineID = id
	}
	if opts.MonitorBuffer <= 0 {
		opts.MonitorBuffer = 1024
	}
	if opts.ProbeWorkers <= 0 {
		opts.ProbeWorkers = 256
	}
	if opts.Policy == nil {
		opts.Policy = policy.AllowAllChecker{}
	}
	e := &Engine{cli: cli, opts: opts}
	e.probeSched = newProbeScheduler(e, opts.ProbeWorkers)
	go e.probeSched.run()
	return e, nil
}

// EngineID returns the configured / generated engine ID.
func (e *Engine) EngineID() string { return e.opts.EngineID }

// NewStack creates a Stack handle (no docker resources are touched
// until Up). name is the human-readable identifier; the underlying
// stackID is generated automatically (use AdoptStack to pick up an
// existing stack by ID).
func (e *Engine) NewStack(name string) (*Stack, error) {
	if name == "" {
		return nil, errors.New("network.Engine.NewStack: name required")
	}
	id, err := newID()
	if err != nil {
		return nil, err
	}
	s := newStack(e, id, name)
	e.stacks.Store(id, s)
	return s, nil
}

// Stacks returns a snapshot list of every registered stack.
func (e *Engine) Stacks() []*Stack {
	out := []*Stack{}
	e.stacks.Range(func(_, v interface{}) bool {
		out = append(out, v.(*Stack))
		return true
	})
	return out
}

// Close stops the probe scheduler and releases engine-level
// resources. Does NOT bring down stacks — call Stack.Down on each
// before Close, or accept that orphaned containers will be visible
// to a future engine via crash recovery.
func (e *Engine) Close(ctx context.Context) error {
	e.closeOnce.Do(func() {
		e.probeSched.stop()
	})
	return e.closeErr
}
