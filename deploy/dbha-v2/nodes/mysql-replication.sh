#!/bin/bash
set -eu
: "${MYSQL_ROOT_PASSWORD:?}"
: "${MYSQL_REPL_USER:?}"
: "${MYSQL_REPL_PASSWORD:?}"
export MYSQL_PWD="$MYSQL_ROOT_PASSWORD"
# This marker shares the lifecycle of the lab's data volumes. Never reconfigure
# replication after a failover just because compose starts the init job again.
if [ -f /state/initialized ]; then
  echo 'Replication initialized previously; topology left unchanged.'
  exit 0
fi
for value in "$MYSQL_MASTER" "$MYSQL_STANDBY" "$MYSQL_REPL_USER" "$MYSQL_REPL_PASSWORD"; do
  case "$value" in ''|*[!a-zA-Z0-9_.]*) echo 'Invalid bootstrap argument' >&2; exit 1;; esac
done
status=$(mysql -h "$MYSQL_STANDBY" -uroot -e 'SHOW SLAVE STATUS\G')
if [ -z "$status" ]; then
  mysql -h "$MYSQL_STANDBY" -uroot <<SQL
CHANGE MASTER TO MASTER_HOST='$MYSQL_MASTER', MASTER_PORT=3306, MASTER_USER='$MYSQL_REPL_USER', MASTER_PASSWORD='$MYSQL_REPL_PASSWORD', MASTER_AUTO_POSITION=1;
START SLAVE;
SQL
elif ! grep -q "Master_Host: $MYSQL_MASTER$" <<<"$status"; then
  echo 'Existing replication points elsewhere; refusing to overwrite it.' >&2
  exit 1
fi
mysql -h "$MYSQL_MASTER" -uroot <<'SQL'
CREATE DATABASE IF NOT EXISTS lab;
CREATE TABLE IF NOT EXISTS lab.ha_probe (id BIGINT PRIMARY KEY AUTO_INCREMENT, note VARCHAR(128), created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP);
INSERT INTO lab.ha_probe(note) VALUES ('bootstrap');
SQL
for ((i=0; i<60; i++)); do
  status=$(mysql -h "$MYSQL_STANDBY" -uroot -e 'SHOW SLAVE STATUS\G')
  if grep -q 'Slave_IO_Running: Yes' <<<"$status" && grep -q 'Slave_SQL_Running: Yes' <<<"$status" && mysql -h "$MYSQL_STANDBY" -uroot -Nse 'SELECT COUNT(*) FROM lab.ha_probe' >/dev/null 2>&1; then
    touch /state/initialized
    echo 'GTID replication and initial data verified.'
    exit 0
  fi
  sleep 2
done
echo 'Replication did not become ready.' >&2
exit 1
