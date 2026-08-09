package db

import (
	"database/sql"
	"fmt"
	"strings"

	_ "github.com/jackc/pgx/v5/stdlib" // registers the "pgx" database/sql driver
	_ "modernc.org/sqlite"             // registers the "sqlite" database/sql driver
)

// OpenRelayTicketStore opens a standalone *sql.DB used as the shared relay
// ticket store for clustered (multi-relay) deployments.
//
// databaseURL takes precedence: postgres:// or postgresql:// URLs open
// PostgreSQL via the pgx stdlib driver. Anything else is treated as a SQLite
// file path (dbPath) opened with WAL journaling and a busy timeout so multiple
// server processes can share the same file.
func OpenRelayTicketStore(databaseURL, dbPath string) (*sql.DB, error) {
	lower := strings.ToLower(databaseURL)
	if strings.HasPrefix(lower, "postgres://") || strings.HasPrefix(lower, "postgresql://") {
		sqlDB, err := sql.Open("pgx", databaseURL)
		if err != nil {
			return nil, fmt.Errorf("db: failed to open PostgreSQL relay ticket store: %w", err)
		}
		sqlDB.SetMaxOpenConns(10)
		sqlDB.SetMaxIdleConns(5)
		sqlDB.SetConnMaxLifetime(0)
		if err := sqlDB.Ping(); err != nil {
			sqlDB.Close()
			return nil, fmt.Errorf("db: PostgreSQL relay ticket store ping failed: %w", err)
		}
		return sqlDB, nil
	}

	dsn := fmt.Sprintf("file:%s?_journal_mode=WAL&_busy_timeout=5000&_foreign_keys=ON", dbPath)
	sqlDB, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("db: failed to open SQLite relay ticket store %q: %w", dbPath, err)
	}
	sqlDB.SetMaxOpenConns(1) // SQLite single-writer
	sqlDB.SetMaxIdleConns(1)
	sqlDB.SetConnMaxLifetime(0)
	if err := sqlDB.Ping(); err != nil {
		sqlDB.Close()
		return nil, fmt.Errorf("db: SQLite relay ticket store ping failed: %w", err)
	}
	return sqlDB, nil
}
