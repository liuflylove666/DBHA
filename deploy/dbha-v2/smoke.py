#!/usr/bin/env python3
"""Verify the isolated lab; --failover stops only its disposable master node."""
import argparse
import json
from pathlib import Path
import subprocess
import time
import uuid

HERE = Path(__file__).resolve().parent
COMPOSE = ["docker", "compose", "--project-directory", str(HERE)]


def compose(*args, input=None):
    return subprocess.run(COMPOSE + list(args), input=input, text=True, capture_output=True, check=True, timeout=30).stdout.strip()


def sql(query, service="metadata-store", host="127.0.0.1", port=3306, user="root"):
    # SQL goes through stdin; credentials stay in the container environment.
    credential = "APP_PASSWORD" if user == "app" else "MYSQL_ROOT_PASSWORD"
    return compose("exec", "-T", service, "sh", "-c", f'MYSQL_PWD="${credential}" exec mysql --connect-timeout=5 -u "$3" -N -B -h "$1" -P "$2"', "sh", host, str(port), user, input=query + ";\n")


def eventually(fn, timeout=180):
    deadline = time.monotonic() + timeout
    error = None
    while time.monotonic() < deadline:
        try:
            result = fn()
            if result:
                return result
        except (subprocess.CalledProcessError, subprocess.TimeoutExpired, ValueError, AssertionError) as exc:
            error = exc
        time.sleep(2)
    raise RuntimeError(f"Check timed out; last error: {error}")


def primary():
    return sql("SELECT ip FROM dbha_metadata.standalone_instances WHERE instance_role='backend_master' AND status IN ('running','available')")


def check_proxy(ip, expected):
    result = sql("SELECT @@server_id", host=ip, port=10000)
    return result == expected


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--failover", action="store_true")
    args = parser.parse_args()
    eventually(lambda: sql("SELECT COUNT(*) FROM dbha_metadata.standalone_instances") == "4")
    before = eventually(primary)
    nodes = ["mysql-standby", "proxy1", "proxy2"]
    if before == "10.203.80.21":
        nodes.append("mysql-master")
    for node in nodes:
        eventually(lambda node=node: json.loads(compose("exec", "-T", node, "dbha-probe", "health", "-j", "-c", "/etc/dbha/probe.yaml"))["status"] == "running")
    expected = "21" if before == "10.203.80.21" else "22"
    for ip in ("10.203.80.31", "10.203.80.32"):
        eventually(lambda ip=ip: check_proxy(ip, expected))
    active_nodes = int(sql("SELECT COUNT(*) FROM dbha_metadata.standalone_instances WHERE status IN ('running','available')"))
    eventually(lambda: int(sql("SELECT COUNT(DISTINCT db_ip) FROM dbha_data.t_dbha_status WHERE report_timestamp >= UNIX_TIMESTAMP()-30")) >= active_nodes)
    note = "smoke-" + uuid.uuid4().hex
    sql(f"INSERT INTO lab.ha_probe(note) VALUES ('{note}')", service="mysql-standby", host="10.203.80.31", port=10000, user="app")
    assert sql(f"SELECT COUNT(*) FROM lab.ha_probe WHERE note='{note}'", service="mysql-standby", host="10.203.80.32", port=10000, user="app") == "1"
    if before == "10.203.80.21":
        eventually(lambda: sql(f"SELECT COUNT(*) FROM lab.ha_probe WHERE note='{note}'", host="10.203.80.22") == "1")
    print("PASS: MySQL metadata, fresh metrics from active nodes, both Proxy routes, write/read" + (" and replication" if expected == "21" else ""), flush=True)
    if not args.failover:
        return
    if before != "10.203.80.21":
        raise RuntimeError("Failover test requires the original master; no automatic topology reset is performed")
    config = (HERE / "generated/analysis.yaml").read_text()
    if "enableSwitching: true" not in config:
        raise RuntimeError("First explicitly enable lab switching: bash lab.sh enable-switching")
    print("Stopping this lab's mysql-master container; it will remain stopped after the test.", flush=True)
    compose("stop", "mysql-master")
    eventually(lambda: primary() == "10.203.80.22", timeout=240)
    for ip in ("10.203.80.31", "10.203.80.32"):
        eventually(lambda ip=ip: check_proxy(ip, "22"))
    sql("INSERT INTO lab.ha_probe(note) VALUES ('after-failover')", service="mysql-standby", host="10.203.80.31", port=10000, user="app")
    # The replay of the same directed swap must not swap the roles back.
    payload = json.dumps({"bk_cloud_id": 0, "payloads": [{"instance1": {"ip": "10.203.80.21", "port": 3306}, "instance2": {"ip": "10.203.80.22", "port": 3306}}]})
    compose("exec", "-T", "metadata-api", "sh", "-c", 'curl -fsS -H "Authorization: Bearer $API_TOKEN" -H "Content-Type: application/json" --data-binary @- http://localhost:8080/api/v1/swap-mysql-role', input=payload)
    assert primary() == "10.203.80.22", "Swap replay changed primary"
    compose("restart", "proxy1")
    eventually(lambda: check_proxy("10.203.80.31", "22"))
    print("PASS: automatic promotion, both Proxy routes, post-failover write, idempotent role replay, Proxy restart", flush=True)
    print("Old master remains stopped. Do not restart it without isolation and replication recovery.")


if __name__ == "__main__":
    main()
