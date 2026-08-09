package relay

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib" // registers the "pgx" database/sql driver
	_ "modernc.org/sqlite"
)

// Compile-time assertion: the DB-backed store satisfies the AuthorizationStore
// interface, so relay.Server can use it interchangeably with the in-memory one.
var _ AuthorizationStore = (*DBAuthorizationRegistry)(nil)

// newRelayTicketDBs opens two independent *sql.DB handles on the same SQLite
// file, simulating two server processes sharing one ticket store.
func newRelayTicketDBs(t *testing.T) (*sql.DB, *sql.DB) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "relay-tickets.db")
	open := func() *sql.DB {
		dsn := fmt.Sprintf("file:%s?_journal_mode=WAL&_busy_timeout=5000&_foreign_keys=ON", path)
		db, err := sql.Open("sqlite", dsn)
		if err != nil {
			t.Fatalf("open sqlite ticket store: %v", err)
		}
		db.SetMaxOpenConns(1) // SQLite single-writer per handle
		t.Cleanup(func() { db.Close() })
		return db
	}
	return open(), open()
}

func newRelayTicketRegistries(t *testing.T) (*DBAuthorizationRegistry, *DBAuthorizationRegistry) {
	t.Helper()
	db1, db2 := newRelayTicketDBs(t)
	r1, err := NewDBAuthorizationRegistry(db1)
	if err != nil {
		t.Fatalf("NewDBAuthorizationRegistry(db1): %v", err)
	}
	r2, err := NewDBAuthorizationRegistry(db2)
	if err != nil {
		t.Fatalf("NewDBAuthorizationRegistry(db2): %v", err)
	}
	// Wiring regression guard: a real SQLite handle must never be detected as
	// PostgreSQL (see TestIsPostgresDriver — %T string matching misdetects it).
	if r1.pg || r2.pg {
		t.Fatalf("NewDBAuthorizationRegistry on SQLite must set pg=false, got r1.pg=%v r2.pg=%v", r1.pg, r2.pg)
	}
	return r1, r2
}

func TestDBAuthorizationRegistrySharedClaims(t *testing.T) {
	r1, r2 := newRelayTicketRegistries(t)

	// Signal-side authorize on "instance 1".
	if !r1.Authorize("shared-uuid", "INIT01", "TARGET1") {
		t.Fatal("expected ticket authorization")
	}
	// Both relay instances see the same ticket through the shared store.
	if !r1.Claim("shared-uuid") {
		t.Fatal("expected first claim via db1")
	}
	if !r2.Claim("shared-uuid") {
		t.Fatal("expected second claim via db2 (shared store)")
	}
	// Third claim must fail after the ticket was consumed.
	if r2.Claim("shared-uuid") {
		t.Fatal("third claim must be rejected after ticket consumption")
	}
}

func TestDBAuthorizationRegistryConcurrentClaims(t *testing.T) {
	r1, r2 := newRelayTicketRegistries(t)
	if !r1.Authorize("race-uuid", "INIT02", "TARGET2") {
		t.Fatal("expected ticket authorization")
	}

	const attempts = 20
	successes := make([]bool, attempts)
	var wg sync.WaitGroup
	for i := 0; i < attempts; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			r := r1
			if i%2 == 1 {
				r = r2
			}
			successes[i] = r.Claim("race-uuid")
		}(i)
	}
	wg.Wait()

	got := 0
	for _, ok := range successes {
		if ok {
			got++
		}
	}
	if got != 2 {
		t.Fatalf("exactly 2 concurrent claims must succeed, got %d", got)
	}
}

func TestDBAuthorizationRegistryReplayProtection(t *testing.T) {
	r1, r2 := newRelayTicketRegistries(t)
	if !r1.Authorize("replay-uuid", "INIT03", "TARGET3") {
		t.Fatal("expected ticket authorization")
	}
	if !r1.Claim("replay-uuid") || !r2.Claim("replay-uuid") {
		t.Fatal("expected both claims to succeed")
	}

	// Consumed UUID must not be re-authorizable while tombstoned.
	if r1.Authorize("replay-uuid", "INIT03", "TARGET3") {
		t.Fatal("consumed UUID must not be re-authorized before tombstone expiry")
	}
	// A different pair must also be rejected (relay replay attempt).
	if r2.Authorize("replay-uuid", "EVIL04", "TARGET3") {
		t.Fatal("consumed UUID must not be re-authorized by a different pair")
	}
}

