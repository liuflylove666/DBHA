#!/usr/bin/env python3
"""Check three deployment identities, READY topology and isolated Proxy routes."""
import argparse
import json
from pathlib import Path
import subprocess
import tempfile
import time

HERE = Path(__file__).resolve().parent
OUT = HERE / "generated-multi"
COMPOSE = ["docker", "compose", "--env-file", str(OUT / ".env"), "-f", str(OUT / "compose.json")]


def compose(*args, input=None):
    return subprocess.run(COMPOSE + list(args), input=input, text=True, capture_output=True, check=True, timeout=30).stdout.strip()


def api(path, method="GET", payload=None):
    command = ["exec", "-T", "dbha-server", "dbha-server", "ctl", "-c", "/etc/dbha/server.json", method, path]
    request_file = None
    try:
        if payload is not None:
            with tempfile.NamedTemporaryFile("w", dir=OUT, prefix="ctl-", suffix=".json", delete=False) as handle:
                json.dump(payload, handle)
                request_file = Path(handle.name)
            request_file.chmod(0o600)
            command += ["--data-file", "/etc/dbha/" + request_file.name]
        return json.loads(compose(*command))["data"]
    finally:
        if request_file:
            request_file.unlink(missing_ok=True)


def groups():
    ids = json.loads((OUT / "identity.json").read_text())["deployment_ids"]
    clusters = api("/api/v1/clusters")["items"]
    return {name: next(c for c in clusters if c["deployment_id"] == did) for name, did in ids.items()}


def usable(state):
    return state.get("topology_state") == "READY" and (
        not state.get("standby_id") or state.get("health_state") == "HEALTHY")


def eventually(fn, timeout=240):
    deadline = time.monotonic() + timeout
    last = None
    while time.monotonic() < deadline:
        try:
            result = fn()
            if result:
                return result
        except Exception as exc:
            last = exc
        time.sleep(2)
    raise RuntimeError(f"timed out: {last}")


def sql(service, host, query, port=10000):
    return compose("exec", "-T", service, "sh", "-c",
                   'MYSQL_PWD="$MYSQL_ROOT_PASSWORD" exec mysql --connect-timeout=5 -uroot -N -B -h "$1" -P "$2"',
                   "sh", host, str(port), input=query + ";\n")


def check():
    state = eventually(lambda: (g if len(g := groups()) == 3 and all(usable(c) for c in g.values()) else None))
    assert len({c["id"] for c in state.values()}) == 3
    for number in (1, 2, 3):
        name = f"cluster{number}"
        master = state[name]["primary_id"]
        standby_uuid = sql(f"{name}-mysql-standby", "127.0.0.1", "SELECT @@server_uuid", 3306)
        expected = 20*number + (2 if master == "mysql:" + standby_uuid else 1)
        for offset in (11, 12):
            host = f"10.203.81.{20*number+offset}"
            assert sql(f"{name}-mysql-standby", host, "SELECT @@server_id") == str(expected)
    return state


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--failover", action="store_true")
    parser.add_argument("--guard-initial-primary", action="store_true")
    parser.add_argument("--record-initial", action="store_true")
    parser.add_argument("--set-switching", choices=("true", "false"))
    args = parser.parse_args()
    baseline = OUT / "initial-primary-ids.json"
    if args.guard_initial_primary:
        if baseline.exists():
            expected = json.loads(baseline.read_text())
            for name, state in groups().items():
                if state.get("primary_id") not in ("", expected[name]):
                    raise RuntimeError(f"{name} switched; refusing to restart old primary")
        return
    if args.set_switching:
        states = eventually(lambda: (g if len(g := groups()) == 3 and all(usable(c) for c in g.values()) else None)) \
            if args.set_switching == "true" else groups()
        for state in states.values():
            api(f"/api/v1/clusters/{state['id']}/switching", "PUT",
                {"expected_epoch": state["topology_epoch"], "enabled": args.set_switching == "true"})
        print("switching_enabled=" + args.set_switching)
        return
    before = check()
    if args.record_initial:
        if not baseline.exists():
            baseline.write_text(json.dumps({name: c["primary_id"] for name, c in before.items()}, indent=2) + "\n")
            baseline.chmod(0o600)
        return
    print("PASS: three independent READY clusters and six Proxy routes")
    if not args.failover:
        return
    if not all(c.get("switching_enabled") for c in before.values()):
        raise RuntimeError("enable switching explicitly on each cluster before fault injection")
    compose("stop", "cluster1-mysql-master")
    eventually(lambda: groups()["cluster1"]["primary_id"] != before["cluster1"]["primary_id"], 300)
    after = groups()
    assert all(after[f"cluster{i}"]["primary_id"] == before[f"cluster{i}"]["primary_id"] for i in (2, 3))
    check()
    print("PASS: cluster1 promoted; clusters2/3 unchanged; six routes checked")


if __name__ == "__main__":
    main()
