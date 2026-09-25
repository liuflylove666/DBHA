#!/usr/bin/env bash
# Snapshot management etcd plus the independent server rollback watermark.
set -euo pipefail
cd "$(dirname "$0")"
umask 077
backup_dir="$(pwd)/backups"
mkdir -p -m 0700 "$backup_dir"
compose=(docker compose --env-file generated/.env)
etcd_id=$("${compose[@]}" ps -q etcd)
server_id=$("${compose[@]}" ps -q dbha-server)
[ -n "$etcd_id" ] && [ -n "$server_id" ] || { echo "etcd and dbha-server must be running" >&2; exit 1; }
stamp=$(date -u +%Y%m%dT%H%M%SZ)
snapshot="$backup_dir/$stamp.db"
watermark="$backup_dir/$stamp.watermark.json"
docker exec "$etcd_id" /usr/local/bin/etcdctl --endpoints=http://127.0.0.1:2379 snapshot save "/backups/$stamp.db"
docker exec "$etcd_id" /usr/local/bin/etcdutl snapshot status "/backups/$stamp.db" -w json >/dev/null
chmod 0600 "$snapshot"
if ! docker exec "$server_id" cat /var/lib/dbha-server/control-watermark.json > "$watermark"; then
  rm -f "$snapshot" "$watermark"
  echo "control watermark unavailable; snapshot not retained" >&2
  exit 1
fi
chmod 0600 "$watermark"
shasum -a 256 "$snapshot" "$watermark" > "$backup_dir/$stamp.sha256"
chmod 0600 "$backup_dir/$stamp.sha256"
python3 - "$backup_dir" <<'PY'
from pathlib import Path
import sys
directory = Path(sys.argv[1])
snapshots = sorted(directory.glob("*.db"), reverse=True)
for path in snapshots[7:]:
    base = path.name[:-3]
    for suffix in (".db", ".watermark.json", ".sha256"):
        (directory / (base + suffix)).unlink(missing_ok=True)
PY
echo "Snapshot retained: $snapshot (seven newest backups kept)"
