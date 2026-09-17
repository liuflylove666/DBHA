#!/bin/bash
set -euo pipefail
: "${PROXY_ADMIN_USER:?}"
: "${PROXY_ADMIN_PASSWORD:?}"
: "${SSH_PASSWORD:?}"
: "${METADATA_URL:?}"
: "${API_TOKEN:?}"
# Resolve the current master from durable metadata on every start. A restarted
# Proxy must not route back to the pre-failover master.
MYSQL_MASTER=$(python3 - <<'PY'
import json, os, urllib.request
cluster = os.environ.get('METADATA_CLUSTER_ADDRESS', 'research-mysql.local')
req = urllib.request.Request(os.environ['METADATA_URL'], data=json.dumps({'db_cloud_token': os.environ['API_TOKEN'], 'bk_cloud_id': 0, 'addresses': [cluster]}).encode(), headers={'Content-Type': 'application/json'})
with urllib.request.urlopen(req, timeout=10) as response:
    result = json.load(response)
if result.get('code') != 0:
    raise SystemExit('metadata lookup failed')
rows = result['data']
masters = [r for r in rows if r.get('cluster') == cluster and r.get('instance_role') == 'backend_master' and r.get('status') in ('running', 'available')]
if len(masters) != 1:
    raise SystemExit('Expected exactly one available master; refusing to route')
print(f"{masters[0]['ip']}:{masters[0]['port']}")
PY
)
printf 'app@%%\nroot@%%\ndbha@%%\n' > /tmp/proxy-users.cnf
cat > /tmp/mysql-proxy.cnf <<EOF
[mysql-proxy]
basedir = /opt/mysql-proxy
plugin-dir = /opt/mysql-proxy/lib/mysql-proxy/plugins
admin-lua-script = /opt/mysql-proxy/lib/mysql-proxy/lua/admin.lua
admin-users-file = /tmp/proxy-users.cnf
proxy-address = 0.0.0.0:10000
admin-address = 0.0.0.0:11000
admin-username = ${PROXY_ADMIN_USER}
admin-password = ${PROXY_ADMIN_PASSWORD}
proxy-backend-addresses = ${MYSQL_MASTER}
pid-file = /run/dbha/mysql-proxy.pid
daemon = true
log-file = /var/log/dbha/mysql-proxy.log
log-level = info
plugins = proxy,admin
EOF
chmod 600 /tmp/mysql-proxy.cnf
mkdir -p /run/sshd /run/dbha /var/log/dbha
if [ ! -s /etc/machine-id ]; then hostname | md5sum | cut -d " " -f 1 > /etc/machine-id; fi
ssh-keygen -A
printf 'root:%s\n' "$SSH_PASSWORD" | chpasswd
printf 'PermitRootLogin yes\nPasswordAuthentication yes\nUsePAM no\n' > /etc/ssh/sshd_config.d/dbha-lab.conf
/usr/sbin/sshd -D &
sshd_pid=$!
rm -f /run/dbha/mysql-proxy.pid
/opt/mysql-proxy/bin/mysql-proxy --defaults-file=/tmp/mysql-proxy.cnf
for attempt in {1..50}; do
  [ -s /run/dbha/mysql-proxy.pid ] && break
  sleep 0.1
done
proxy_pid=$(cat /run/dbha/mysql-proxy.pid)
# This vendor binary daemonizes; monitor its PID alongside sshd and probe.
(while kill -0 "$proxy_pid" 2>/dev/null; do
  [ "$(awk '{print $3}' /proc/"$proxy_pid"/stat)" != Z ] || break
  sleep 1
done; exit 1) &
monitor_pid=$!
/usr/local/bin/dbha-probe -c /etc/dbha/probe.yaml &
probe_pid=$!
trap 'kill "$proxy_pid" "$sshd_pid" "$probe_pid" 2>/dev/null || true' TERM INT EXIT
wait -n "$monitor_pid" "$sshd_pid" "$probe_pid"
exit 1
