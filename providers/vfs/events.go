package vfs

import (
	"strings"
	"sync"
	"time"
)

// ChangeOp discriminates the kind of mutation that produced an event.
type ChangeOp uint8

const (
	// OpWrite is a file create-or-overwrite (Tree.Write, Tree.WriteReader).
	OpWrite ChangeOp = iota + 1
	// OpMkdir is a directory creation (Tree.Mkdir, Tree.MkdirAll — only the
	// final directory created, not its parents).
	OpMkdir
	// OpRemove is an entry deletion (Tree.Remove, Tree.RemoveAll). For
	// RemoveAll the event is on the root path; subscribers walking the
	// subtree see the descendants disappear via subsequent stat calls.
	OpRemove
	// OpRename describes a rename. Path is the new path; OldPath holds the
	// previous path. Subscribers should treat as Remove(OldPath) +
	// Write(Path) for invalidation purposes.
	OpRename
	// OpSymlink is a symlink creation (Tree.Symlink). LinkTarget on the
	// event holds the target.
	OpSymlink
	// OpChmod is a permission change.
	OpChmod
	// OpChtime is an mtime change.
	OpChtime
)

// String renders the op as a short token for logs/debugging.
func (o ChangeOp) String() string {
	switch o {
	case OpWrite:
		return "write"
	case OpMkdir:
		return "mkdir"
	case OpRemove:
		return "remove"
	case OpRename:
		return "rename"
	case OpSymlink:
		return "symlink"
	case OpChmod:
		return "chmod"
	case OpChtime:
		return "chtime"
	default:
		return "unknown"
	}
}

// ChangeEvent describes a single mutation observed on the tree. Path is
// always populated; other fields are op-specific.
//
// Events do NOT carry full content — subscribers that need the new bytes
// re-read from the tree. This keeps the publish path cheap (no buffer
// copies) and avoids the writer paying for serialization on the hot path.
type ChangeEvent struct {
	Path       string
	Op         ChangeOp
	ModTime    time.Time
	OldPath    string // OpRename only
	LinkTarget string // OpSymlink only
	// Coalesced is set when this event represents one or more dropped
	// events the subscriber didn't drain in time. Path will be the
	// nearest common ancestor of the dropped events, signaling
	// "something under here changed, walk it." Op will be OpWrite as a
	// generic re-scan trigger.
	Coalesced bool
}

// SubscribeOpts configures a Subscribe call.
type SubscribeOpts struct {
	// PathPrefix limits delivered events to those at or beneath the
	// prefix. Empty matches everything. The match is purely by string —
	// "/foo" matches "/foo", "/foo/x", "/foo/x/y" but not "/foobar".
	PathPrefix string
	// Buffer is the channel buffer depth. The publish path drops events
	// (and emits a Coalesced marker) when the buffer is full, so a slow
	// subscriber never stalls writers. Default 64.
	Buffer int
}

// subscriber is the per-subscription state held by the Tree.
type subscriber struct {
	id      uint64
	prefix  string
	ch      chan ChangeEvent
	pending chan struct{} // non-blocking signal for "drop happened, emit coalesced"
	// closed is set under Tree.subsMu by unsub. publish reads it under
	// subsMu.RLock to decide whether to send. The mutex coordination
	// ensures we never race a publish-side send against an unsub-side
	// close of sub.ch.
	closed     bool
	overflowMu sync.Mutex
	overflowed bool
	// coalesceRoot is the deepest common-ancestor path of dropped
	// events. Updated under overflowMu; emitted as Path on the
	// coalesced event when the buffer drains.
	coalesceRoot string
}

// Subscribe registers a change-event consumer. The returned channel
// receives ChangeEvents until Unsubscribe is called or the tree's
// reference is dropped.
//
// Backpressure: if the subscriber falls behind (buffer full), the tree
// publishes a coalesced event marker INSTEAD of blocking on send. The
// coalesced marker carries the nearest common ancestor of dropped
// events, telling the subscriber "rescan under here." This is the right
// trade for a sync engine: never stall writers, occasionally pay a
// coarser re-scan.
func (t *Tree) Subscribe(opts SubscribeOpts) (<-chan ChangeEvent, func()) {
	buf := opts.Buffer
	if buf <= 0 {
		buf = 64
	}
	sub := &subscriber{
		id:      t.subAlloc.Add(1),
		prefix:  cleanPrefix(opts.PathPrefix),
		ch:      make(chan ChangeEvent, buf),
		pending: make(chan struct{}, 1),
	}

	t.subsMu.Lock()
	if t.subscribers == nil {
		t.subscribers = make(map[uint64]*subscriber)
	}
	t.subscribers[sub.id] = sub
	t.subsMu.Unlock()

	// Coalesce-emitter goroutine: when overflow flag is set and buffer
	// drains far enough, send a single Coalesced event. Exits on
	// channel close.
	go t.coalesceLoop(sub)

	// unsub is idempotent — guarded by sub.closed. Mutex coordination:
	// taking subsMu.Lock while publish holds RLock means publish will
	// finish its current sub.ch <- ev before we set closed and close
	// the channel. After we return, no goroutine will send to sub.ch.
	var unsubOnce sync.Once
	unsub := func() {
		unsubOnce.Do(func() {
			t.subsMu.Lock()
			sub.closed = true
			delete(t.subscribers, sub.id)
			t.subsMu.Unlock()
			// Close pending so coalesceLoop exits its `for range`.
			close(sub.pending)
			// Close sub.ch so consumers reading it see EOF. Must
			// happen AFTER subsMu unlock so any in-flight publish has
			// drained, AND after pending close so coalesceLoop won't
			// try to send a final coalesced event into a closed ch.
			close(sub.ch)
		})
	}
	return sub.ch, unsub
}

