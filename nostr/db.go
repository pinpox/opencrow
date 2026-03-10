package nostr

import (
	"context"
	"database/sql"
	_ "embed"
	"errors"
	"fmt"
	"path/filepath"

	// Register the pure-Go SQLite driver for the nostr backend's persistent state.
	_ "modernc.org/sqlite"
)

const nostrDBFile = "nostr.db"

//go:embed sqlc/schema.sql
var nostrSchema string

// DB wraps a SQLite database for the nostr backend's persistent state:
// seen rumors (dedup) and publish queue (retry).
type DB struct {
	sql     *sql.DB
	queries *Queries
}

// OpenDB opens (or creates) the nostr SQLite database in dataDir.
func OpenDB(ctx context.Context, dataDir string) (*DB, error) {
	if dataDir == "" {
		return nil, errors.New("dataDir must not be empty")
	}

	dbPath := filepath.Join(dataDir, nostrDBFile)

	sqlDB, err := sql.Open("sqlite", dbPath+"?_journal_mode=WAL&_busy_timeout=5000")
	if err != nil {
		return nil, fmt.Errorf("opening nostr db: %w", err)
	}

	if err := sqlDB.PingContext(ctx); err != nil {
		sqlDB.Close()

		return nil, fmt.Errorf("pinging nostr db: %w", err)
	}

	if err := migrateNostrDB(ctx, sqlDB); err != nil {
		sqlDB.Close()

		return nil, err
	}

	return &DB{
		sql:     sqlDB,
		queries: New(sqlDB),
	}, nil
}

// Close closes the underlying database connection.
func (d *DB) Close() error {
	if err := d.sql.Close(); err != nil {
		return fmt.Errorf("closing nostr db: %w", err)
	}

	return nil
}

func migrateNostrDB(ctx context.Context, db *sql.DB) error {
	if _, err := db.ExecContext(ctx, nostrSchema); err != nil {
		return fmt.Errorf("migrating nostr db: %w", err)
	}

	return nil
}
