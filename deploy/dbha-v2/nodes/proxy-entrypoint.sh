#!/bin/bash
set -euo pipefail
: "${SSH_PASSWORD:?}"
mkdir -p /run/sshd /run/dbha /var/log/dbha /var/lib/dbha-probe
if [ ! -s /etc/machine-id ]; then hostname | md5sum | cut -d ' ' -f 1 > /etc/machine-id; fi
printf 'root:%s\n' "$SSH_PASSWORD" | chpasswd
printf 'PermitRootLogin yes\nPasswordAuthentication yes\nUsePAM no\nHostKey /etc/ssh/ssh_host_ed25519_key\n' > /etc/ssh/sshd_config.d/dbha-lab.conf
/usr/sbin/sshd -D & sshd_pid=$!
/usr/local/bin/dbha-probe discover -c /etc/dbha/discovery.json & probe_pid=$!
python3 /usr/local/bin/proxy-supervisor.py & supervisor_pid=$!
trap 'kill "$sshd_pid" "$probe_pid" "$supervisor_pid" 2>/dev/null || true' TERM INT EXIT
wait -n "$sshd_pid" "$probe_pid" "$supervisor_pid"
exit 1