func TestDBAuthorizationRegistryExpiry(t *testing.T) {
	r1, r2 := newRelayTicketRegistries(t)
	if !r1.Authorize("expiry-uuid", "INIT05", "TARGET5") {
		t.Fatal("expected ticket authorization")
	}

	// Backdate the ticket into the past; expired tickets are pruned on access.
	if _, err := r1.db.Exec(`UPDATE relay_tickets SET expires_at = ? WHERE uuid = ?`, time.Now().UnixMilli()-1000, "expiry-uuid"); err != nil {
		t.Fatalf("backdate ticket: %v", err)
	}
	if r2.Claim("expiry-uuid") {
		t.Fatal("expired ticket must not be claimable")
	}
	// The same UUID becomes usable again once the stale ticket expired.
	if !r1.Authorize("expiry-uuid", "INIT05", "TARGET5") {
		t.Fatal("expired ticket must be re-authorizable")
	}
}

func TestDBAuthorizationRegistryRelease(t *testing.T) {
	r1, r2 := newRelayTicketRegistries(t)
	if !r1.Authorize("release-uuid", "INIT06", "TARGET6") {
		t.Fatal("expected ticket authorization")
	}
	if !r1.Claim("release-uuid") {
		t.Fatal("expected first claim")
	}
	// Release frees one claim slot so a second peer can still connect.
	r2.Release("release-uuid")
	if !r2.Claim("release-uuid") {
		t.Fatal("expected claim after release")
	}
	if !r2.Claim("release-uuid") {
		t.Fatal("expected second claim after release")
	}
	if r2.Claim("release-uuid") {
		t.Fatal("third claim must fail after both slots consumed")
	}
}

func TestDBAuthorizationRegistryRevokesPeerTickets(t *testing.T) {
	r1, r2 := newRelayTicketRegistries(t)
	if !r1.Authorize("ban-uuid", "BANNED1", "TARGET1") {
		t.Fatal("expected ticket authorization")
	}
	r2.RevokeForPeer("BANNED1")
	if r1.Claim("ban-uuid") {
		t.Fatal("revoked peer ticket must not be claimable")
	}
	// Revoked tickets are tombstoned: the UUID cannot be replayed.
	if r1.Authorize("ban-uuid", "BANNED1", "TARGET1") {
		t.Fatal("revoked UUID must not be re-authorizable")
	}
}

func TestDBAuthorizationRegistryPlaceholderConversion(t *testing.T) {
	pg := &DBAuthorizationRegistry{pg: true}
	got := pg.sql(`INSERT INTO t (a, b) VALUES (?, ?) ON CONFLICT(uuid) DO UPDATE SET b = excluded.b`)
	want := `INSERT INTO t (a, b) VALUES ($1, $2) ON CONFLICT(uuid) DO UPDATE SET b = excluded.b`
	if got != want {
		t.Fatalf("pg placeholder conversion:\n got %q\nwant %q", got, want)
	}

	sqlite := &DBAuthorizationRegistry{pg: false}
	if got := sqlite.sql(`SELECT ? FROM t WHERE uuid = ?`); got != `SELECT ? FROM t WHERE uuid = ?` {
		t.Fatalf("sqlite pass-through expected unchanged SQL, got %q", got)
	}
}

// TestIsPostgresDriver exercises the driver-detection helper — the logic
// that was previously untested and silently broken: string-matching %T
// output can never match "pgx" because the registered pgx database/sql
// driver's dynamic type is *stdlib.Driver (package-qualified name contains
// no "pgx").
func TestIsPostgresDriver(t *testing.T) {
	// pgx stdlib driver instance: constructing one directly is not possible
	// (unexported fields), so assert via the registered driver lookup used by
	// database/sql for the "pgx" name.
	sqlDB, err := sql.Open("pgx", "postgres://user:pass@127.0.0.1:1/none?connect_timeout=1")
	if err != nil {
		t.Fatalf("sql.Open(pgx): %v", err)
	}
	defer sqlDB.Close()
	// sql.Open is lazy: Driver() returns the registered instance without
	// establishing a connection, so no live PostgreSQL server is required.
	if !isPostgresDriver(sqlDB) {
		t.Fatal("isPostgresDriver(pgx *sql.DB) = false, want true — driver detection is broken")
	}

	// Genuine SQLite handle must not be detected as PostgreSQL.
	db1, db2 := newRelayTicketDBs(t)
	if isPostgresDriver(db1) || isPostgresDriver(db2) {
		t.Fatal("isPostgresDriver(SQLite *sql.DB) = true, want false")
	}

	// Nil safety.
	if isPostgresDriver(nil) {
		t.Fatal("isPostgresDriver(nil) = true, want false")
	}
}
