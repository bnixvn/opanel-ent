// Package db opens the SQLite store and keeps its schema current.
//
// The driver is modernc.org/sqlite, a pure-Go implementation. That choice is
// load-bearing: it keeps CGO_ENABLED=0 possible, which is what lets the panel
// ship as one static binary with no Python venv beside CloudLinux's own
// Python tooling.
package db

import (
	"context"
	"database/sql"
	"embed"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/pressly/goose/v3"
	_ "modernc.org/sqlite"
)

//go:embed all:migrations
var migrationsFS embed.FS

// DB wraps *sql.DB so callers depend on this package rather than database/sql
// directly, leaving room to add tracing or metrics in one place.
type DB struct {
	*sql.DB
	path string
}

// Open creates the database file if needed, applies pragmas, and migrates to
// the newest schema version.
func Open(ctx context.Context, path string) (*DB, error) {
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o750); err != nil {
			return nil, fmt.Errorf("create data dir %s: %w", dir, err)
		}
	}

	// _txlock=immediate takes the write lock at BEGIN instead of at first
	// write, which turns SQLITE_BUSY deadlocks between concurrent writers
	// into an ordinary wait.
	dsn := path + "?_pragma=busy_timeout(10000)&_pragma=journal_mode(WAL)" +
		"&_pragma=synchronous(NORMAL)&_pragma=foreign_keys(ON)&_txlock=immediate"

	sqlDB, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open sqlite %s: %w", path, err)
	}

	// SQLite takes one writer at a time. A single connection removes lock
	// contention entirely; WAL still allows concurrent readers through the
	// same handle.
	sqlDB.SetMaxOpenConns(1)
	sqlDB.SetMaxIdleConns(1)
	sqlDB.SetConnMaxLifetime(0)

	pingCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := sqlDB.PingContext(pingCtx); err != nil {
		_ = sqlDB.Close()
		return nil, fmt.Errorf("ping sqlite %s: %w", path, err)
	}

	d := &DB{DB: sqlDB, path: path}
	if err := d.Migrate(ctx); err != nil {
		_ = sqlDB.Close()
		return nil, err
	}
	return d, nil
}

// Path returns the database file location, for logs and diagnostics.
func (d *DB) Path() string { return d.path }

// Migrate applies any pending migrations. It is safe to call on every start.
func (d *DB) Migrate(ctx context.Context) error {
	goose.SetBaseFS(migrationsFS)
	goose.SetLogger(goose.NopLogger())
	if err := goose.SetDialect("sqlite3"); err != nil {
		return fmt.Errorf("goose dialect: %w", err)
	}
	if err := goose.UpContext(ctx, d.DB, "migrations"); err != nil {
		return fmt.Errorf("migrate: %w", err)
	}
	return nil
}

// SchemaVersion reports the newest applied migration.
func (d *DB) SchemaVersion(ctx context.Context) (int64, error) {
	if err := goose.SetDialect("sqlite3"); err != nil {
		return 0, err
	}
	return goose.GetDBVersionContext(ctx, d.DB)
}

// InTx runs fn inside a transaction, rolling back on error or panic.
func (d *DB) InTx(ctx context.Context, fn func(*sql.Tx) error) (err error) {
	tx, err := d.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() {
		if p := recover(); p != nil {
			_ = tx.Rollback()
			panic(p)
		}
		if err != nil {
			_ = tx.Rollback()
		}
	}()
	if err = fn(tx); err != nil {
		return err
	}
	return tx.Commit()
}
