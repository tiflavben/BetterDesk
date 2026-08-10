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

// ---------------------------------------------------------------------------
// relay_heartbeat — per-node liveness/session telemetry written by each relay
// instance (relay-only nodes write the shared store; the signal/API side reads
// it to surface per-node active sessions and cumulative traffic).
// ---------------------------------------------------------------------------

const (
	relayHeartbeatDDLSQLite = `CREATE TABLE IF NOT EXISTS relay_heartbeat (
		node_id TEXT PRIMARY KEY,
		addr TEXT NOT NULL DEFAULT '',
		active_sessions INTEGER NOT NULL DEFAULT 0,
		total_bytes BIGINT NOT NULL DEFAULT 0,
		cpu_percent REAL NOT NULL DEFAULT 0,
		mem_percent REAL NOT NULL DEFAULT 0,
		bandwidth_mbps REAL NOT NULL DEFAULT 0,
		last_seen TEXT NOT NULL DEFAULT (datetime('now'))
	)`
	relayHeartbeatDDLPostgres = `CREATE TABLE IF NOT EXISTS relay_heartbeat (
		node_id TEXT PRIMARY KEY,
		addr TEXT NOT NULL DEFAULT '',
		active_sessions INTEGER NOT NULL DEFAULT 0,
		total_bytes BIGINT NOT NULL DEFAULT 0,
		cpu_percent DOUBLE PRECISION NOT NULL DEFAULT 0,
		mem_percent DOUBLE PRECISION NOT NULL DEFAULT 0,
		bandwidth_mbps DOUBLE PRECISION NOT NULL DEFAULT 0,
		last_seen TEXT NOT NULL DEFAULT NOW()
	)`
)

// EnsureRelayHeartbeatTable creates the shared relay heartbeat table and
// migrates pre-existing tables (added cpu/mem/bandwidth columns).
func EnsureRelayHeartbeatTable(db *sql.DB) error {
	if db == nil {
		return fmt.Errorf("db: nil database for relay heartbeat table")
	}
	ddl := relayHeartbeatDDLSQLite
	pg := isPostgresDriver(db)
	if pg {
		ddl = relayHeartbeatDDLPostgres
	}
	if _, err := db.Exec(ddl); err != nil {
		return fmt.Errorf("db: relay heartbeat migrate: %w", err)
	}
	// Column migration for tables created before the metrics columns.
	have := func(col string) bool {
		if pg {
			var n int
			// information_schema check is robust across PG versions.
			_ = db.QueryRow(`SELECT COUNT(*) FROM information_schema.columns
				WHERE table_name='relay_heartbeat' AND column_name=$1`, col).Scan(&n)
			return n > 0
		}
		rows, err := db.Query(`PRAGMA table_info(relay_heartbeat)`)
		if err != nil {
			return true // leave as-is on error
		}
		defer rows.Close()
		for rows.Next() {
			var cid, notnull, pk int
			var name, ctype string
			var dflt any
			if err := rows.Scan(&cid, &name, &ctype, &notnull, &dflt, &pk); err != nil {
				return true
			}
			if name == col {
				return true
			}
		}
		return false
	}
	for _, col := range []struct{ name, typ string }{
		{"cpu_percent", "REAL"}, {"mem_percent", "REAL"}, {"bandwidth_mbps", "REAL"},
	} {
		if have(col.name) {
			continue
		}
		if _, err := db.Exec("ALTER TABLE relay_heartbeat ADD COLUMN " + col.name + " " + col.typ + " NOT NULL DEFAULT 0"); err != nil {
			return fmt.Errorf("db: relay heartbeat add column %s: %w", col.name, err)
		}
	}
	return nil
}

// UpsertRelayHeartbeat records (or refreshes) a relay node's liveness row,
// including per-node system metrics (cpu/mem percent, bandwidth Mbps).
func UpsertRelayHeartbeat(db *sql.DB, nodeID, addr string, activeSessions, totalBytes int64, cpuPct, memPct, bwMbps float64) error {
	if db == nil {
		return fmt.Errorf("db: nil database for relay heartbeat upsert")
	}
	q := "INSERT INTO relay_heartbeat (node_id, addr, active_sessions, total_bytes, cpu_percent, mem_percent, bandwidth_mbps) VALUES (?, ?, ?, ?, ?, ?, ?) " +
		"ON CONFLICT(node_id) DO UPDATE SET addr=excluded.addr, active_sessions=excluded.active_sessions, " +
		"total_bytes=excluded.total_bytes, cpu_percent=excluded.cpu_percent, mem_percent=excluded.mem_percent, " +
		"bandwidth_mbps=excluded.bandwidth_mbps, last_seen=datetime('now')"
	if isPostgresDriver(db) {
		q = "INSERT INTO relay_heartbeat (node_id, addr, active_sessions, total_bytes, cpu_percent, mem_percent, bandwidth_mbps) VALUES ($1, $2, $3, $4, $5, $6, $7) " +
			"ON CONFLICT(node_id) DO UPDATE SET addr=EXCLUDED.addr, active_sessions=EXCLUDED.active_sessions, " +
			"total_bytes=EXCLUDED.total_bytes, cpu_percent=EXCLUDED.cpu_percent, mem_percent=EXCLUDED.mem_percent, " +
			"bandwidth_mbps=EXCLUDED.bandwidth_mbps, last_seen=NOW()"
	}
	_, err := db.Exec(q, nodeID, addr, activeSessions, totalBytes, cpuPct, memPct, bwMbps)
	return err
}

// RelayHeartbeat is one row of the shared heartbeat table.
type RelayHeartbeat struct {
	NodeID         string
	Addr           string
	ActiveSessions int64
	TotalBytes     int64
	CPUPercent     float64
	MemPercent     float64
	BandwidthMbps  float64
	LastSeen       string
}

// GetAllRelayHeartbeats returns every relay node's latest heartbeat.
func GetAllRelayHeartbeats(db *sql.DB) (map[string]RelayHeartbeat, error) {
	if db == nil {
		return nil, fmt.Errorf("db: nil database for relay heartbeat read")
	}
	rows, err := db.Query(`SELECT node_id, addr, active_sessions, total_bytes, cpu_percent, mem_percent, bandwidth_mbps, last_seen FROM relay_heartbeat`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]RelayHeartbeat{}
	for rows.Next() {
		var h RelayHeartbeat
		if err := rows.Scan(&h.NodeID, &h.Addr, &h.ActiveSessions, &h.TotalBytes, &h.CPUPercent, &h.MemPercent, &h.BandwidthMbps, &h.LastSeen); err != nil {
			return nil, err
		}
		out[h.NodeID] = h
	}
	return out, rows.Err()
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
