#!/usr/bin/env bash
# BetterDesk multi-relay cluster — MASTER node (signal + API + PostgreSQL)
# Topology: 1 master (control plane) + 2 relay nodes (intermediate)
# Usage: set vars below (or env), run as root on the master node.
set -euo pipefail

# ====== EDIT THESE ======
MASTER_IP="${MASTER_IP:-}"                  # this node's public/LAN IP
RELAY1_ADDR="${RELAY1_ADDR:-}"              # e.g. 1.2.3.4:21117
RELAY2_ADDR="${RELAY2_ADDR:-}"              # e.g. 5.6.7.8:21117
PG_PASSWORD="${PG_PASSWORD:-change-me-strong}"
BINARY="/root/betterdesk-server-linux-amd64"
# ========================

[ -n "$MASTER_IP" ] || { echo "MASTER_IP required"; exit 1; }
[ -n "$RELAY1_ADDR" ] && [ -n "$RELAY2_ADDR" ] || { echo "RELAY1_ADDR and RELAY2_ADDR required"; exit 1; }

echo "== [1/5] install PostgreSQL =="
if ! command -v psql >/dev/null; then
  apt-get update -y && apt-get install -y postgresql postgresql-contrib
fi
systemctl enable --now postgresql

echo "== [2/5] create DB + user =="
su - postgres -c "psql -c \"CREATE USER betterdesk WITH PASSWORD '$PG_PASSWORD';\" 2>/dev/null || true"
su - postgres -c "psql -c 'CREATE DATABASE betterdesk OWNER betterdesk;' 2>/dev/null || true"
su - postgres -c "psql -c 'GRANT ALL PRIVILEGES ON DATABASE betterdesk TO betterdesk;' 2>/dev/null || true"

echo "== [3/5] install binary + identity =="
mkdir -p /opt/betterdesk /etc/betterdesk /var/lib/betterdesk
install -m 0755 "$BINARY" /opt/betterdesk/betterdesk-server
[ -f /etc/betterdesk/id_ed25519 ] || /opt/betterdesk/betterdesk-server -key-file /etc/betterdesk/id_ed25519 -mode signal -help >/dev/null 2>&1 || true
# (binary generates the key on first real start; copy id_ed25519* to relay nodes)

echo "== [4/5] systemd unit (Go signal + API) =="
cat > /etc/systemd/system/betterdesk-master.service <<EOF
[Unit]
Description=BetterDesk master (signal + API + PG ticket store)
After=network.target postgresql.service
Wants=postgresql.service

[Service]
Type=simple
User=root
Environment=RELAY_SERVERS=${RELAY1_ADDR},${RELAY2_ADDR}
Environment=RELAY_TICKET_STORE=db
Environment=ENROLLMENT_MODE=open
ExecStart=/opt/betterdesk/betterdesk-server -mode signal \\
  -db "postgres://betterdesk:${PG_PASSWORD}@127.0.0.1:5432/betterdesk?sslmode=disable" \\
  -key-file /etc/betterdesk/id_ed25519
Restart=on-failure
RestartSec=3

[Install]
WantedBy=multi-user.target
EOF
systemctl daemon-reload
systemctl enable --now betterdesk-master

echo "== [5/6] verify (Go) =="
sleep 2
systemctl status betterdesk-master --no-pager -l | head -15 || true
ss -lntup | grep -E '2111[4568]|5432' || true
su - postgres -c "psql -d betterdesk -c '\\dt relay_*'" 2>/dev/null || true

echo "== [6/6] Web console panel (:5000, optional — set WITH_PANEL=1) =="
if [ "${WITH_PANEL:-0}" = "1" ]; then
  # Node 24 + native build deps (better-sqlite3 needs a toolchain)
  if ! command -v node >/dev/null || [ "$(node -v | cut -d. -f1 | tr -d v)" -lt 22 ]; then
    curl -fsSL https://deb.nodesource.com/setup_24.x | bash -
    apt-get install -y nodejs build-essential python3
  fi
  mkdir -p /opt/betterdesk-console
  # Web console sources must be deployed separately (scp -r web-nodejs/* root@master:/opt/betterdesk-console/)
  if [ -f /opt/betterdesk-console/package.json ]; then
    cd /opt/betterdesk-console
    npm ci --omit=dev 2>/dev/null || npm install --omit=dev
    cat > /etc/systemd/system/betterdesk-console.service <<EOF
[Unit]
Description=BetterDesk web console
After=network.target betterdesk-master.service
Wants=betterdesk-master.service

[Service]
Type=simple
User=root
WorkingDirectory=/opt/betterdesk-console
Environment=PORT=5000
Environment=HOST=0.0.0.0
Environment=BETTERDESK_API_URL=http://127.0.0.1:21114/api
ExecStart=/usr/bin/node server.js
Restart=on-failure
RestartSec=3

[Install]
WantedBy=multi-user.target
EOF
    systemctl daemon-reload
    systemctl enable --now betterdesk-console
    sleep 3
    systemctl status betterdesk-console --no-pager -l | head -10 || true
    ss -lntup | grep 5000 || true
    echo "PANEL DONE (web-nodejs sources must be at /opt/betterdesk-console)"
  else
    echo "SKIP: /opt/betterdesk-console/package.json missing — deploy web-nodejs sources first"
  fi
else
  echo "SKIP: WITH_PANEL=0 (set WITH_PANEL=1 to install the Node.js console)"
fi

echo
echo "MASTER DONE. Copy identity to relay nodes:"
echo "  scp /etc/betterdesk/id_ed25519* root@<relay>:/etc/betterdesk/"
