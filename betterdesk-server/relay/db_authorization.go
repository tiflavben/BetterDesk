package relay

import (
	"database/sql"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/stdlib"
	"github.com/unitronix/betterdesk-server/config"
)

// DBAuthorizationRegistry implements AuthorizationStore on a shared database
// (SQLite or PostgreSQL). Tickets are visible to every server instance, which
// allows the relay tier to scale horizontally (hbbr-style): any signal may
// authorize a ticket and any relay instance may claim it.
type DBAuthorizationRegistry struct {
	db  *sql.DB
	pg  bool // PostgreSQL uses $1..$n placeholders; SQLite uses ?
	now func() time.Time
}

const (
	relayTicketsDDL = `CREATE TABLE IF NOT EXISTS relay_tickets (
		uuid TEXT PRIMARY KEY,
		initiator_id TEXT NOT NULL,
		target_id TEXT NOT NULL,
		expires_at INTEGER NOT NULL,
		claims INTEGER NOT NULL DEFAULT 0
	)`
	relayTicketUsedDDL = `CREATE TABLE IF NOT EXISTS relay_ticket_used (
		uuid TEXT PRIMARY KEY,
		expires_at INTEGER NOT NULL
	)`
)

// NewDBAuthorizationRegistry creates a DB-backed ticket store and ensures the
// ticket tables exist (idempotent). The same *sql.DB must back every signal /
// relay instance of the cluster.
func NewDBAuthorizationRegistry(db *sql.DB) (*DBAuthorizationRegistry, error) {
	if db == nil {
		return nil, fmt.Errorf("relay: nil database for DB authorization registry")
	}
	r := &DBAuthorizationRegistry{
		db:  db,
		pg:  isPostgresDriver(db),
		now: time.Now,
	}
	for _, stmt := range []string{relayTicketsDDL, relayTicketUsedDDL} {
		if _, err := db.Exec(stmt); err != nil {
			return nil, fmt.Errorf("relay: ticket store migrate: %w", err)
		}
	}
	return r, nil
}

// isPostgresDriver reports whether db is backed by the PostgreSQL driver.
//
// The pgx stdlib package registers its driver under the names "pgx" and
// "pgx/v5", but the dynamic type of the registered instance is *stdlib.Driver
// — a type-assertion is the reliable check. String-matching %T output
// (strings.Contains(fmt.Sprintf("%T", db.Driver()), "pgx")) is a known
// anti-pattern here: it prints the package-qualified type name, which never
// contains "pgx", so PostgreSQL would be silently misdetected as SQLite and
// every ?-placeholder statement would fail with a syntax error.
func isPostgresDriver(db *sql.DB) bool {
	if db == nil {
		return false
	}
	_, ok := db.Driver().(*stdlib.Driver)
	return ok
}

