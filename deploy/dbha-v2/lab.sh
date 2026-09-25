#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")"
compose=(docker compose --env-file generated/.env)
case "${1:-}" in
  up)
    python3 configure.py
    mkdir -p -m 0700 backups
    chmod 0700 backups
    if [ -n "${DBHA_LOCAL_GO:-}" ]; then
      mkdir -p .build
      ( cd ../.. && CGO_ENABLED=0 GOOS=linux GOARCH=amd64 "$DBHA_LOCAL_GO" build -trimpath -o deploy/dbha-v2/.build/dbha-server ./dbm-services/common/dbha-v2/cmd/server )
      ( cd ../.. && CGO_ENABLED=0 GOOS=linux GOARCH=amd64 "$DBHA_LOCAL_GO" build -trimpath -o deploy/dbha-v2/.build/dbha-probe ./dbm-services/common/dbha-v2/cmd/probe )
      docker build --platform linux/amd64 -f Dockerfile.prebuilt -t dbha-v2-lab-runtime:local ../..
    else
      docker build --platform linux/amd64 -f Dockerfile -t dbha-v2-lab-runtime:local ../..
    fi
    "${compose[@]}" build mysql-master proxy1
    "${compose[@]}" up -d --wait --wait-timeout 180 etcd dbha-server
    docker run --rm --platform linux/amd64 --network dbha-v2-lab_lab --entrypoint python3 \
      -v "$PWD/generated:/data" -v "$PWD/install.py:/install.py:ro" -v "$PWD/configure.py:/configure.py:ro" \
      dbha-v2-lab-proxy:local /install.py --output /data --server http://10.203.80.12:8080
    python3 smoke.py --guard-initial-primary
    "${compose[@]}" up -d mysql-master mysql-standby
    "${compose[@]}" up -d bootstrap
    "${compose[@]}" up -d proxy1 proxy2
    python3 smoke.py --record-initial
    ;;
  check) python3 smoke.py ;;
  enable-switching) python3 smoke.py --set-switching true ;;
  disable-switching) python3 smoke.py --set-switching false ;;
  failover-test) python3 smoke.py --failover ;;
  stop) "${compose[@]}" stop ;;
  *) echo "Usage: $0 {up|check|enable-switching|disable-switching|failover-test|stop}" >&2; exit 2 ;;
esac
