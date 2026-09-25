#!/bin/bash
set -euo pipefail
: "${SSH_PASSWORD:?}"
printf 'root:%s\n' "$SSH_PASSWORD" | chpasswd
mkdir -p /run/sshd /run/dbha /var/log/dbha
if [ ! -s /etc/machine-id ]; then hostname | md5sum | cut -d " " -f 1 > /etc/machine-id; fi
printf 'PermitRootLogin yes\nPasswordAuthentication yes\nUsePAM no\nHostKey /etc/ssh/ssh_host_ed25519_key\n' > /etc/ssh/sshd_config.d/dbha-lab.conf
/usr/sbin/sshd -D &
sshd_pid=$!
/usr/local/bin/docker-entrypoint.sh mysqld "$@" &
mysql_pid=$!
probe_pid=""
trap 'kill "$mysql_pid" "$sshd_pid" ${probe_pid:-} 2>/dev/null || true' TERM INT EXIT
if [ -f /etc/dbha/discovery.json ]; then
  /usr/local/bin/dbha-probe discover -c /etc/dbha/discovery.json &
  probe_pid=$!
else
  echo 'missing /etc/dbha/discovery.json' >&2
  exit 1
fi
wait -n "$mysql_pid" "$sshd_pid" "$probe_pid"
exit 1
