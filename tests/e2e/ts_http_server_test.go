//go:build e2e

package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"mkfst/providers/cache"
	"mkfst/providers/tasks"
	"mkfst/providers/ts"
	"mkfst/providers/ts/bundle"
	tsserver "mkfst/providers/ts/server"
	"mkfst/providers/workflows"
)

// TestTSServer_SubmitRunInspect runs the full HTTP-server path:
// stand up the mkfst server in-process, POST a TS workflow,
// trigger /run, poll /inspect until terminal, assert state.
func TestTSServer_SubmitRunInspect(t *testing.T) {
	ctx := context.Background()

	store := tasks.NewMemoryStore(tasks.MemoryOpts{})
	t.Cleanup(func() { _ = store.Close() })
	worker, _ := tasks.NewWorker(tasks.WorkerOpts{
		Store: store, Concurrency: 4,
		PollInterval: 5 * time.Millisecond, MaintenanceInterval: 10 * time.Millisecond,
	})
	wfEng, _ := workflows.NewEngine(workflows.EngineOpts{
		Scheduler: tasks.NewScheduler(store),
		Worker:    worker,
		Outputs:   cache.NewMemoryCache(cache.MemoryOpts{MaxBytes: 1 << 20}),
	})

	al := bundle.NewAllowlist(tsSDKPath(t))
	_ = al.Add(bundle.ModuleEntry{
		Name: "@mkfst/sdk",
		Path: filepath.Join(tsSDKPath(t), "mkfst-sdk"),
	})
	tsEng, _ := ts.NewEngine(ts.EngineOpts{
		WorkflowEngine: wfEng,
		Allowlist:      al,
		EmitSourceMaps: true,
	})

	runCtx, cancelRun := context.WithCancel(ctx)
	defer cancelRun()
	doneCh := make(chan error, 1)
	go func() { doneCh <- worker.Run(runCtx) }()

	// Bring up the HTTP server on an ephemeral port.
	srv := tsserver.NewServer(tsEng)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	httpSrv := &http.Server{Handler: srv.Routes()}
	go func() { _ = httpSrv.Serve(ln) }()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = httpSrv.Shutdown(ctx)
	})
	base := "http://" + ln.Addr().String()

	// healthz
	resp, err := http.Get(base + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 200 {
		t.Fatalf("healthz status %d", resp.StatusCode)
	}
	resp.Body.Close()

	// 1. submit
	src := []byte(`
import { defineTask, defineDAG } from "@mkfst/sdk";

const httpEcho = defineTask({
  name: "httpEcho",
  run: async () => "via-http",
});

export default defineDAG("httpFlow", (b) => {
  b.add(httpEcho);
});
`)
	resp, err = http.Post(base+"/v1/workflows?name=httpFlow", "application/typescript", bytes.NewReader(src))
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode >= 300 {
		t.Fatalf("submit status %d body=%s", resp.StatusCode, body)
	}
	var submitView struct {
		Name   string `json:"name"`
		SHA256 string `json:"sha256"`
		Tasks  int    `json:"tasks"`
	}
	if err := json.Unmarshal(body, &submitView); err != nil {
		t.Fatal(err)
	}
	if submitView.Name != "httpFlow" {
		t.Fatalf("unexpected name %q", submitView.Name)
	}
	if submitView.Tasks != 1 {
		t.Fatalf("expected 1 task got %d", submitView.Tasks)
	}
	if submitView.SHA256 == "" {
		t.Fatal("missing sha256")
	}

	// 2. run
	resp, err = http.Post(base+"/v1/workflows/httpFlow/run", "application/octet-stream", nil)
	if err != nil {
		t.Fatal(err)
	}
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode >= 300 {
		t.Fatalf("run status %d body=%s", resp.StatusCode, body)
	}
	var runView struct {
		InstanceID string `json:"instanceId"`
	}
	if err := json.Unmarshal(body, &runView); err != nil {
		t.Fatal(err)
	}
	if runView.InstanceID == "" {
		t.Fatal("missing instance id")
	}

	// 3. inspect (poll until terminal).
	deadline := time.Now().Add(15 * time.Second)
	var lastBody string
	for time.Now().Before(deadline) {
		resp, err := http.Get(base + "/v1/instances/" + runView.InstanceID)
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		lastBody = string(b)
		if strings.Contains(lastBody, `"state":"completed"`) ||
			strings.Contains(lastBody, `"state":"failed"`) {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if !strings.Contains(lastBody, `"state":"completed"`) {
		t.Fatalf("instance did not complete: %s", lastBody)
	}
	cancelRun()
	<-doneCh
}

// TestTSServer_SubmitRejectedNoSDK proves the allowlist enforces
// at the HTTP layer too — submission with an unknown import is
// rejected with a structured error.
func TestTSServer_SubmitRejectedNoSDK(t *testing.T) {
	ctx := context.Background()
	store := tasks.NewMemoryStore(tasks.MemoryOpts{})
	t.Cleanup(func() { _ = store.Close() })
	worker, _ := tasks.NewWorker(tasks.WorkerOpts{
		Store: store, Concurrency: 4,
	})
	wfEng, _ := workflows.NewEngine(workflows.EngineOpts{
		Scheduler: tasks.NewScheduler(store),
		Worker:    worker,
	})
	al := bundle.NewAllowlist(tsSDKPath(t))
	// Intentionally omit @mkfst/sdk from the allow list.
	tsEng, _ := ts.NewEngine(ts.EngineOpts{
		WorkflowEngine: wfEng,
		Allowlist:      al,
	})
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() { _ = worker.Run(runCtx) }()

	srv := tsserver.NewServer(tsEng)
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	httpSrv := &http.Server{Handler: srv.Routes()}
	go func() { _ = httpSrv.Serve(ln) }()
	t.Cleanup(func() { _ = httpSrv.Shutdown(context.Background()) })

	src := []byte(`import { defineDAG } from "@mkfst/sdk"; export default defineDAG("x", b => {});`)
	resp, _ := http.Post("http://"+ln.Addr().String()+"/v1/workflows", "application/typescript", bytes.NewReader(src))
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("expected 422, got %d body=%s", resp.StatusCode, body)
	}
	if !strings.Contains(string(body), "allowlist") {
		t.Fatalf("expected 'allowlist' in error, got %s", body)
	}
}
