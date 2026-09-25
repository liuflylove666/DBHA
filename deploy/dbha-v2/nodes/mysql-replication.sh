#!/bin/bash
set -eu
: "${MYSQL_ROOT_PASSWORD:?}"
: "${MYSQL_REPL_USER:?}"
: "${MYSQL_REPL_PASSWORD:?}"
[ "${#MYSQL_REPL_PASSWORD}" -le 32 ] || { echo 'Replication password exceeds MySQL 32-character limit.' >&2; exit 1; }
export MYSQL_PWD="$MYSQL_ROOT_PASSWORD"
for value in "$MYSQL_MASTER" "$MYSQL_STANDBY" "$MYSQL_REPL_USER" "$MYSQL_REPL_PASSWORD"; do
  case "$value" in ''|*[!a-zA-Z0-9_.]*) echo 'Invalid bootstrap argument' >&2; exit 1;; esac
done

replication_ready() {
  status=$(mysql -h "$MYSQL_STANDBY" -uroot -e 'SHOW REPLICA STATUS\G')
  grep -q 'Replica_IO_Running: Yes' <<<"$status" &&
    grep -q 'Replica_SQL_Running: Yes' <<<"$status" &&
    mysql -h "$MYSQL_STANDBY" -uroot -Nse 'SELECT COUNT(*) FROM lab.ha_probe' >/dev/null 2>&1
}

status=$(mysql -h "$MYSQL_STANDBY" -uroot -e 'SHOW REPLICA STATUS\G')
if [ -z "$status" ]; then
  # A promoted candidate has no replication channel. The persistent marker
  # prevents a later compose run from turning it back into the old standby.
  if [ -f /state/initialized ]; then
    echo 'Replication channel is absent after prior initialization; topology left unchanged.'
    exit 0
  fi
  mysql -h "$MYSQL_STANDBY" -uroot -e 'SET PERSIST read_only=ON; SET PERSIST super_read_only=ON;'
  mysql -h "$MYSQL_STANDBY" -uroot <<SQL
CHANGE REPLICATION SOURCE TO SOURCE_HOST='$MYSQL_MASTER', SOURCE_PORT=3306, SOURCE_USER='$MYSQL_REPL_USER', SOURCE_PASSWORD='$MYSQL_REPL_PASSWORD', SOURCE_AUTO_POSITION=1;
START REPLICA;
SQL
elif ! grep -q "Source_Host: $MYSQL_MASTER$" <<<"$status"; then
  echo 'Existing replication points elsewhere; refusing to overwrite it.' >&2
  exit 1
else
  standby_mode=$(mysql -h "$MYSQL_STANDBY" -uroot -Nse "SELECT CONCAT(@@global.read_only, ':', @@global.super_read_only)")
  master_mode=$(mysql -h "$MYSQL_MASTER" -uroot -Nse 'SELECT @@global.read_only')
  if [ "$standby_mode" != '1:1' ] || [ "$master_mode" != '0' ]; then
    echo 'Current database roles do not match the original topology; refusing replication repair.' >&2
    exit 1
  fi
  mysql -h "$MYSQL_STANDBY" -uroot -e 'START REPLICA;' || true
  for ((i=0; i<5; i++)); do
    replication_ready && break
    sleep 2
  done
  if ! replication_ready; then
    echo 'Existing replication channel is unhealthy; rebuilding it from GTID state.'
    mysql -h "$MYSQL_STANDBY" -uroot <<SQL
STOP REPLICA;
RESET REPLICA ALL;
CHANGE REPLICATION SOURCE TO SOURCE_HOST='$MYSQL_MASTER', SOURCE_PORT=3306, SOURCE_USER='$MYSQL_REPL_USER', SOURCE_PASSWORD='$MYSQL_REPL_PASSWORD', SOURCE_AUTO_POSITION=1;
START REPLICA;
SQL
  fi
fi
# A candidate must be read-only until the control service has verified and
# promoted it. Keep this setting across restarts in the persisted data volume.
mysql -h "$MYSQL_STANDBY" -uroot -e 'SET PERSIST read_only=ON; SET PERSIST super_read_only=ON;'
mysql -h "$MYSQL_MASTER" -uroot <<'SQL'
CREATE DATABASE IF NOT EXISTS lab;
CREATE TABLE IF NOT EXISTS lab.ha_probe (id BIGINT PRIMARY KEY AUTO_INCREMENT, note VARCHAR(128), created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP);
INSERT INTO lab.ha_probe(note) VALUES ('bootstrap');
SQL
for ((i=0; i<60; i++)); do
  if replication_ready; then
    touch /state/initialized
    echo 'GTID replication and initial data verified.'
    exit 0
  fi
  sleep 2
done
echo 'Replication did not become ready.' >&2
exit 1