// sql renders a SQLite-style statement with ? placeholders for the backing
// driver (PostgreSQL's extended protocol requires $1..$n).
func (r *DBAuthorizationRegistry) sql(stmt string) string {
	if !r.pg {
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

// Authorize records an authorized initiator/target pair for uuid. A consumed
// UUID (tombstoned in relay_ticket_used) cannot be re-authorized before its
// expiry, preventing relay replay. An existing ticket may be retried only by
// the same pair while unexpired.
func (r *DBAuthorizationRegistry) Authorize(uuid, initiatorID, targetID string) bool {
	if uuid == "" || initiatorID == "" || targetID == "" {
		return false
	}
	now := r.now().UnixMilli()
	r.pruneExpired(now)

	tx, err := r.db.Begin()
	if err != nil {
		log.Printf("[relay] DB ticket authorize: begin: %v", err)
		return false
	}
	defer tx.Rollback()

	// Replay protection is atomic with the insert: the ticket is only created
	// when no unexpired tombstone exists, closing the cross-process race where
	// a concurrent Claim could tombstone the UUID between check and insert.
	res, err := tx.Exec(r.sql(`INSERT INTO relay_tickets (uuid, initiator_id, target_id, expires_at)
		SELECT ?, ?, ?, ? WHERE NOT EXISTS
		(SELECT 1 FROM relay_ticket_used WHERE uuid = ? AND expires_at > ?)
		ON CONFLICT(uuid) DO NOTHING`),
		uuid, initiatorID, targetID, now+config.RelayPairTimeout.Milliseconds(), uuid, now)
	if err != nil {
		log.Printf("[relay] DB ticket authorize: insert: %v", err)
		return false
	}
	if n, _ := res.RowsAffected(); n == 1 {
		return tx.Commit() == nil
	}

	// Existing ticket: only the same pair may retry, while unexpired.
	var dbInitiator, dbTarget string
	var dbExpires int64
	err = tx.QueryRow(r.sql(`SELECT initiator_id, target_id, expires_at FROM relay_tickets WHERE uuid = ?`), uuid).
		Scan(&dbInitiator, &dbTarget, &dbExpires)
	if err != nil {
		return false
	}
	if dbExpires <= now || dbInitiator != initiatorID || dbTarget != targetID {
		return false
	}
	return tx.Commit() == nil
}

// Claim reserves one of the two connections needed for an authorized relay
// pair. The second successful claim consumes the ticket permanently and
// tombstones the UUID.
func (r *DBAuthorizationRegistry) Claim(uuid string) bool {
	if uuid == "" {
		return false
	}
	now := r.now().UnixMilli()
	r.pruneExpired(now)

	tx, err := r.db.Begin()
	if err != nil {
		log.Printf("[relay] DB ticket claim: begin: %v", err)
		return false
	}
	defer tx.Rollback()

	res, err := tx.Exec(r.sql(`UPDATE relay_tickets SET claims = claims + 1
		WHERE uuid = ? AND claims < 2 AND expires_at > ?`), uuid, now)
	if err != nil {
		log.Printf("[relay] DB ticket claim: update: %v", err)
		return false
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return false
	}

	var claims int
	if err := tx.QueryRow(r.sql(`SELECT claims FROM relay_tickets WHERE uuid = ?`), uuid).Scan(&claims); err != nil {
		return false
	}
	if claims >= 2 {
		// Second claim consumes the ticket and tombstones the UUID.
		var expiresAt int64
		if err := tx.QueryRow(r.sql(`SELECT expires_at FROM relay_tickets WHERE uuid = ?`), uuid).Scan(&expiresAt); err != nil {
			return false
		}
		if _, err := tx.Exec(r.sql(`DELETE FROM relay_tickets WHERE uuid = ?`), uuid); err != nil {
			return false
		}
		if _, err := tx.Exec(r.sql(`INSERT INTO relay_ticket_used (uuid, expires_at) VALUES (?, ?)
			ON CONFLICT(uuid) DO UPDATE SET expires_at = excluded.expires_at`), uuid, expiresAt); err != nil {
			return false
		}
	}
	return tx.Commit() == nil
}

// Release returns a first claim to the ticket when its pending relay
// connection times out before a second peer arrives.
func (r *DBAuthorizationRegistry) Release(uuid string) {
	if uuid == "" {
		return
	}
	r.pruneExpired(r.now().UnixMilli())
	_, _ = r.db.Exec(r.sql(`UPDATE relay_tickets SET claims = claims - 1 WHERE uuid = ? AND claims > 0`), uuid)
}

// RevokeForPeer invalidates all unpaired relay tickets involving peerID.
// Consumed tickets remain tombstoned until expiry so a UUID cannot be replayed.
func (r *DBAuthorizationRegistry) RevokeForPeer(peerID string) {
	if peerID == "" {
		return
	}
	now := r.now().UnixMilli()
	r.pruneExpired(now)

	tx, err := r.db.Begin()
	if err != nil {
		return
	}
	defer tx.Rollback()

	rows, err := tx.Query(r.sql(`SELECT uuid, expires_at FROM relay_tickets WHERE initiator_id = ? OR target_id = ?`), peerID, peerID)
	if err != nil {
		return
	}
	type ticketRef struct {
		uuid      string
		expiresAt int64
	}
	var refs []ticketRef
	for rows.Next() {
		var ref ticketRef
		if err := rows.Scan(&ref.uuid, &ref.expiresAt); err != nil {
			rows.Close()
			return
		}
		refs = append(refs, ref)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return
	}

	for _, ref := range refs {
		if _, err := tx.Exec(r.sql(`DELETE FROM relay_tickets WHERE uuid = ?`), ref.uuid); err != nil {
			return
		}
		expiresAt := ref.expiresAt
		if expiresAt <= now {
			expiresAt = now + config.RelayPairTimeout.Milliseconds()
		}
		if _, err := tx.Exec(r.sql(`INSERT INTO relay_ticket_used (uuid, expires_at) VALUES (?, ?)
			ON CONFLICT(uuid) DO UPDATE SET expires_at = excluded.expires_at`), ref.uuid, expiresAt); err != nil {
			return
		}
	}
	_ = tx.Commit()
}

// pruneExpired removes stale tickets and tombstones. It runs on every
// operation, mirroring the in-memory registry's prune-on-access behavior.
func (r *DBAuthorizationRegistry) pruneExpired(now int64) {
	_, _ = r.db.Exec(r.sql(`DELETE FROM relay_tickets WHERE expires_at <= ?`), now)
	_, _ = r.db.Exec(r.sql(`DELETE FROM relay_ticket_used WHERE expires_at <= ?`), now)
}
