//go:build e2e

package e2e

import (
	"context"
	"strings"
	"testing"
	"time"

	"mkfst/providers/docker"
)

// pullCases is the set of images we want to confirm we can pull. Kept
// small (alpine + busybox) to keep the test bandwidth-friendly. The
// language-stack images (python, node, golang) are pulled on-demand by
// the build tests, so we don't double-pay here.
var pullCases = []struct {
	ref     string
	maxSize int64 // sanity cap; 0 disables
}{
	{ref: "alpine:3.19"},
	{ref: "busybox:1.36"},
}

func TestPullSmallImages(t *testing.T) {
	c := requireDaemon(t)
	requireInternet(t)

	for _, tc := range pullCases {
		tc := tc
		t.Run(tc.ref, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
			defer cancel()

			events, err := c.Pull(ctx, tc.ref)
			if err != nil {
				t.Fatalf("Pull: %v", err)
			}

			var statusCount, progressCount int
			for ev := range events {
				switch ev.Kind {
				case docker.EventStatus:
					statusCount++
				case docker.EventProgress:
					progressCount++
				case docker.EventError:
					t.Fatalf("pull error: %v", ev.Err)
				}
			}
			t.Logf("%s: status=%d progress=%d", tc.ref, statusCount, progressCount)

			// Inspect to confirm the image is now locally present.
			inspectCtx, inspectCancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer inspectCancel()
			img, _, err := c.SDK().ImageInspectWithRaw(inspectCtx, tc.ref)
			if err != nil {
				t.Fatalf("post-pull inspect %s: %v", tc.ref, err)
			}
			if img.ID == "" {
				t.Fatalf("inspect returned empty ID for %s", tc.ref)
			}
			if tc.maxSize > 0 && img.Size > tc.maxSize {
				t.Logf("warning: %s is larger than expected (%d > %d)", tc.ref, img.Size, tc.maxSize)
			}
		})
	}
}

func TestPullPlatformPin(t *testing.T) {
	c := requireDaemon(t)
	requireInternet(t)

	// Pin amd64 explicitly. On amd64 hosts this is the natural manifest;
	// on arm64 hosts it forces an emulated pull. Either way, the image
	// should land locally with the requested architecture.
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	events, err := c.Pull(ctx, "alpine:3.19", docker.PullPlatform("linux/amd64"))
	if err != nil {
		t.Fatalf("Pull: %v", err)
	}
	if err := drainPullEvents(events); err != nil {
		t.Fatalf("pull stream: %v", err)
	}

	inspectCtx, inspectCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer inspectCancel()
	img, _, err := c.SDK().ImageInspectWithRaw(inspectCtx, "alpine:3.19")
	if err != nil {
		t.Fatalf("inspect: %v", err)
	}
	if !strings.EqualFold(img.Architecture, "amd64") {
		t.Fatalf("architecture mismatch: got %q, want amd64", img.Architecture)
	}
}

func TestPullUnknownTagErrors(t *testing.T) {
	c := requireDaemon(t)
	requireInternet(t)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	events, err := c.Pull(ctx, "alpine:does-not-exist-9999")
	if err != nil {
		// Some daemon versions surface "manifest unknown" before opening
		// the stream; that's fine, that's the error we wanted.
		return
	}
	if drainErr := drainPullEvents(events); drainErr == nil {
		t.Fatalf("expected error pulling nonexistent tag")
	}
}
