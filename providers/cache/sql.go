package cache

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"mkfst/db"
)

// SQLOpts configures NewSQLCache.
type SQLOpts struct {
	// TablePrefix is prepended to the cache table name. Lets multiple
	// caches coexist in the same schema. Default "mkfst_cache_".
	TablePrefix string

	// SweepInterval governs background expiry. The SQL cache filters
	// expired rows on read (so a stale Get always misses correctly),
	// but eventually has to physically delete expired rows or the
	// table grows unbounded. SweepInterval is how often a background
	// goroutine deletes expired rows. 0 disables the sweeper —
	// useful in tests where you want pure-foreground behavior.
	// Default 5 minutes.
	SweepInterval time.Duration
}

type sqlDialect int

const (
	sqlDialectSQLite sqlDialect = iota
	sqlDialectPostgres
	sqlDialectMySQL
)

func dialectFor(connType string) (sqlDialect, error) {
	switch strings.ToUpper(connType) {
	case "", "SQLITE":
		return sqlDialectSQLite, nil
	case "POSTGRESQL", "POSTGRES":
		return sqlDialectPostgres, nil
	case "MYSQL":
		return sqlDialectMySQL, nil
	default:
		return 0, fmt.Errorf("cache.NewSQLCache: unsupported db type %q", connType)
	}
}

// NewSQLCache returns a Cache backed by an SQL database accessed
// through mkfst's db.Connection. Supports PostgreSQL, MySQL 5.7+,
// and SQLite.
//
// On first use, runs idempotent CREATE TABLE IF NOT EXISTS migrations.
// Subsequent constructions reuse the existing table.
//
// Spawns a background sweeper goroutine that physically deletes
// expired rows on SweepInterval cadence. The sweeper is anchored to
// the supplied connection's lifetime — Close() stops it cleanly.
func NewSQLCache(conn *db.Connection, opts SQLOpts) (Cache, error) {
	if conn == nil || conn.Conn == nil {
		return nil, errors.New("cache.NewSQLCache: nil connection")
	}
	d, err := dialectFor(conn.Config.Type)
	if err != nil {
		return nil, err
	}
	if opts.TablePrefix == "" {
		opts.TablePrefix = "mkfst_cache_"
	}
	if opts.SweepInterval == 0 {
		opts.SweepInterval = 5 * time.Minute
	}
	c := &sqlCache{
		db:      conn.Conn,
		dialect: d,
		opts:    opts,
		now:     time.Now,
		stopCh:  make(chan struct{}),
		doneCh:  make(chan struct{}),
	}
	if err := c.migrate(context.Background()); err != nil {
		return nil, fmt.Errorf("cache.NewSQLCache: migrate: %w", err)
	}
	if opts.SweepInterval > 0 {
		go c.sweepLoop()
	} else {
		close(c.doneCh)
	}
	return c, nil
}

type sqlCache struct {
	db      *sql.DB
	dialect sqlDialect
	opts    SQLOpts

	now    func() time.Time
	closed bool

	stopCh chan struct{}
	doneCh chan struct{}
}

func (c *sqlCache) table() string { return c.opts.TablePrefix + "entries" }

func (c *sqlCache) migrate(ctx context.Context) error {
	t := c.table()
	var stmt string
	switch c.dialect {
	case sqlDialectPostgres:
		stmt = fmt.Sprintf(`CREATE TABLE IF NOT EXISTS %s (
			cache_key   VARCHAR(512) PRIMARY KEY,
			cache_value BYTEA NOT NULL,
			expires_at  TIMESTAMPTZ
		)`, t)
	case sqlDialectMySQL:
		stmt = fmt.Sprintf(`CREATE TABLE IF NOT EXISTS %s (
			cache_key   VARCHAR(512) PRIMARY KEY,
			cache_value BLOB NOT NULL,
			expires_at  DATETIME(6)
		) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4`, t)
	default: // SQLite
		stmt = fmt.Sprintf(`CREATE TABLE IF NOT EXISTS %s (
			cache_key   TEXT PRIMARY KEY,
			cache_value BLOB NOT NULL,
			expires_at  INTEGER
		)`, t)
	}
	if _, err := c.db.ExecContext(ctx, stmt); err != nil {
		return fmt.Errorf("create table: %w", err)
	}

	// Index on expires_at speeds up the sweeper. Skip for tiny
	// caches; the cost is negligible at scale.
	idxStmt := fmt.Sprintf(`CREATE INDEX IF NOT EXISTS %s_exp ON %s (expires_at)`, c.table(), c.table())
	if _, err := c.db.ExecContext(ctx, idxStmt); err != nil {
		// Some MySQL versions can't create the index this way
		// post-table-creation if the column already has implicit
		// indexing. Non-fatal.
		_ = err
	}
	return nil
}

func (c *sqlCache) Get(ctx context.Context, key string) ([]byte, bool, error) {
	if c.closed {
		return nil, false, ErrClosed
	}
	q := c.rebind(`SELECT cache_value, expires_at FROM ` + c.table() + ` WHERE cache_key = ?`)
	var value []byte
	var expiresRaw interface{}
	err := c.db.QueryRowContext(ctx, q, key).Scan(&value, &expiresRaw)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, false, nil
		}
		return nil, false, fmt.Errorf("get: %w", err)
	}
	if !c.notExpired(expiresRaw) {
		// Stale entry. Treat as miss; the sweeper will physically
		// delete it on its next pass.
		return nil, false, nil
	}
	return value, true, nil
}

