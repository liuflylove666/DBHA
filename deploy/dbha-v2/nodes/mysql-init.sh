#!/bin/sh
set -eu
# Credentials are generated as hex by configure.py. Reject SQL metacharacters.
for value in "$DBHA_USER" "$DBHA_PASSWORD" "$MYSQL_REPL_USER" "$MYSQL_REPL_PASSWORD" "$APP_PASSWORD"; do
  case "$value" in ''|*[!a-zA-Z0-9_]*) echo 'Invalid lab credential format' >&2; exit 1;; esac
done
MYSQL_PWD="$MYSQL_ROOT_PASSWORD" mysql -uroot <<SQL
SET SESSION sql_log_bin=0;
CREATE DATABASE IF NOT EXISTS infodba_schema;
CREATE USER IF NOT EXISTS '$DBHA_USER'@'%' IDENTIFIED WITH mysql_native_password BY '$DBHA_PASSWORD';
GRANT ALL PRIVILEGES ON *.* TO '$DBHA_USER'@'%';
CREATE USER IF NOT EXISTS '$MYSQL_REPL_USER'@'%' IDENTIFIED WITH mysql_native_password BY '$MYSQL_REPL_PASSWORD';
GRANT REPLICATION SLAVE, REPLICATION CLIENT ON *.* TO '$MYSQL_REPL_USER'@'%';
CREATE USER IF NOT EXISTS 'app'@'%' IDENTIFIED WITH mysql_native_password BY '$APP_PASSWORD';
GRANT ALL PRIVILEGES ON lab.* TO 'app'@'%';
SQL
