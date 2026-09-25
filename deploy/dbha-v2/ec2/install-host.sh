#!/usr/bin/env bash
# Run on each EC2 after distributing only that host's generated directory.
set -euo pipefail
role=${1:?Usage: install-host.sh controller[N]|mysql1|mysql2|proxy1|proxy2 CONFIG_DIRECTORY}
source_dir=${2:?CONFIG_DIRECTORY required}
case "$role" in
  controller|mysql1|mysql2|proxy1|proxy2) ;;
  controller*) [[ "${role#controller}" =~ ^[1-9][0-9]*$ ]] || exit 2 ;;
  *) exit 2 ;;
esac
[ "$(id -u)" = 0 ] || { echo "run as root" >&2; exit 1; }
command -v install >/dev/null
id dbha >/dev/null 2>&1 || useradd --system --create-home --home-dir /var/lib/dbha --shell /bin/bash dbha
install -d -o dbha -g dbha -m 0700 /etc/dbha /var/log/dbha
if [[ "$role" = controller* ]]; then
  for name in server.json admin.token ssh_known_hosts; do
    install -o dbha -g dbha -m 0600 "$source_dir/$name" "/etc/dbha/$name"
  done
  install -o root -g root -m 0600 "$source_dir/etcd.env" /etc/dbha/etcd.env
  for name in bootstrap-replication.sql root-client.cnf app-client.cnf; do
    [ ! -f "$source_dir/$name" ] || install -o root -g root -m 0600 "$source_dir/$name" "/etc/dbha/$name"
  done
  install -d -o dbha -g dbha -m 0700 /srv/dbha/server
  install -d -o root -g root -m 0700 /srv/dbha/etcd
  for name in dbha-etcd.service dbha-server.service; do
    install -m 0644 "$(dirname "$0")/systemd/$name" "/etc/systemd/system/$name"
  done
else
  for name in discovery.json agent.token dbha.password proxy-admin.password; do
    install -o dbha -g dbha -m 0600 "$source_dir/$name" "/etc/dbha/$name"
  done
  install -d -o dbha -g dbha -m 0700 /srv/dbha/probe
  install -d -o dbha -g dbha -m 0750 /run/dbha
  install -m 0644 "$(dirname "$0")/systemd/dbha-probe.service" /etc/systemd/system/
  if [[ "$role" = mysql* ]]; then
    install -o root -g root -m 0600 "$source_dir/mysql.env" /etc/dbha/mysql.env
    install -o root -g root -m 0644 "$source_dir/mysql.cnf" /etc/dbha/mysql.cnf
    install -d -o root -g root -m 0700 /etc/dbha/mysql-init /srv/dbha/mysql
    install -o root -g root -m 0600 "$source_dir/mysql-init/10-accounts.sql" /etc/dbha/mysql-init/10-accounts.sql
    install -m 0644 "$(dirname "$0")/systemd/dbha-mysql.service" /etc/systemd/system/
  else
    install -o root -g root -m 0600 "$source_dir/proxy.env" /etc/dbha/proxy.env
    install -m 0644 "$(dirname "$0")/systemd/dbha-proxy.service" /etc/systemd/system/
  fi
fi
systemctl daemon-reload
echo "$role configuration installed; no service has been started."
