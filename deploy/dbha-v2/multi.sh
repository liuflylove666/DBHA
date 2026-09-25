#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")"
compose=(docker compose --env-file generated-multi/.env -f generated-multi/compose.json)
case "${1:-}" in
  up)
    python3 multicluster.py
    if [ -n "${DBHA_LOCAL_GO:-}" ]; then
      mkdir -p .build
      ( cd ../.. && CGO_ENABLED=0 GOOS=linux GOARCH=amd64 "$DBHA_LOCAL_GO" build -trimpath -o deploy/dbha-v2/.build/dbha-server ./dbm-services/common/dbha-v2/cmd/server )
      ( cd ../.. && CGO_ENABLED=0 GOOS=linux GOARCH=amd64 "$DBHA_LOCAL_GO" build -trimpath -o deploy/dbha-v2/.build/dbha-probe ./dbm-services/common/dbha-v2/cmd/probe )
      docker build --platform linux/amd64 -f Dockerfile.prebuilt -t dbha-v2-lab-runtime:local ../..
    else
      docker build --platform linux/amd64 -f Dockerfile -t dbha-v2-lab-runtime:local ../..
    fi
    docker build --platform linux/amd64 -f nodes/Dockerfile.mysql -t dbha-v2-lab-mysql:local nodes
    docker build --platform linux/amd64 -f nodes/Dockerfile.proxy -t dbha-v2-lab-proxy:local nodes
    "${compose[@]}" up -d --wait --wait-timeout 180 --remove-orphans etcd dbha-server
    docker run --rm --platform linux/amd64 --network dbha-v2-multi-lab_lab --entrypoint python3 \
      -v "$PWD/generated-multi:/data" -v "$PWD/install.py:/install.py:ro" -v "$PWD/configure.py:/configure.py:ro" \
      dbha-v2-lab-proxy:local /install.py --output /data --server http://10.203.81.12:8080
    python3 smoke_multi.py --guard-initial-primary
    for group in cluster1 cluster2 cluster3; do
      "${compose[@]}" up -d "$group-mysql-master" "$group-mysql-standby"
      "${compose[@]}" up -d "$group-bootstrap"
    done
    "${compose[@]}" up -d cluster1-proxy1 cluster1-proxy2 cluster2-proxy1 cluster2-proxy2 cluster3-proxy1 cluster3-proxy2
    python3 smoke_multi.py --record-initial
    ;;
  check) python3 smoke_multi.py ;;
  enable-switching) python3 smoke_multi.py --set-switching true ;;
  disable-switching) python3 smoke_multi.py --set-switching false ;;
  failover-test) python3 smoke_multi.py --failover ;;
  stop) "${compose[@]}" stop ;;
  *) echo "Usage: $0 {up|check|enable-switching|disable-switching|failover-test|stop}" >&2; exit 2 ;;
esac
