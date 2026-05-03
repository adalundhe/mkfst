//go:build e2e

package e2e

import (
	"context"
	"strings"
	"testing"
	"time"

	"mkfst/providers/docker"
	"mkfst/providers/vfs"
)

// TestGoAppBuildAndRun exercises the full happy path for a compiled
// language: write a tiny Go program + Dockerfile entirely to VFS, build
// a multi-stage image (golang:1.22-alpine builder → scratch runner),
// run it, capture logs, verify output.
//
// The interesting bit is that the entire build context is in-memory.
// No tar files on disk, no host directories. The daemon receives the
// VFS-streamed tar and produces an image.
func TestGoAppBuildAndRun(t *testing.T) {
	c := requireDaemon(t)
	requireInternet(t)

	tree := vfs.NewTree(vfs.TreeOpts{})

	const main = `package main

import "fmt"

func main() {
	fmt.Println("hello from in-memory go build")
}
`
	const goMod = `module hello

go 1.22
`
	const dockerfile = `# syntax=docker/dockerfile:1
FROM golang:1.22-alpine AS builder
WORKDIR /src
COPY go.mod ./
COPY main.go ./
RUN go build -trimpath -ldflags="-s -w" -o /out/hello ./

FROM scratch
COPY --from=builder /out/hello /hello
ENTRYPOINT ["/hello"]
`
	if err := tree.Write("/main.go", []byte(main), 0o644); err != nil {
		t.Fatalf("write main.go: %v", err)
	}
	if err := tree.Write("/go.mod", []byte(goMod), 0o644); err != nil {
		t.Fatalf("write go.mod: %v", err)
	}
	if err := tree.Write("/Dockerfile", []byte(dockerfile), 0o644); err != nil {
		t.Fatalf("write Dockerfile: %v", err)
	}

	imageTag := uniqueName("mkfst-e2e-go")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	events, err := c.Build(ctx, docker.NewVFSSource(tree),
		docker.Tag(imageTag),
		docker.Pull(),
		docker.NoCache(),
	)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	imageID, _ := drainBuildEvents(t, events)
	withCleanupImage(t, c, imageID)

	exitCode, stdout, _ := runWaitAndCollect(t, c, imageTag)
	if exitCode != 0 {
		t.Fatalf("container exited %d (stdout=%q)", exitCode, stdout)
	}
	if !strings.Contains(stdout, "hello from in-memory go build") {
		t.Fatalf("unexpected stdout: %q", stdout)
	}
}

// TestGoAppMultiBuildArg verifies build-arg propagation by stamping the
// binary with a value passed via docker.Arg(). Sanity-checks that our
// Arg() option actually reaches the daemon's --build-arg path.
func TestGoAppMultiBuildArg(t *testing.T) {
	c := requireDaemon(t)
	requireInternet(t)

	tree := vfs.NewTree(vfs.TreeOpts{})

	const main = `package main

import (
	"fmt"
	"os"
)

func main() {
	fmt.Println("VERSION=" + os.Getenv("BUILD_VERSION"))
}
`
	const dockerfile = `# syntax=docker/dockerfile:1
FROM golang:1.22-alpine AS builder
ARG BUILD_VERSION=unset
WORKDIR /src
COPY main.go ./
RUN echo "module v" > go.mod && go build -o /out/v ./

FROM alpine:3.19
ARG BUILD_VERSION=unset
ENV BUILD_VERSION=${BUILD_VERSION}
COPY --from=builder /out/v /v
ENTRYPOINT ["/v"]
`
	_ = tree.Write("/main.go", []byte(main), 0o644)
	_ = tree.Write("/Dockerfile", []byte(dockerfile), 0o644)

	imageTag := uniqueName("mkfst-e2e-go-arg")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	events, err := c.Build(ctx, docker.NewVFSSource(tree),
		docker.Tag(imageTag),
		docker.Arg("BUILD_VERSION", "v1.2.3-from-test"),
	)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	imageID, _ := drainBuildEvents(t, events)
	withCleanupImage(t, c, imageID)

	_, stdout, _ := runWaitAndCollect(t, c, imageTag)
	if !strings.Contains(stdout, "VERSION=v1.2.3-from-test") {
		t.Fatalf("build arg didn't reach binary: %q", stdout)
	}
}
