package db

import (
	"database/sql"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5/stdlib"
)

// relay_traffic is a shared per-UUID traffic counter used by the relay tier
// (SQLite or PostgreSQL). It mirrors the relay_tickets store: the same
// *sql.DB must back every server instance of the cluster.
//
// SQLite and PostgreSQL differ in two ways that matter here:
//   - placeholders: ? vs $1..$n (see sqlPlaceholders)
//   - current-timestamp expression: datetime('now') vs NOW()
const (
	relayTrafficDDLSQLite = `CREATE TABLE IF NOT EXISTS relay_traffic (
		uuid TEXT PRIMARY KEY,
		bytes INTEGER NOT NULL DEFAULT 0,
		updated_at TEXT NOT NULL DEFAULT (datetime('now'))
	)`
	relayTrafficDDLPostgres = `CREATE TABLE IF NOT EXISTS relay_traffic (
		uuid TEXT PRIMARY KEY,
		bytes BIGINT NOT NULL DEFAULT 0,
		updated_at TEXT NOT NULL DEFAULT NOW()
	)`
)

// isPostgresDriver reports whether db is backed by the PostgreSQL driver.
//
// The pgx stdlib package registers its driver under the names "pgx" and
// "pgx/v5", but the dynamic type of the registered instance is *stdlib.Driver
// — a type-assertion is the reliable check (same logic as relay package).
func isPostgresDriver(db *sql.DB) bool {
	if db == nil {
		return false
	}
	_, ok := db.Driver().(*stdlib.Driver)
	return ok
}

// sqlPlaceholders renders a SQLite-style statement with ? placeholders for the
// backing driver (PostgreSQL's extended protocol requires $1..$n).
func sqlPlaceholders(stmt string, pg bool) string {
	if !pg {
		return stmt
	}
	var b strings.Builder
	n := 0
	for i := 0; i < len(stmt); i++ {
		if stmt[i] == '?' {
			n++
			fmt.Fprintf(&b, "$%d", n)
		} else {
			b.WriteByte(stmt[i])
		}
	}
	return b.String()
}

// relayTrafficNowExpr returns the driver-appropriate current-timestamp
// expression for the updated_at column.
func relayTrafficNowExpr(pg bool) string {
	if pg {
		return "NOW()"
	}
	return "datetime('now')"
}

// EnsureRelayTrafficTable creates the shared relay_traffic table if missing
// (idempotent). Call it once at startup before using the store.
func EnsureRelayTrafficTable(db *sql.DB) error {
	if db == nil {
		return fmt.Errorf("db: relay traffic ensure table: nil database")
	}
	ddl := relayTrafficDDLSQLite
	if isPostgresDriver(db) {
		ddl = relayTrafficDDLPostgres
	}
	if _, err := db.Exec(ddl); err != nil {
		return fmt.Errorf("db: relay traffic ensure table: %w", err)
	}
	return nil
}

// UpsertRelayTraffic inserts or replaces the traffic counter for uuid with the
// given cumulative byte count.
func UpsertRelayTraffic(db *sql.DB, uuid string, bytes int64) error {
	if db == nil {
		return fmt.Errorf("db: relay traffic upsert: nil database")
	}
	pg := isPostgresDriver(db)
	stmt := sqlPlaceholders(fmt.Sprintf(
		`INSERT INTO relay_traffic (uuid, bytes, updated_at) VALUES (?, ?, %s)
		 ON CONFLICT(uuid) DO UPDATE SET bytes = excluded.bytes, updated_at = %s`,
		relayTrafficNowExpr(pg), relayTrafficNowExpr(pg)), pg)
	if _, err := db.Exec(stmt, uuid, bytes); err != nil {
		return fmt.Errorf("db: relay traffic upsert %q: %w", uuid, err)
	}
	return nil
}

// GetAllRelayTraffic returns a snapshot of every uuid -> bytes counter.
func GetAllRelayTraffic(db *sql.DB) (map[string]int64, error) {
	if db == nil {
		return nil, fmt.Errorf("db: relay traffic list: nil database")
	}
	rows, err := db.Query(`SELECT uuid, bytes FROM relay_traffic`)
	if err != nil {
		return nil, fmt.Errorf("db: relay traffic list: %w", err)
	}
	defer rows.Close()
	out := make(map[string]int64)
	for rows.Next() {
		var uuid string
		var bytes int64
		if err := rows.Scan(&uuid, &bytes); err != nil {
			return nil, fmt.Errorf("db: relay traffic list: %w", err)
		}
		out[uuid] = bytes
	}
	return out, rows.Err()
}

// DeleteRelayTraffic removes the counters for the given uuids. An empty
// argument list is a no-op.
func DeleteRelayTraffic(db *sql.DB, uuids ...string) error {
	if db == nil {
		return fmt.Errorf("db: relay traffic delete: nil database")
	}
	if len(uuids) == 0 {
		return nil
	}
	args := make([]any, 0, len(uuids))
	marks := make([]string, 0, len(uuids))
	for _, u := range uuids {
		args = append(args, u)
		marks = append(marks, "?")
	}
	stmt := sqlPlaceholders("DELETE FROM relay_traffic WHERE uuid IN ("+strings.Join(marks, ",")+")", isPostgresDriver(db))
	if _, err := db.Exec(stmt, args...); err != nil {
		return fmt.Errorf("db: relay traffic delete: %w", err)
	}
	return nil
}
