//go:build redis_integration

package cache

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	rds "github.com/redis/go-redis/v9"

	dockerprov "mkfst/providers/docker"
)

// TestRedisCacheConformance runs the conformance suite against a
// real Redis instance spun up via mkfst's docker provider. Same
// gate pattern as providers/tasks's redis_test.go.
//
//	go test -tags redis_integration ./providers/cache/
func TestRedisCacheConformance(t *testing.T) {
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

	containerName := fmt.Sprintf("mkfst-cache-redis-%d", time.Now().UnixNano())
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
	client := waitForRedisReady(t, addr, 30*time.Second)
	t.Cleanup(func() { _ = client.Close() })

	runCacheConformance(t, "redis", func(t *testing.T) Cache {
		// Per-subtest key prefix so subtests don't see each other.
		prefix := "mkc_" + strings.ReplaceAll(strings.ReplaceAll(t.Name(), "/", "_"), "-", "_") + ":"
		c, err := NewRedisCache(RedisOpts{Client: client, KeyPrefix: prefix})
		if err != nil {
			t.Fatalf("NewRedisCache: %v", err)
		}
		t.Cleanup(func() {
			_, _ = c.DeletePrefix(context.Background(), "")
		})
		return c
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
