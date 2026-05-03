//go:build redis_integration

package tasks

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	rds "github.com/redis/go-redis/v9"

	dockerprov "mkfst/providers/docker"
)

// TestRedisStoreConformance runs the conformance suite against a real
// Redis instance spun up via mkfst's docker provider. Gated behind
// the `redis_integration` build tag.
//
// Run via:
//   go test -tags redis_integration ./providers/tasks/
func TestRedisStoreConformance(t *testing.T) {
	docker, err := dockerprov.New(dockerprov.Opts{Timeout: 5 * time.Second})
	if err != nil {
		t.Skipf("docker daemon unreachable: %v", err)
	}
	t.Cleanup(func() { _ = docker.Close() })

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	if _, err := docker.SDK().ImageInspect(ctx, "redis:7-alpine"); err != nil {
		ev, perr := docker.Pull(ctx, "redis:7-alpine")
		if perr != nil {
			t.Skipf("redis pull failed: %v", perr)
		}
		if _, derr := dockerprov.DrainEvents(ev); derr != nil {
			t.Skipf("redis pull stream: %v", derr)
		}
	}

	containerName := fmt.Sprintf("mkfst-tasks-redis-%d", time.Now().UnixNano())
	res, err := docker.Run(ctx, "redis:7-alpine",
		dockerprov.Name(containerName),
		dockerprov.Port(dockerprov.PortMap{ContainerPort: 6379}),
		dockerprov.AutoRemove(),
		dockerprov.Detach(),
	)
	if err != nil {
		t.Fatalf("Run redis: %v", err)
	}
	t.Cleanup(func() {
		stopT := 5 * time.Second
		_ = docker.Stop(context.Background(), res.ContainerID, &stopT)
	})

	hostPort := waitForRedisPort(t, docker, res.ContainerID)
	addr := fmt.Sprintf("127.0.0.1:%d", hostPort)

	// Wait for redis to actually accept connections.
	client := waitForRedisReady(t, addr, 30*time.Second)
	t.Cleanup(func() { _ = client.Close() })

	runStoreConformance(t, "redis", func(t *testing.T) Store {
		// Per-subtest key prefix so subtests don't bleed into each
		// other (we share one redis instance across the full
		// conformance run).
		prefix := "mkt_" + strings.ReplaceAll(strings.ReplaceAll(t.Name(), "/", "_"), "-", "_") + ":"
		store, err := NewRedisStore(RedisOpts{
			Client:      client,
			KeyPrefix:   prefix,
			DedupWindow: time.Minute,
		})
		if err != nil {
			t.Fatalf("NewRedisStore: %v", err)
		}
		t.Cleanup(func() {
			// Drop every key under the prefix.
			iter := client.Scan(context.Background(), 0, prefix+"*", 256).Iterator()
			batch := []string{}
			for iter.Next(context.Background()) {
				batch = append(batch, iter.Val())
				if len(batch) >= 256 {
					_ = client.Del(context.Background(), batch...).Err()
					batch = batch[:0]
				}
			}
			if len(batch) > 0 {
				_ = client.Del(context.Background(), batch...).Err()
			}
		})
		return store
	})
}

func waitForRedisPort(t *testing.T, c *dockerprov.Client, containerID string) int {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		info, err := c.Inspect(ctx, containerID)
		cancel()
		if err == nil && info.NetworkSettings != nil {
			for portKey, bindings := range info.NetworkSettings.Ports {
				if portKey.Port() == "6379" && len(bindings) > 0 {
					var p int
					fmt.Sscanf(bindings[0].HostPort, "%d", &p)
					if p > 0 {
						return p
					}
				}
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("redis container never published port 6379")
	return 0
}

func waitForRedisReady(t *testing.T, addr string, timeout time.Duration) *rds.Client {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		c := rds.NewClient(&rds.Options{Addr: addr})
		ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
		err := c.Ping(ctx).Err()
		cancel()
		if err == nil {
			return c
		}
		_ = c.Close()
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatalf("redis never became ready at %s", addr)
	return nil
}
