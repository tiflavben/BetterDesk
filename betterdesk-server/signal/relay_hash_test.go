package signal

import (
	"fmt"
	"path/filepath"
	"testing"

	"github.com/unitronix/betterdesk-server/config"
	"github.com/unitronix/betterdesk-server/db"
)

func newRelayHashTestServer(t *testing.T, relayServers string) *Server {
	t.Helper()
	database, err := db.OpenSQLite(filepath.Join(t.TempDir(), "relay-hash-test.db"))
	if err != nil {
		t.Fatalf("OpenSQLite: %v", err)
	}
	t.Cleanup(func() { database.Close() })
	cfg := config.DefaultConfig()
	cfg.RelayServers = relayServers
	return New(cfg, nil, database)
}

func TestGetRelayServerForSingleRelay(t *testing.T) {
	srv := newRelayHashTestServer(t, "relay1.example.com:21117")
	for i := 0; i < 5; i++ {
		if got := srv.getRelayServerFor("INIT01", "TARGET1"); got != "relay1.example.com:21117" {
			t.Fatalf("single relay: expected relay1.example.com:21117, got %q", got)
		}
	}
}

func TestGetRelayServerForNoConfiguredRelay(t *testing.T) {
	srv := newRelayHashTestServer(t, "")
	// No relay servers configured: falls back to the auto-detect default.
	want := srv.getRelayServer()
	if got := srv.getRelayServerFor("INIT01", "TARGET1"); got != want {
		t.Fatalf("no configured relays: expected %q, got %q", want, got)
	}
}

func TestGetRelayServerForConsistentHashing(t *testing.T) {
	srv := newRelayHashTestServer(t, "r1.example.com:21117,r2.example.com:21117,r3.example.com:21117,r4.example.com:21117")
	relays := []string{
		"r1.example.com:21117",
		"r2.example.com:21117",
		"r3.example.com:21117",
		"r4.example.com:21117",
	}
	inSet := func(r string) bool {
		for _, cand := range relays {
			if cand == r {
				return true
			}
		}
		return false
	}

	// The same pair is stable across repeated calls.
	first := srv.getRelayServerFor("INIT01", "TARGET1")
	if !inSet(first) {
		t.Fatalf("relay %q not in configured set", first)
	}
	for i := 0; i < 10; i++ {
		if got := srv.getRelayServerFor("INIT01", "TARGET1"); got != first {
			t.Fatalf("unstable hash: got %q want %q", got, first)
		}
	}

	// (A,B) and (B,A) resolve to the same instance (canonical ordering).
	if ab := srv.getRelayServerFor("INIT01", "TARGET1"); ab != srv.getRelayServerFor("TARGET1", "INIT01") {
		t.Fatalf("(A,B) and (B,A) must resolve identically: %q vs %q", ab, srv.getRelayServerFor("TARGET1", "INIT01"))
	}

	// Distribution: across many distinct pairs at least two instances appear.
	seen := map[string]bool{}
	for i := 0; i < 200; i++ {
		a := fmt.Sprintf("peer-a-%d", i)
		b := fmt.Sprintf("peer-b-%d", i)
		seen[srv.getRelayServerFor(a, b)] = true
	}
	if len(seen) < 2 {
		t.Fatalf("expected at least 2 distinct relays across pairs, got %d", len(seen))
	}
}
