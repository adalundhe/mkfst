//go:build sql_integration

package cache

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"

	mkfstdb "mkfst/db"
	dockerprov "mkfst/providers/docker"
)

// TestPostgresCacheConformance runs the cache conformance suite
// against a real Postgres instance spun up via the docker provider.
//
//	go test -tags sql_integration ./providers/cache/
func TestPostgresCacheConformance(t *testing.T) {
	docker, err := dockerprov.New(dockerprov.Opts{Timeout: 5 * time.Second})
	if err != nil {
		t.Skipf("docker daemon unreachable: %v", err)
	}
	t.Cleanup(func() { _ = docker.Close() })

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	if _, err := docker.SDK().ImageInspect(ctx, "postgres:16-alpine"); err != nil {
		ev, perr := docker.Pull(ctx, "postgres:16-alpine")
		if perr != nil {
			t.Skipf("postgres pull failed: %v", perr)
		}
		if _, derr := dockerprov.DrainEvents(ev); derr != nil {
			t.Skipf("postgres pull stream: %v", derr)
		}
	}

	containerName := fmt.Sprintf("mkfst-cache-pg-%d", time.Now().UnixNano())
	res, err := docker.Run(ctx, "postgres:16-alpine",
		dockerprov.Name(containerName),
		dockerprov.Env("POSTGRES_PASSWORD", "test"),
		dockerprov.Env("POSTGRES_DB", "cache"),
		dockerprov.Cmd("postgres", "-c", "fsync=off", "-c", "synchronous_commit=off", "-c", "full_page_writes=off"),
		dockerprov.Port(dockerprov.PortMap{ContainerPort: 5432}),
		dockerprov.AutoRemove(),
		dockerprov.Detach(),
	)
	if err != nil {
		t.Fatalf("Run postgres: %v", err)
	}
	t.Cleanup(func() {
		stopT := 5 * time.Second
		_ = docker.Stop(context.Background(), res.ContainerID, &stopT)
	})

	hostPort := waitForPGPort(t, docker, res.ContainerID)
	dsn := fmt.Sprintf("postgres://postgres:test@127.0.0.1:%d/cache?sslmode=disable", hostPort)
	rawDB := waitForPGReady(t, dsn, 30*time.Second)
	t.Cleanup(func() { _ = rawDB.Close() })

	runCacheConformance(t, "postgres", func(t *testing.T) Cache {
		// Per-subtest table prefix for isolation.
		prefix := "mkc_" + strings.ReplaceAll(strings.ReplaceAll(t.Name(), "/", "_"), "-", "_") + "_"
		conn := &mkfstdb.Connection{Conn: rawDB, Config: mkfstdb.ConnectionInfo{Type: "POSTGRESQL"}}
		c, err := NewSQLCache(conn, SQLOpts{TablePrefix: prefix, SweepInterval: -1})
		if err != nil {
			t.Fatalf("NewSQLCache: %v", err)
		}
		t.Cleanup(func() {
			_, _ = rawDB.Exec(fmt.Sprintf("DROP TABLE IF EXISTS %sentries CASCADE", prefix))
		})
		return c
	})
}

func waitForPGPort(t *testing.T, c *dockerprov.Client, containerID string) int {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		info, err := c.Inspect(ctx, containerID)
		cancel()
		if err == nil && info.NetworkSettings != nil {
			for portKey, bindings := range info.NetworkSettings.Ports {
				if portKey.Port() == "5432" && len(bindings) > 0 {
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
	t.Fatalf("postgres container never published port 5432")
	return 0
}

func waitForPGReady(t *testing.T, dsn string, timeout time.Duration) *sql.DB {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var db *sql.DB
	var err error
	for time.Now().Before(deadline) {
		db, err = sql.Open("pgx", dsn)
		if err == nil {
			pingCtx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
			err = db.PingContext(pingCtx)
			cancel()
			if err == nil {
				db.SetMaxOpenConns(20)
				return db
			}
			_ = db.Close()
		}
		time.Sleep(250 * time.Millisecond)
	}
	t.Fatalf("postgres never became ready: %v", err)
	return nil
}
