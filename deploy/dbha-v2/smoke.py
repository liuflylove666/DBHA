#!/usr/bin/env python3
"""Verify discovery and routes; optional failover affects only the disposable lab."""
import argparse
import json
from pathlib import Path
import subprocess
import tempfile
import time
import uuid

HERE = Path(__file__).resolve().parent
COMPOSE = ["docker", "compose", "--env-file", str(HERE / "generated/.env")]


def compose(*args, input=None):
    return subprocess.run(COMPOSE + list(args), input=input, text=True, capture_output=True, check=True, timeout=30).stdout.strip()


def api(path, method="GET", payload=None):
    command = ["exec", "-T", "dbha-server", "dbha-server", "ctl", "-c", "/etc/dbha/server.json", method, path]
    request_file = None
    try:
        if payload is not None:
            with tempfile.NamedTemporaryFile("w", dir=HERE / "generated", prefix="ctl-", suffix=".json", delete=False) as handle:
                json.dump(payload, handle)
                request_file = Path(handle.name)
            request_file.chmod(0o600)
            command += ["--data-file", "/etc/dbha/" + request_file.name]
        return json.loads(compose(*command))["data"]
    finally:
        if request_file:
            request_file.unlink(missing_ok=True)


def cluster():
    deployment = json.loads((HERE / "generated/identity.json").read_text())["deployment_ids"]["lab"]
    items = api("/api/v1/clusters")["items"]
    return next(item for item in items if item["deployment_id"] == deployment)


def usable(state):
    return state.get("topology_state") == "READY" and (
        not state.get("standby_id") or state.get("health_state") == "HEALTHY")


def eventually(fn, seconds=180):
    deadline = time.monotonic() + seconds
    error = None
    while time.monotonic() < deadline:
        try:
            result = fn()
            if result:
                return result
        except Exception as exc:
            error = exc
        time.sleep(2)
    raise RuntimeError(f"timed out: {error}")


def sql(query, service="mysql-standby", host="127.0.0.1", port=3306, user="root"):
    credential = "APP_PASSWORD" if user == "app" else "MYSQL_ROOT_PASSWORD"
    command = f'MYSQL_PWD="${credential}" exec mysql --connect-timeout=5 -u "$3" -N -B -h "$1" -P "$2"'
    return compose("exec", "-T", service, "sh", "-c", command, "sh", host, str(port), user, input=query + ";\n")


def check_route(ip, expected):
    return sql("SELECT @@server_id", host=ip, port=10000) == str(expected)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--guard-initial-primary", action="store_true")
    parser.add_argument("--record-initial", action="store_true")
    parser.add_argument("--set-switching", choices=("true", "false"))
    parser.add_argument("--failover", action="store_true")
    args = parser.parse_args()
    baseline = HERE / "generated/initial-primary-id"
    if args.guard_initial_primary:
        if baseline.exists() and cluster().get("primary_id") not in ("", baseline.read_text().strip()):
            raise RuntimeError("topology switched; refusing to restart the old primary")
        return
    if args.set_switching:
        state = eventually(lambda: (c if usable(c := cluster()) else None), 240) if args.set_switching == "true" else cluster()
        data = api(f"/api/v1/clusters/{state['id']}/switching", "PUT",
                   {"expected_epoch": state["topology_epoch"], "enabled": args.set_switching == "true"})
        print(f"switching_enabled={args.set_switching}, cluster={state['id']}")
        return
    state = eventually(lambda: (c if usable(c := cluster()) else None), 240)
    if args.record_initial:
        if not baseline.exists():
            baseline.write_text(state["primary_id"] + "\n")
            baseline.chmod(0o600)
        return
    expected = 22 if state["primary_id"] == "mysql:" + sql("SELECT @@server_uuid", service="mysql-standby") else 21
    for ip in ("10.203.80.31", "10.203.80.32"):
        eventually(lambda ip=ip: check_route(ip, expected))
    note = "smoke-" + uuid.uuid4().hex
    sql(f"INSERT INTO lab.ha_probe(note) VALUES ('{note}')", host="10.203.80.31", port=10000, user="app")
    assert sql(f"SELECT COUNT(*) FROM lab.ha_probe WHERE note='{note}'", host="10.203.80.32", port=10000, user="app") == "1"
    print(f"PASS: READY/{state['health_state']}, two Proxy routes to server_id={expected}, application write/read")
    if not args.failover:
        return
    if expected != 21 or not state.get("switching_enabled"):
        raise RuntimeError("failover requires original primary and explicitly enabled switching")
    compose("stop", "mysql-master")
    eventually(lambda: cluster().get("primary_id") == "mysql:" + sql("SELECT @@server_uuid", service="mysql-standby"), 300)
    for ip in ("10.203.80.31", "10.203.80.32"):
        eventually(lambda ip=ip: check_route(ip, 22), 180)
    sql("INSERT INTO lab.ha_probe(note) VALUES ('after-failover')", host="10.203.80.31", port=10000, user="app")
    print("PASS: promotion, both routes, post-failover write; old primary remains stopped")


if __name__ == "__main__":
    main()
