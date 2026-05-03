package files

import (
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"mkfst/providers/vfs"
)

func TestDownloadWritesIntoVFS(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("hello world"))
	}))
	defer srv.Close()

	tree := vfs.NewTree(vfs.TreeOpts{})
	files := NewService(tree)

	if err := files.Download(context.Background(), srv.URL, "/inbox/greeting.txt"); err != nil {
		t.Fatalf("Download: %v", err)
	}
	got, err := tree.Read("/inbox/greeting.txt")
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if string(got) != "hello world" {
		t.Fatalf("got %q", got)
	}
}

func TestDownloadHeaderAndBearer(t *testing.T) {
	var receivedAuth, receivedX string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedAuth = r.Header.Get("Authorization")
		receivedX = r.Header.Get("X-Custom")
		_, _ = w.Write([]byte("ok"))
	}))
	defer srv.Close()

	files := NewService(vfs.NewTree(vfs.TreeOpts{}))
	err := files.Download(context.Background(), srv.URL, "/x.txt",
		Bearer("tok-123"),
		Header("X-Custom", "yes"),
	)
	if err != nil {
		t.Fatalf("Download: %v", err)
	}
	if receivedAuth != "Bearer tok-123" {
		t.Fatalf("Authorization header: %q", receivedAuth)
	}
	if receivedX != "yes" {
		t.Fatalf("X-Custom header: %q", receivedX)
	}
}

func TestDownloadVerifyChecksum(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("abc"))
	}))
	defer srv.Close()

	tree := vfs.NewTree(vfs.TreeOpts{})
	files := NewService(tree)

	// SHA-256 of "abc"
	const sum = "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad"

	if err := files.Download(context.Background(), srv.URL, "/match.txt", VerifySHA256(sum)); err != nil {
		t.Fatalf("verify pass: %v", err)
	}
	err := files.Download(context.Background(), srv.URL, "/mismatch.txt",
		VerifySHA256("0000000000000000000000000000000000000000000000000000000000000000"),
	)
	if !errors.Is(err, ErrChecksumMismatch) {
		t.Fatalf("verify fail: want ErrChecksumMismatch, got %v", err)
	}
	// Mismatch must NOT have written the file.
	if _, err := tree.Stat("/mismatch.txt"); !errors.Is(err, vfs.ErrNotExist) {
		t.Fatalf("mismatch file should not be written, stat err=%v", err)
	}
}

func TestDownloadAutoDecompressGzip(t *testing.T) {
	body := strings.Repeat("foo ", 100)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Encoding", "gzip")
		gw := gzip.NewWriter(w)
		_, _ = gw.Write([]byte(body))
		_ = gw.Close()
	}))
	defer srv.Close()

	tree := vfs.NewTree(vfs.TreeOpts{})
	files := NewService(tree)

	if err := files.Download(context.Background(), srv.URL, "/x.txt", AutoDecompress()); err != nil {
		t.Fatalf("Download: %v", err)
	}
	got, _ := tree.Read("/x.txt")
	if string(got) != body {
		t.Fatalf("decompressed body mismatch")
	}
}

func TestDownloadRetriesOnTransientFailure(t *testing.T) {
	var attempts int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&attempts, 1)
		if n < 3 {
			http.Error(w, "transient", http.StatusInternalServerError)
			return
		}
		_, _ = w.Write([]byte("eventual success"))
	}))
	defer srv.Close()

	files := NewService(vfs.NewTree(vfs.TreeOpts{}))
	err := files.Download(context.Background(), srv.URL, "/x.txt", RetryUpTo(5))
	if err != nil {
		t.Fatalf("Download: %v", err)
	}
	if got := atomic.LoadInt32(&attempts); got != 3 {
		t.Fatalf("expected 3 attempts, got %d", got)
	}
}

func TestDownloadProgressCallback(t *testing.T) {
	body := strings.Repeat("x", 200_000) // > one chunk
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", fmt.Sprintf("%d", len(body)))
		_, _ = io.WriteString(w, body)
	}))
	defer srv.Close()

	files := NewService(vfs.NewTree(vfs.TreeOpts{}))
	var lastCurrent int64
	var lastTotal int64
	var ticks int
	err := files.Download(context.Background(), srv.URL, "/x.txt", OnProgress(func(p DownloadProgress) {
		ticks++
		lastCurrent = p.Current
		lastTotal = p.Total
	}))
	if err != nil {
		t.Fatalf("Download: %v", err)
	}
	if ticks < 2 {
		t.Fatalf("expected multiple progress ticks, got %d", ticks)
	}
	if lastCurrent != int64(len(body)) {
		t.Fatalf("final current: got %d want %d", lastCurrent, len(body))
	}
	if lastTotal != int64(len(body)) {
		t.Fatalf("Total: got %d want %d", lastTotal, len(body))
	}
}

func TestDownloadAllParallel(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Tiny artificial delay to force parallel scheduling to matter.
		time.Sleep(20 * time.Millisecond)
		_, _ = io.WriteString(w, r.URL.Path)
	}))
	defer srv.Close()

	tree := vfs.NewTree(vfs.TreeOpts{})
	files := NewService(tree, ConcurrencyLimit(8))

	jobs := make([]Job, 16)
	for i := range jobs {
		jobs[i] = Job{
			URL: fmt.Sprintf("%s/job-%d", srv.URL, i),
			Dst: fmt.Sprintf("/jobs/job-%d.txt", i),
		}
	}

	start := time.Now()
	results := files.DownloadAll(context.Background(), jobs)
	elapsed := time.Since(start)

	if len(results) != len(jobs) {
		t.Fatalf("results len: got %d want %d", len(results), len(jobs))
	}
	for i, r := range results {
		if r.Err != nil {
			t.Fatalf("job %d: %v", i, r.Err)
		}
		got, err := tree.Read(r.Job.Dst)
		if err != nil {
			t.Fatalf("read %s: %v", r.Job.Dst, err)
		}
		if string(got) != "/job-"+fmt.Sprint(i) {
			t.Fatalf("job %d body: got %q", i, got)
		}
	}
	// 16 jobs * 20ms each = 320ms serial; with concurrency 8 we should
	// see <100ms in the happy path. Allow generous margin for CI slowness.
	if elapsed > 200*time.Millisecond {
		t.Logf("warning: parallel downloads took %s — concurrency may not be effective", elapsed)
	}
}

func TestDownloadCancellation(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Block until client disconnects.
		<-r.Context().Done()
	}))
	defer srv.Close()

	files := NewService(vfs.NewTree(vfs.TreeOpts{}))
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()

	err := files.Download(ctx, srv.URL, "/x.txt")
	if err == nil {
		t.Fatalf("expected cancellation error")
	}
}
