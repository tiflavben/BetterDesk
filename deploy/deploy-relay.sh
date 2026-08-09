#!/usr/bin/env bash
# BetterDesk multi-relay cluster — RELAY node (intermediate, relay-only)
# Usage: RELAY_ID=1|2 MASTER_IP=... ./deploy-relay.sh  (run as root on each relay)
set -euo pipefail

RELAY_ID="${RELAY_ID:?set RELAY_ID=1 or 2}"
MASTER_IP="${MASTER_IP:?set MASTER_IP (control plane address)}"
PG_PASSWORD="${PG_PASSWORD:-change-me-strong}"
BINARY="/root/betterdesk-server-linux-amd64"
RELAY_PORT="${RELAY_PORT:-21117}"

echo "== [1/4] install binary =="
mkdir -p /opt/betterdesk /etc/betterdesk /var/lib/betterdesk
install -m 0755 "$BINARY" /opt/betterdesk/betterdesk-server
# shared cluster identity (copy from master: scp /etc/betterdesk/id_ed25519* root@relay:/etc/betterdesk/)
[ -f /etc/betterdesk/id_ed25519 ] || echo "WARN: /etc/betterdesk/id_ed25519 missing — copy from master!"

echo "== [2/4] systemd unit =="
cat > /etc/systemd/system/betterdesk-relay${RELAY_ID}.service <<EOF
[Unit]
Description=BetterDesk relay node ${RELAY_ID}
After=network.target

[Service]
Type=simple
User=root
Environment=RELAY_TICKET_STORE=db
ExecStart=/opt/betterdesk/betterdesk-server -mode relay \\
  -db "postgres://betterdesk:${PG_PASSWORD}@${MASTER_IP}:5432/betterdesk?sslmode=disable" \\
  -relay-port ${RELAY_PORT} \\
  -key-file /etc/betterdesk/id_ed25519
Restart=on-failure
RestartSec=3

[Install]
WantedBy=multi-user.target
EOF
systemctl daemon-reload
systemctl enable --now betterdesk-relay${RELAY_ID}

echo "== [3/4] verify =="
sleep 2
systemctl status betterdesk-relay${RELAY_ID} --no-pager -l | head -15 || true
ss -lntup | grep ${RELAY_PORT} || true

echo "== [4/4] ticket-store sanity (must show WARN auto-switch or explicit db) =="
journalctl -u betterdesk-relay${RELAY_ID} -n 30 --no-pager | grep -E "ticket store|WARN|relay" | tail -8 || true
echo "RELAY ${RELAY_ID} DONE."