// publish broadcasts ev to every matching subscriber. Called from inside
// the tree mutex by every mutating op. Must be very cheap — does not
// block on subscriber sends; on full buffer, sets the overflow flag for
// the coalesce-emitter to handle.
//
// The subsMu.RLock barrier coordinates with unsub: while we hold the
// read lock here, no unsub can take the write lock, so sub.ch can't be
// closed under us. Once unsub does run, sub.closed flips before the
// channel close — subsequent publishes skip the closed sub entirely.
func (t *Tree) publish(ev ChangeEvent) {
	t.subsMu.RLock()
	defer t.subsMu.RUnlock()
	for _, sub := range t.subscribers {
		if sub.closed {
			continue
		}
		if sub.prefix != "" && !pathMatchesPrefix(ev.Path, sub.prefix) {
			continue
		}
		select {
		case sub.ch <- ev:
		default:
			// Buffer full — record drop, let coalesceLoop emit a marker.
			sub.markOverflow(ev.Path)
		}
	}
}

func (s *subscriber) markOverflow(droppedPath string) {
	s.overflowMu.Lock()
	if !s.overflowed {
		s.overflowed = true
		s.coalesceRoot = droppedPath
	} else {
		s.coalesceRoot = commonPathPrefix(s.coalesceRoot, droppedPath)
	}
	s.overflowMu.Unlock()
	// Non-blocking nudge to the coalesce loop.
	select {
	case s.pending <- struct{}{}:
	default:
	}
}

// coalesceLoop emits a single Coalesced event whenever the subscriber
// has dropped events and there's room in the channel. Runs per-
// subscription; exits cleanly when sub.pending is closed by unsub.
//
// Concurrency: takes subsMu.RLock around the send so unsub can't close
// sub.ch from under us. unsub closes pending FIRST (which terminates
// our `for range`), then takes subsMu.Lock (waits for our RLock to
// drop), then closes ch. By that point we're already out of the loop
// and not sending, so no panic risk.
func (t *Tree) coalesceLoop(sub *subscriber) {
	for range sub.pending {
		// Wait briefly to let the consumer drain — gives them a chance
		// to absorb the in-flight events before we synthesize another.
		// Empirically: 1ms is short enough to feel "live" and long
		// enough to coalesce bursts of writes.
		time.Sleep(1 * time.Millisecond)

		sub.overflowMu.Lock()
		if !sub.overflowed {
			sub.overflowMu.Unlock()
			continue
		}
		root := sub.coalesceRoot
		sub.overflowed = false
		sub.coalesceRoot = ""
		sub.overflowMu.Unlock()

		ev := ChangeEvent{
			Path:      root,
			Op:        OpWrite, // generic "re-scan" hint
			ModTime:   time.Now().UTC(),
			Coalesced: true,
		}

		// Coordinate the send with unsub via subsMu. If sub.closed is
		// already set, unsub has won the race — skip and exit on the
		// next pending-channel read (which will be the close signal).
		t.subsMu.RLock()
		if sub.closed {
			t.subsMu.RUnlock()
			continue
		}
		select {
		case sub.ch <- ev:
		default:
			// Still full; mark overflow again so the next `pending`
			// signal triggers another retry.
			sub.markOverflow(root)
		}
		t.subsMu.RUnlock()
	}
}

// === path helpers used by publish ===

func cleanPrefix(p string) string {
	p = strings.TrimSpace(p)
	if p == "" || p == "/" {
		return ""
	}
	if !strings.HasPrefix(p, "/") {
		p = "/" + p
	}
	return strings.TrimRight(p, "/")
}

func pathMatchesPrefix(path, prefix string) bool {
	if prefix == "" {
		return true
	}
	if path == prefix {
		return true
	}
	return strings.HasPrefix(path, prefix+"/")
}

// commonPathPrefix returns the deepest path that is an ancestor of both a
// and b (or "/" if they share no ancestor below root). Used to coalesce
// dropped events into a single re-scan hint covering all of them.
func commonPathPrefix(a, b string) string {
	if a == "" {
		return b
	}
	if b == "" {
		return a
	}
	aParts := strings.Split(strings.Trim(a, "/"), "/")
	bParts := strings.Split(strings.Trim(b, "/"), "/")
	min := len(aParts)
	if len(bParts) < min {
		min = len(bParts)
	}
	out := []string{}
	for i := 0; i < min; i++ {
		if aParts[i] != bParts[i] {
			break
		}
		out = append(out, aParts[i])
	}
	if len(out) == 0 {
		return "/"
	}
	return "/" + strings.Join(out, "/")
}
