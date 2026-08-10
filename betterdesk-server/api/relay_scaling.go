package api

import (
	"database/sql"
	"net"
	"net/http"
	"time"

	"github.com/unitronix/betterdesk-server/db"
)

// relayScalingTelemetry reports live status of the configured relay tier
// (RELAY_SERVERS) plus shared-store traffic/ticket statistics. The web
// console's scaling page consumes this instead of its hard-coded stub.
func (s *Server) handleRelayScalingRelays(w http.ResponseWriter, r *http.Request) {
	relays := s.cfg.GetRelayServers()
	type relayStatus struct {
		Address        string `json:"address"`
		Status         string `json:"status"`
		LatencyMs      int64  `json:"latency_ms"`
		ActiveSessions int64  `json:"active_sessions"`
		TotalBytes     int64  `json:"total_bytes"`
	}
	// Per-node heartbeat rows (relay-only nodes upsert every 10s).
	var heartbeats map[string]db.RelayHeartbeat
	if s.relayTicketDB != nil {
		if hb, err := db.GetAllRelayHeartbeats(s.relayTicketDB); err == nil {
			heartbeats = hb
		}
	}
	matchHeartbeat := func(addr string) (int64, int64) {
		ip := addr
		if h, _, err := net.SplitHostPort(addr); err == nil {
			ip = h
		}
		for _, hb := range heartbeats {
			hbIP := hb.Addr
			if h, _, err := net.SplitHostPort(hb.Addr); err == nil {
				hbIP = h
			}
			if hbIP == ip {
				return hb.ActiveSessions, hb.TotalBytes
			}
		}
		return 0, 0
	}
	out := make([]relayStatus, 0, len(relays))
	for _, addr := range relays {
		rs := relayStatus{Address: addr}
		start := time.Now()
		conn, err := net.DialTimeout("tcp", addr, 1500*time.Millisecond)
		if err != nil {
			rs.Status = "offline"
		} else {
			conn.Close()
			rs.Status = "online"
			rs.LatencyMs = time.Since(start).Milliseconds()
		}
		rs.ActiveSessions, rs.TotalBytes = matchHeartbeat(addr)
		out = append(out, rs)
	}
	if len(out) == 0 {
		// No RELAY_SERVERS configured (single-node deployment): leave the
		// list empty and let the console show its configured state.
		resp := map[string]any{"relays": []relayStatus{}}
		writeJSON(w, http.StatusOK, resp)
		return
	}

	resp := map[string]any{
		"relays": out,
	}
	if s.relayTicketDB != nil {
		active := 0
		if err := s.relayTicketDB.QueryRow(`SELECT COUNT(*) FROM relay_tickets`).Scan(&active); err == nil {
			resp["active_tickets"] = active
		}
		var traffic int64
		if err := s.relayTicketDB.QueryRow(`SELECT COALESCE(SUM(bytes),0) FROM relay_traffic`).Scan(&traffic); err == nil {
			resp["total_traffic_bytes"] = traffic
		}
		var activeSessions int64
		if err := s.relayTicketDB.QueryRow(
			`SELECT COUNT(*) FROM relay_traffic WHERE updated_at >= datetime('now','-60 seconds')`).Scan(&activeSessions); err == nil {
			resp["active_sessions_approx"] = activeSessions
		}
	}
	writeJSON(w, http.StatusOK, resp)
}

// SetRelayTicketDB wires the shared ticket/traffic store for telemetry.
func (s *Server) SetRelayTicketDB(db *sql.DB) {
	s.relayTicketDB = db
}
