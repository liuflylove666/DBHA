#!/bin/bash
set -euo pipefail
: "${PROXY_ADMIN_USER:?}"
: "${PROXY_ADMIN_PASSWORD:?}"
: "${METADATA_URL:?}"
: "${API_TOKEN:?}"
for value in "$PROXY_ADMIN_USER" "$PROXY_ADMIN_PASSWORD"; do
  [[ "$value" =~ ^[a-zA-Z0-9_]+$ ]] || { echo 'Invalid admin credential format' >&2; exit 1; }
done
backend=$(python3 - <<'PY'
import ipaddress, json, os, urllib.request
payload = {'db_cloud_token': os.environ['API_TOKEN'], 'bk_cloud_id': 0, 'addresses': ['research-mysql.local']}
request = urllib.request.Request(os.environ['METADATA_URL'], data=json.dumps(payload).encode(), headers={'Content-Type': 'application/json'})
with urllib.request.urlopen(request, timeout=10) as response:
    result = json.load(response)
if result.get('code') != 0:
    raise SystemExit('Metadata lookup failed')
masters = [row for row in result['data'] if row['instance_role'] == 'backend_master' and row['status'] in ('running', 'available')]
if len(masters) != 1:
    raise SystemExit('Expected exactly one available master')
master = masters[0]
ipaddress.IPv4Address(master['ip'])
port = int(master['port'])
if not 0 < port < 65536:
    raise SystemExit('Invalid backend port')
print(f"{master['ip']}:{port}")
PY
)
umask 077
mkdir -p /run/mysql-proxy /var/log/mysql-proxy
printf 'app@%%\nroot@%%\ndbha@%%\n' > /run/mysql-proxy/users.cnf
cat > /run/mysql-proxy/proxy.cnf <<CONF
[mysql-proxy]
basedir = /opt/mysql-proxy
plugin-dir = /opt/mysql-proxy/lib/mysql-proxy/plugins
plugins = proxy,admin
admin-lua-script = /opt/mysql-proxy/lib/mysql-proxy/lua/admin.lua
admin-users-file = /run/mysql-proxy/users.cnf
admin-address = 0.0.0.0:11000
admin-username = ${PROXY_ADMIN_USER}
admin-password = ${PROXY_ADMIN_PASSWORD}
proxy-address = 0.0.0.0:10000
proxy-backend-addresses = ${backend}
pid-file = /run/mysql-proxy/proxy.pid
daemon = true
log-file = /var/log/mysql-proxy/proxy.log
log-level = info
CONF
rm -f /run/mysql-proxy/proxy.pid
/opt/mysql-proxy/bin/mysql-proxy --defaults-file=/run/mysql-proxy/proxy.cnf
for attempt in {1..50}; do
  [ -s /run/mysql-proxy/proxy.pid ] && break
  sleep 0.1
done
proxy_pid=$(cat /run/mysql-proxy/proxy.pid)
trap 'kill "$proxy_pid" 2>/dev/null || true' TERM INT EXIT
while kill -0 "$proxy_pid" 2>/dev/null; do
  [ "$(awk '{print $3}' /proc/"$proxy_pid"/stat)" != Z ] || break
  sleep 1
done
exit 1
