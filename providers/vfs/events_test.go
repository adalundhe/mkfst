package vfs

import (
	"runtime"
	"testing"
	"time"
)

// TestSubscribeUnsubReleasesGoroutines proves that Subscribe+Unsubscribe
// cycles don't accumulate goroutines (the coalesce-loop leak we found
// during the goroutine audit).
func TestSubscribeUnsubReleasesGoroutines(t *testing.T) {
	tree := NewTree(TreeOpts{})

	// Baseline: take a goroutine count after a short settle.
	runtime.GC()
	time.Sleep(50 * time.Millisecond)
	baseline := runtime.NumGoroutine()

	// Subscribe + immediately unsubscribe many times. Before the fix,
	// each cycle leaked one goroutine (the coalesceLoop) because
	// sub.pending was never closed.
	const N = 500
	for i := 0; i < N; i++ {
		_, unsub := tree.Subscribe(SubscribeOpts{Buffer: 4})
		unsub()
	}

	// Allow goroutines to wind down.
	runtime.GC()
	time.Sleep(100 * time.Millisecond)
	after := runtime.NumGoroutine()

	// We tolerate a small slack for runtime jitter (Go's GC can
	// transiently spawn helpers). Anything ≥ N would mean a leak.
	if delta := after - baseline; delta > 10 {
		t.Fatalf("Subscribe+Unsub of %d cycles leaked %d goroutines (baseline=%d, after=%d)",
			N, delta, baseline, after)
	}
}

// TestSubscribeRaceUnsubAndPublish stresses the closed-channel race we
// guarded against: simultaneous publish() and unsub() must never panic.
func TestSubscribeRaceUnsubAndPublish(t *testing.T) {
	tree := NewTree(TreeOpts{})
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("panic during race: %v", r)
		}
	}()

	for i := 0; i < 50; i++ {
		ch, unsub := tree.Subscribe(SubscribeOpts{Buffer: 4})
		// Drain in a goroutine.
		drainDone := make(chan struct{})
		go func() {
			for range ch {
			}
			close(drainDone)
		}()
		// Burst writes from another goroutine.
		writeDone := make(chan struct{})
		go func() {
			for j := 0; j < 100; j++ {
				_ = tree.Write("/race-"+itoa(j)+".txt", []byte("x"), 0o644)
			}
			close(writeDone)
		}()
		// Unsubscribe immediately (racing with both goroutines).
		unsub()
		<-writeDone
		<-drainDone
	}
}

func TestSubscribeReceivesWriteEvents(t *testing.T) {
	tree := NewTree(TreeOpts{})
	ch, unsub := tree.Subscribe(SubscribeOpts{})
	defer unsub()

	if err := tree.Write("/foo.txt", []byte("hi"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	select {
	case ev := <-ch:
		if ev.Op != OpWrite {
			t.Fatalf("op: got %v", ev.Op)
		}
		if ev.Path != "/foo.txt" {
			t.Fatalf("path: got %q", ev.Path)
		}
	case <-time.After(time.Second):
		t.Fatalf("no event received")
	}
}

func TestSubscribePrefixFilter(t *testing.T) {
	tree := NewTree(TreeOpts{})
	ch, unsub := tree.Subscribe(SubscribeOpts{PathPrefix: "/wanted", Buffer: 16})
	defer unsub()

	_ = tree.Write("/wanted/a.txt", []byte("a"), 0o644)
	_ = tree.Write("/other/b.txt", []byte("b"), 0o644)
	_ = tree.Write("/wanted/c.txt", []byte("c"), 0o644)

	deadline := time.After(300 * time.Millisecond)
	got := []string{}
loop:
	for {
		select {
		case ev := <-ch:
			got = append(got, ev.Path)
		case <-deadline:
			break loop
		}
	}
	// Expect at minimum the two file writes; possibly also the
	// auto-created parent dir. Critically: NO event from /other/...
	for _, p := range got {
		if p != "/wanted" && p != "/wanted/a.txt" && p != "/wanted/c.txt" {
			t.Fatalf("unexpected event path under /wanted filter: %q (full: %v)", p, got)
		}
	}
	sawA, sawC := false, false
	for _, p := range got {
		if p == "/wanted/a.txt" {
			sawA = true
		}
		if p == "/wanted/c.txt" {
			sawC = true
		}
	}
	if !sawA || !sawC {
		t.Fatalf("missing expected file events: a=%v c=%v (got %v)", sawA, sawC, got)
	}
}

func TestSubscribeOverflowCoalesces(t *testing.T) {
	tree := NewTree(TreeOpts{})
	ch, unsub := tree.Subscribe(SubscribeOpts{Buffer: 2})
	defer unsub()

	// Burst-write 50 files. Subscriber buffer is 2; rest must coalesce.
	for i := 0; i < 50; i++ {
		_ = tree.Write("/files/f"+itoa(i)+".txt", []byte("x"), 0o644)
	}

	// Read everything that arrives within 200ms.
	got := []ChangeEvent{}
	deadline := time.After(200 * time.Millisecond)
loop:
	for {
		select {
		case ev := <-ch:
			got = append(got, ev)
		case <-deadline:
			break loop
		}
	}

	// We should see at most 2 buffered + at least 1 coalesced marker,
	// but in practice the coalesce loop sleeps 1ms then fires once,
	// then reschedules — so the count varies. Key invariant: a
	// Coalesced event appears, AND the producer didn't stall.
	sawCoalesce := false
	for _, ev := range got {
		if ev.Coalesced {
			sawCoalesce = true
			if ev.Path == "" {
				t.Fatalf("coalesce event missing path: %+v", ev)
			}
		}
	}
	if !sawCoalesce {
		t.Fatalf("expected at least one coalesced event in %d delivered", len(got))
	}
}

func TestSubscribeEmitsRemoveAndMkdir(t *testing.T) {
	tree := NewTree(TreeOpts{})
	ch, unsub := tree.Subscribe(SubscribeOpts{Buffer: 16})
	defer unsub()

	_ = tree.Mkdir("/d", 0o755)
	_ = tree.Write("/d/file.txt", []byte("x"), 0o644)
	_ = tree.Remove("/d/file.txt")
	_ = tree.Remove("/d")

	got := map[ChangeOp]int{}
	deadline := time.After(500 * time.Millisecond)
loop:
	for {
		select {
		case ev := <-ch:
			got[ev.Op]++
		case <-deadline:
			break loop
		}
	}
	if got[OpMkdir] < 1 {
		t.Fatalf("expected ≥1 mkdir, got %v", got)
	}
	if got[OpWrite] < 1 {
		t.Fatalf("expected ≥1 write, got %v", got)
	}
	if got[OpRemove] < 2 {
		t.Fatalf("expected ≥2 remove, got %v", got)
	}
}

// itoa: tiny helper to avoid importing strconv in a test file.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	digits := []byte{}
	for n > 0 {
		digits = append([]byte{byte('0' + n%10)}, digits...)
		n /= 10
	}
	return string(digits)
}
