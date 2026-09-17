#!/bin/bash
set -euo pipefail
: "${SSH_PASSWORD:?}"
printf 'root:%s\n' "$SSH_PASSWORD" | chpasswd
mkdir -p /run/sshd /run/dbha /var/log/dbha
if [ ! -s /etc/machine-id ]; then hostname | md5sum | cut -d " " -f 1 > /etc/machine-id; fi
ssh-keygen -A
printf 'PermitRootLogin yes\nPasswordAuthentication yes\nUsePAM no\n' > /etc/ssh/sshd_config.d/dbha-lab.conf
/usr/sbin/sshd -D &
sshd_pid=$!
/usr/local/bin/docker-entrypoint.sh mysqld "$@" &
mysql_pid=$!
probe_pid=""
trap 'kill "$mysql_pid" "$sshd_pid" ${probe_pid:-} 2>/dev/null || true' TERM INT EXIT
if [ -f /etc/dbha/probe.yaml ]; then
  /usr/local/bin/dbha-probe --config /etc/dbha/probe.yaml &
  probe_pid=$!
else
  echo 'missing /etc/dbha/probe.yaml' >&2
  exit 1
fi
wait -n "$mysql_pid" "$sshd_pid" "$probe_pid"
exit 1
