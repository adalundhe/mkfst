//go:build sql_integration

package tasks

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"

	dockerprov "mkfst/providers/docker"
	mkfstdb "mkfst/db"
)

// TestPostgresStoreConformance runs the SQL-store conformance suite
// against a real Postgres instance spun up via mkfst's own docker
// provider. Gated behind `sql_integration` so default test runs don't
// need a docker daemon.
//
// Run via:
//   go test -tags sql_integration ./providers/tasks/
//
// Honors DOCKER_HOST so it works against rootless docker.
func TestPostgresStoreConformance(t *testing.T) {
	docker, err := dockerprov.New(dockerprov.Opts{Timeout: 5 * time.Second})
	if err != nil {
		t.Skipf("docker daemon unreachable: %v", err)
	}
	t.Cleanup(func() { _ = docker.Close() })

	// Pull postgres if missing.
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	if _, err := docker.SDK().ImageInspect(ctx, "postgres:16-alpine"); err != nil {
		ev, perr := docker.Pull(ctx, "postgres:16-alpine")
		if perr != nil {
			t.Skipf("postgres image pull failed: %v", perr)
		}
		if _, derr := dockerprov.DrainEvents(ev); derr != nil {
			t.Skipf("postgres image pull stream: %v", derr)
		}
	}

	// Run a one-shot postgres container with an exposed port. Auto-
	// removed on stop. Fast startup with disabled fsync (in-memory
	// for our tests).
	containerName := fmt.Sprintf("mkfst-tasks-pg-%d", time.Now().UnixNano())
	res, err := docker.Run(ctx, "postgres:16-alpine",
		dockerprov.Name(containerName),
		dockerprov.Env("POSTGRES_PASSWORD", "test"),
		dockerprov.Env("POSTGRES_DB", "tasks"),
		dockerprov.Cmd("postgres",
			"-c", "fsync=off",
			"-c", "synchronous_commit=off",
			"-c", "full_page_writes=off",
		),
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
	dsn := fmt.Sprintf("postgres://postgres:test@127.0.0.1:%d/tasks?sslmode=disable", hostPort)

	// Wait for postgres to actually accept connections (initdb is
	// async — TCP open ≠ ready for queries).
	rawDB := waitForPGReady(t, dsn, 30*time.Second)
	t.Cleanup(func() { _ = rawDB.Close() })

	runStoreConformance(t, "postgres", func(t *testing.T) Store {
		// Each subtest gets a fresh schema via TablePrefix so there's
		// no cross-test bleed. We can't truly isolate because we
		// share one container; prefixing tables is the next best.
		prefix := "mt_" + strings.ReplaceAll(strings.ReplaceAll(t.Name(), "/", "_"), "-", "_") + "_"
		conn := &mkfstdb.Connection{Conn: rawDB, Config: mkfstdb.ConnectionInfo{Type: "POSTGRESQL"}}
		store, err := NewSQLStore(conn, SQLOpts{DedupWindow: time.Minute, TablePrefix: prefix})
		if err != nil {
			t.Fatalf("NewSQLStore: %v", err)
		}
		t.Cleanup(func() {
			// Drop the test's tables so the next subtest starts clean.
			_, _ = rawDB.Exec(fmt.Sprintf("DROP TABLE IF EXISTS %stasks, %sdedup CASCADE", prefix, prefix))
		})
		return store
	})
	_ = os.Stdout // keep unused-import scanner happy across go versions
}

// waitForPGPort discovers the host port the daemon assigned to the
// container's 5432.
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

// waitForPGReady opens a *sql.DB and pings until ready or timeout.
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
