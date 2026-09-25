#!/bin/sh
set -eu
mkdir -p /run/dbha /var/log/dbha
exec python3 /usr/local/bin/proxy-supervisor.py
