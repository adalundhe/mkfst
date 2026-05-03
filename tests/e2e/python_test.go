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

// TestPythonAppBuildAndRun: tiny Python script in VFS, build a python:3-
// alpine image, run it, verify stdout. Covers the "interpreted language"
// scenario where we don't compile but still want to test the build →
// run → logs flow.
func TestPythonAppBuildAndRun(t *testing.T) {
	c := requireDaemon(t)
	requireInternet(t)

	tree := vfs.NewTree(vfs.TreeOpts{})

	const app = `import sys
print(f"hello from python {sys.version_info.major}.{sys.version_info.minor}")
`
	const dockerfile = `# syntax=docker/dockerfile:1
FROM python:3-alpine
WORKDIR /app
COPY app.py ./
ENTRYPOINT ["python", "/app/app.py"]
`
	_ = tree.Write("/app.py", []byte(app), 0o644)
	_ = tree.Write("/Dockerfile", []byte(dockerfile), 0o644)

	imageTag := uniqueName("mkfst-e2e-py")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	events, err := c.Build(ctx, docker.NewVFSSource(tree),
		docker.Tag(imageTag),
	)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	imageID, _ := drainBuildEvents(t, events)
	withCleanupImage(t, c, imageID)

	exitCode, stdout, _ := runWaitAndCollect(t, c, imageTag)
	if exitCode != 0 {
		t.Fatalf("python script exited %d", exitCode)
	}
	if !strings.Contains(stdout, "hello from python 3.") {
		t.Fatalf("stdout: %q", stdout)
	}
}

// TestPythonAppWithRequirementsInstall covers the "needs to install a
// real dependency" path. We use a small pure-Python wheel (requests is
// the canonical example but it's heavy; tabulate is ~30KB and trivially
// importable). Catches errors in the build context that involve a
// non-trivial RUN step.
func TestPythonAppWithRequirementsInstall(t *testing.T) {
	c := requireDaemon(t)
	requireInternet(t)

	tree := vfs.NewTree(vfs.TreeOpts{})

	const app = `import tabulate
data = [["alice", 30], ["bob", 25]]
print(tabulate.tabulate(data, headers=["name", "age"]))
`
	const requirements = `tabulate==0.9.0
`
	const dockerfile = `# syntax=docker/dockerfile:1
FROM python:3-alpine
WORKDIR /app
COPY requirements.txt ./
RUN pip install --no-cache-dir -r requirements.txt
COPY app.py ./
ENTRYPOINT ["python", "/app/app.py"]
`
	_ = tree.Write("/app.py", []byte(app), 0o644)
	_ = tree.Write("/requirements.txt", []byte(requirements), 0o644)
	_ = tree.Write("/Dockerfile", []byte(dockerfile), 0o644)

	imageTag := uniqueName("mkfst-e2e-py-deps")
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	defer cancel()

	events, err := c.Build(ctx, docker.NewVFSSource(tree),
		docker.Tag(imageTag),
	)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	imageID, _ := drainBuildEvents(t, events)
	withCleanupImage(t, c, imageID)

	_, stdout, _ := runWaitAndCollect(t, c, imageTag)
	// tabulate's default format produces lines like "name      age"; check
	// for the header presence rather than exact whitespace which varies.
	if !strings.Contains(stdout, "name") || !strings.Contains(stdout, "alice") {
		t.Fatalf("unexpected stdout: %q", stdout)
	}
}
