package runtime

import (
	"context"
	"errors"
	"sync"
)

// === Runtime pool ===
//
// A bounded pool of pre-warmed Runtime instances, each pre-loaded
// with the same bundle. Borrows are blocking: when the pool is
// fully checked out, the next borrow waits until a Runtime is
// returned.
//
// Why a pool: each Runtime serializes calls internally (QuickJS is
// not thread-safe). Without a pool, every workflow-node invocation
// queues behind every other for the same workflow → no parallelism
// across nodes. A pool of N runtimes lets up to N nodes execute
// concurrently.
//
// Memory cost: each runtime carries the WASM instance plus the
// evaluated bundle's JS heap. ~10–20 MB per runtime in practice.
// Right size: Min(workflow's max-fan-out, EngineOpts.JSWorkers).

// PoolOpts configures NewPool.
type PoolOpts struct {
	// Size is the number of pre-warmed runtimes. Required.
	Size int
	// EngineOpts.HostBridge is shared across all runtimes; the
	// per-call bridge dispatcher is installed once per runtime
	// at warm time via Init.
	EngineOpts RuntimeOpts
	// Engine is the factory; nil = NewEngine().
	Engine Engine
	// Init runs against each fresh runtime at pool warm time.
	// Typical use: load the bundle JS, install host functions,
	// pre-evaluate startup code. Init runs serially across
	// runtimes; if it returns an error, the pool's NewPool fails.
	Init func(ctx context.Context, rt Runtime) error
}

// Pool is a bounded pool of identical, pre-warmed Runtime instances.
type Pool struct {
	rts  chan Runtime
	all  []Runtime
	once sync.Once

	mu     sync.Mutex
	closed bool
}

// NewPool builds + warms a pool of size runtimes.
func NewPool(ctx context.Context, opts PoolOpts) (*Pool, error) {
	if opts.Size <= 0 {
		return nil, errors.New("ts.runtime.NewPool: Size must be > 0")
	}
	eng := opts.Engine
	if eng == nil {
		eng = NewEngine()
	}
	p := &Pool{
		rts: make(chan Runtime, opts.Size),
		all: make([]Runtime, 0, opts.Size),
	}
	for i := 0; i < opts.Size; i++ {
		rt, err := eng.NewRuntime(ctx, opts.EngineOpts)
		if err != nil {
			_ = p.Close(ctx)
			return nil, err
		}
		if opts.Init != nil {
			if err := opts.Init(ctx, rt); err != nil {
				_ = rt.Close(ctx)
				_ = p.Close(ctx)
				return nil, err
			}
		}
		p.all = append(p.all, rt)
		p.rts <- rt
	}
	return p, nil
}

// Borrow returns a Runtime from the pool, blocking until one is
// available or ctx expires. Caller MUST call Return when done.
// Returns ErrPoolClosed when the pool has been Close()d.
func (p *Pool) Borrow(ctx context.Context) (Runtime, error) {
	p.mu.Lock()
	closed := p.closed
	p.mu.Unlock()
	if closed {
		return nil, ErrPoolClosed
	}
	select {
	case rt, ok := <-p.rts:
		if !ok {
			return nil, ErrPoolClosed
		}
		return rt, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// Return puts a Runtime back into the pool. Idempotent at the
// pool level (the underlying chan rejects double-returns via
// blocking; we use a non-blocking send to surface mistakes).
func (p *Pool) Return(rt Runtime) {
	p.mu.Lock()
	closed := p.closed
	p.mu.Unlock()
	if closed {
		return
	}
	select {
	case p.rts <- rt:
	default:
		// Pool is somehow over capacity (caller bug — Returning
		// a runtime that wasn't Borrowed). Drop the runtime
		// rather than block; it'll be GC'd.
	}
}

// With borrows a runtime, runs fn against it, returns it. The
// canonical convenience wrapper.
func (p *Pool) With(ctx context.Context, fn func(rt Runtime) error) error {
	rt, err := p.Borrow(ctx)
	if err != nil {
		return err
	}
	defer p.Return(rt)
	return fn(rt)
}

// Size returns the configured pool size.
func (p *Pool) Size() int { return cap(p.rts) }

// Close closes every Runtime in the pool. Idempotent. Subsequent
// Borrow calls return ErrPoolClosed.
func (p *Pool) Close(ctx context.Context) error {
	var closeErr error
	p.once.Do(func() {
		p.mu.Lock()
		p.closed = true
		p.mu.Unlock()
		// Close the in-channel so blocked Borrow calls receive
		// the zero-value-not-ok signal and return ErrPoolClosed.
		close(p.rts)
		for _, rt := range p.all {
			if err := rt.Close(ctx); err != nil && closeErr == nil {
				closeErr = err
			}
		}
	})
	return closeErr
}

// ErrPoolClosed is returned by Borrow after Close.
var ErrPoolClosed = errors.New("ts.runtime.Pool: closed")
