#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")"
case "${1:-}" in
  up)
    python3 configure.py
    docker build --platform linux/amd64 -f Dockerfile -t dbha-v2-lab-runtime:local ../..
    docker compose build mysql-master proxy1
    docker compose up -d metadata-store
    python3 - <<'PY'
from smoke import eventually, primary
if eventually(primary) != "10.203.80.21":
    raise SystemExit("Topology has already switched. Refusing to restart the old master; use targeted service commands after recovery planning.")
PY
    docker compose up -d
    ;;
  check) python3 smoke.py ;;
  enable-switching)
    python3 configure.py --enable-switching
    docker compose up -d --no-deps --force-recreate analysis
    ;;
  disable-switching)
    python3 configure.py
    docker compose up -d --no-deps --force-recreate analysis
    ;;
  failover-test) python3 smoke.py --failover ;;
  stop) docker compose stop ;;
  *) echo "Usage: $0 {up|check|enable-switching|disable-switching|failover-test|stop}" >&2; exit 2 ;;
esac
