package tasks

import (
	"database/sql"
	"os"
	"path/filepath"
	"testing"
	"time"

	// Pure-Go sqlite for tests so the suite runs without CGO. mkfst's
	// production code can use either go-sqlite3 (CGO, faster) or this
	// driver — they both register `*sql.DB`-compatible drivers; the
	// db.Connection just hands us *sql.DB.
	_ "modernc.org/sqlite"

	"mkfst/db"
)

// TestSQLiteStoreConformance runs the full conformance suite against
// an in-memory SQLite database. SQLite is in-tree (no daemon needed)
// so this runs by default.
//
// Each subtest gets its own database file (well, technically a
// distinct in-memory db via the file::memory: + cache=shared trick)
// to avoid cross-test pollution.
func TestSQLiteStoreConformance(t *testing.T) {
	runStoreConformance(t, "sqlite", func(t *testing.T) Store {
		return newSQLiteTestStore(t)
	})
}

// newSQLiteTestStore opens a fresh per-test SQLite database and
// returns a tasks.Store backed by it. The DB lives in $XDG_RUNTIME_DIR
// (tmpfs) per the project's strict no-disk policy for runtime data,
// and is removed when the test ends.
func newSQLiteTestStore(t *testing.T) Store {
	t.Helper()
	dir := os.Getenv("XDG_RUNTIME_DIR")
	if dir == "" {
		dir = t.TempDir() // fallback for CI environments without XDG
	}
	path := filepath.Join(dir, "mkfst-tasks-"+t.Name()+".db")
	// Sanitize: t.Name() can contain "/" from subtest names.
	for i, r := range path {
		if r == '/' && i > len(dir) {
			path = path[:i] + "_" + path[i+1:]
		}
	}
	t.Cleanup(func() { _ = os.Remove(path) })

	// modernc.org/sqlite pragma syntax differs from go-sqlite3:
	// _pragma=name(value) instead of _name=value. WAL+busy_timeout
	// matter for the concurrent-claim test.
	raw, err := sql.Open("sqlite", path+"?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)")
	if err != nil {
		t.Fatalf("sqlite open: %v", err)
	}
	t.Cleanup(func() { _ = raw.Close() })

	conn := &db.Connection{
		Conn:   raw,
		Config: db.ConnectionInfo{Type: "SQLITE"},
	}
	store, err := NewSQLStore(conn, SQLOpts{DedupWindow: time.Minute})
	if err != nil {
		t.Fatalf("NewSQLStore: %v", err)
	}
	return store
}