func (c *sqlCache) Set(ctx context.Context, key string, value []byte, ttl time.Duration) error {
	if c.closed {
		return ErrClosed
	}
	var expires interface{}
	if ttl > 0 {
		expires = c.encodeTime(c.now().Add(ttl))
	}

	var q string
	switch c.dialect {
	case sqlDialectPostgres:
		q = c.rebind(`INSERT INTO ` + c.table() + ` (cache_key, cache_value, expires_at)
			VALUES (?, ?, ?)
			ON CONFLICT (cache_key) DO UPDATE
			  SET cache_value = EXCLUDED.cache_value, expires_at = EXCLUDED.expires_at`)
	case sqlDialectMySQL:
		q = `INSERT INTO ` + c.table() + ` (cache_key, cache_value, expires_at)
			VALUES (?, ?, ?)
			ON DUPLICATE KEY UPDATE
			  cache_value = VALUES(cache_value), expires_at = VALUES(expires_at)`
	default: // SQLite
		q = `INSERT INTO ` + c.table() + ` (cache_key, cache_value, expires_at)
			VALUES (?, ?, ?)
			ON CONFLICT (cache_key) DO UPDATE
			  SET cache_value = excluded.cache_value, expires_at = excluded.expires_at`
	}
	if _, err := c.db.ExecContext(ctx, q, key, value, expires); err != nil {
		return fmt.Errorf("set: %w", err)
	}
	return nil
}

func (c *sqlCache) Delete(ctx context.Context, key string) error {
	if c.closed {
		return ErrClosed
	}
	q := c.rebind(`DELETE FROM ` + c.table() + ` WHERE cache_key = ?`)
	if _, err := c.db.ExecContext(ctx, q, key); err != nil {
		return fmt.Errorf("delete: %w", err)
	}
	return nil
}

func (c *sqlCache) DeletePrefix(ctx context.Context, prefix string) (int, error) {
	if c.closed {
		return 0, ErrClosed
	}
	// Use LIKE on the indexed primary key. PG and SQLite use
	// straightforward LIKE; MySQL is the same. Escape the LIKE
	// wildcards in the user-supplied prefix to avoid surprise matches.
	escapedPrefix := likeEscape(prefix) + "%"
	// Single-char ESCAPE clause — SQLite rejects multi-char strings
	// here. Backtick-string `'\'` is exactly 3 chars: quote, single
	// backslash, quote — what every dialect wants.
	q := `DELETE FROM ` + c.table() + ` WHERE cache_key LIKE ? ESCAPE '\'`
	if c.dialect == sqlDialectPostgres {
		q = c.rebind(q)
	}
	res, err := c.db.ExecContext(ctx, q, escapedPrefix)
	if err != nil {
		return 0, fmt.Errorf("delete prefix: %w", err)
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}

func (c *sqlCache) Close() error {
	if c.closed {
		return nil
	}
	c.closed = true
	close(c.stopCh)
	<-c.doneCh
	return nil
}

// === sweeper ===

// sweepLoop periodically deletes expired rows. Owned via stopCh +
// doneCh so Close blocks until the sweeper exits.
func (c *sqlCache) sweepLoop() {
	defer close(c.doneCh)
	ticker := time.NewTicker(c.opts.SweepInterval)
	defer ticker.Stop()
	for {
		select {
		case <-c.stopCh:
			return
		case <-ticker.C:
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			q := c.rebind(`DELETE FROM ` + c.table() + ` WHERE expires_at IS NOT NULL AND expires_at < ?`)
			_, _ = c.db.ExecContext(ctx, q, c.encodeTime(c.now()))
			cancel()
		}
	}
}

// === helpers ===

// rebind converts ?-style placeholders to PG's $1, $2 form.
func (c *sqlCache) rebind(query string) string {
	if c.dialect != sqlDialectPostgres {
		return query
	}
	var b strings.Builder
	b.Grow(len(query))
	idx := 1
	for _, r := range query {
		if r == '?' {
			fmt.Fprintf(&b, "$%d", idx)
			idx++
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

// encodeTime adapts time.Time for the active dialect. Same shape as
// providers/tasks/sql.go — SQLite gets unix nanos as INTEGER, PG/
// MySQL use native time.
func (c *sqlCache) encodeTime(t time.Time) interface{} {
	if t.IsZero() {
		return nil
	}
	if c.dialect == sqlDialectSQLite {
		return t.UnixNano()
	}
	return t
}

// notExpired reports whether the row's expires_at is either NULL
// (no expiry) or in the future relative to c.now().
func (c *sqlCache) notExpired(raw interface{}) bool {
	if raw == nil {
		return true
	}
	now := c.now()
	switch v := raw.(type) {
	case time.Time:
		return v.After(now)
	case int64:
		if v == 0 {
			return true
		}
		return time.Unix(0, v).After(now)
	case []byte:
		// Some drivers return TEXT timestamps as []byte.
		t, err := time.Parse(time.RFC3339Nano, string(v))
		if err != nil {
			return true
		}
		return t.After(now)
	case string:
		t, err := time.Parse(time.RFC3339Nano, v)
		if err != nil {
			return true
		}
		return t.After(now)
	}
	return true
}

// likeEscape escapes LIKE wildcards (% and _) in a literal string so
// that user-supplied prefixes don't accidentally match more than
// intended. The corresponding SQL uses ESCAPE '\'.
func likeEscape(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)
	return r.Replace(s)
}
