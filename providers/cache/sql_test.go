package cache

import (
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"

	_ "modernc.org/sqlite"

	mkfstdb "mkfst/db"
)

func TestSQLiteCacheConformance(t *testing.T) {
	runCacheConformance(t, "sqlite", func(t *testing.T) Cache {
		return newSQLiteTestCache(t)
	})
}

func newSQLiteTestCache(t *testing.T) Cache {
	t.Helper()
	dir := os.Getenv("XDG_RUNTIME_DIR")
	if dir == "" {
		dir = t.TempDir()
	}
	safeName := strings.NewReplacer("/", "_", ":", "_").Replace(t.Name())
	path := filepath.Join(dir, "mkfst-cache-"+safeName+".db")
	t.Cleanup(func() { _ = os.Remove(path) })

	raw, err := sql.Open("sqlite", path+"?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)")
	if err != nil {
		t.Fatalf("sqlite open: %v", err)
	}
	t.Cleanup(func() { _ = raw.Close() })

	conn := &mkfstdb.Connection{
		Conn:   raw,
		Config: mkfstdb.ConnectionInfo{Type: "SQLITE"},
	}
	c, err := NewSQLCache(conn, SQLOpts{
		// Disable the background sweeper for tests — TTL semantics
		// are verified via the foreground-filter path. The sweeper
		// would just race the test cleanup.
		SweepInterval: -1,
	})
	if err != nil {
		t.Fatalf("NewSQLCache: %v", err)
	}
	return c
}
