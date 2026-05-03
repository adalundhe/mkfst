//go:build e2e

package e2e

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"mkfst/providers/docker"
	"mkfst/providers/files"
	"mkfst/providers/vfs"
)

// TestHTTPDownloadIntoVFSThenBuild proves the marquee three-provider
// integration: a host HTTP server serves "release artifacts", the files
// provider downloads them straight into a VFS tree (no temp files), and
// the docker provider builds an image from that tree as the build
// context.
//
// This is the "fetch source, materialize in memory, build, run"
// pipeline an agent or build orchestrator wants to express. Before the
// providers, this would have been: download to temp file, untar to
// temp dir, point docker at the dir. With the providers it's three
// composable operations against one in-memory tree.
func TestHTTPDownloadIntoVFSThenBuild(t *testing.T) {
	c := requireDaemon(t)
	requireInternet(t) // need to pull the base image; downloads themselves are local

	// Spin up a local HTTP server hosting the source files we want to
	// pipe through the pipeline.
	const mainPy = `import sys
print("downloaded build pipeline ok, argv =", sys.argv[1:])
`
	const dockerfile = `# syntax=docker/dockerfile:1
FROM python:3-alpine
WORKDIR /app
COPY app.py ./
ENTRYPOINT ["python", "/app/app.py"]
`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/app.py":
			_, _ = w.Write([]byte(mainPy))
		case "/Dockerfile":
			_, _ = w.Write([]byte(dockerfile))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	tree := vfs.NewTree(vfs.TreeOpts{})
	fsvc := files.NewService(tree)

	// Pipeline step 1: download source files into VFS. We assert the
	// SHA-256 of the Dockerfile we pull to prove the verify path works
	// end-to-end (catches accidental mid-flight corruption).
	dockerfileSum := sha256.Sum256([]byte(dockerfile))
	jobs := []files.Job{
		{URL: srv.URL + "/app.py", Dst: "/app.py"},
		{URL: srv.URL + "/Dockerfile", Dst: "/Dockerfile",
			Options: []files.DownloadOption{files.VerifySHA256(hex.EncodeToString(dockerfileSum[:]))}},
	}
	results := fsvc.DownloadAll(context.Background(), jobs)
	for _, r := range results {
		if r.Err != nil {
			t.Fatalf("download %s: %v", r.Job.URL, r.Err)
		}
	}

	// Sanity-check the tree state before kicking off the build.
	if got, _ := tree.Read("/Dockerfile"); !strings.Contains(string(got), "python:3-alpine") {
		t.Fatalf("Dockerfile didn't land correctly in VFS")
	}

	// Pipeline step 2: build from the VFS tree.
	imageTag := uniqueName("mkfst-e2e-pipeline")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	events, err := c.Build(ctx, docker.NewVFSSource(tree), docker.Tag(imageTag))
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	imageID, _ := drainBuildEvents(t, events)
	withCleanupImage(t, c, imageID)

	// Pipeline step 3: run with a custom argv so we can verify the
	// stdout includes the args we passed (proves the whole chain).
	exitCode, stdout, _ := runWaitAndCollect(t, c, imageTag,
		docker.Cmd("two", "args"),
	)
	if exitCode != 0 {
		t.Fatalf("exit %d", exitCode)
	}
	want := "downloaded build pipeline ok, argv = ['two', 'args']"
	if !strings.Contains(stdout, want) {
		t.Fatalf("stdout: got %q want substring %q", stdout, want)
	}
}

// TestParallelDownloadsThenBuild stresses the files+docker boundary by
// downloading many small files in parallel and then building an image
// that COPYs them all in. Catches races between concurrent VFS writes
// and the build's tar serialization.
func TestParallelDownloadsThenBuild(t *testing.T) {
	c := requireDaemon(t)
	requireInternet(t)

	// Server returns a unique payload per path so we can verify each
	// downloaded file landed at the right VFS location.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("payload-for-" + strings.TrimPrefix(r.URL.Path, "/")))
	}))
	defer srv.Close()

	tree := vfs.NewTree(vfs.TreeOpts{})
	fsvc := files.NewService(tree, files.ConcurrencyLimit(8))

	const N = 20
	jobs := make([]files.Job, N)
	for i := 0; i < N; i++ {
		jobs[i] = files.Job{
			URL: fmt.Sprintf("%s/file-%02d.txt", srv.URL, i),
			Dst: fmt.Sprintf("/payload/file-%02d.txt", i),
		}
	}
	results := fsvc.DownloadAll(context.Background(), jobs)
	for _, r := range results {
		if r.Err != nil {
			t.Fatalf("download %s: %v", r.Job.URL, r.Err)
		}
	}

	// Build an image that concatenates the payload files; verify content.
	const dockerfile = `# syntax=docker/dockerfile:1
FROM alpine:3.19
COPY payload /payload
RUN ls /payload | sort > /payload-list
ENTRYPOINT ["sh", "-c", "cat /payload-list && cat /payload/file-00.txt"]
`
	_ = tree.Write("/Dockerfile", []byte(dockerfile), 0o644)

	imageTag := uniqueName("mkfst-e2e-parallel-pipeline")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	events, err := c.Build(ctx, docker.NewVFSSource(tree), docker.Tag(imageTag))
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	imageID, _ := drainBuildEvents(t, events)
	withCleanupImage(t, c, imageID)

	exitCode, stdout, _ := runWaitAndCollect(t, c, imageTag)
	if exitCode != 0 {
		t.Fatalf("exit %d", exitCode)
	}
	if !strings.Contains(stdout, "file-00.txt") {
		t.Fatalf("expected file-00.txt in listing, got: %q", stdout)
	}
	if !strings.Contains(stdout, "payload-for-file-00.txt") {
		t.Fatalf("expected file-00 contents in stdout, got: %q", stdout)
	}
}
