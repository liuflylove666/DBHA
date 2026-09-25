#!/usr/bin/env python3
"""One-time authenticated registration; no topology or roles are written."""
import argparse
import json
from pathlib import Path
import ssl
import urllib.error
import urllib.request

from configure import node_names


def install(out, endpoint):
    ids = json.loads((out / "identity.json").read_text())
    if (out / "migration-agent-map.json").exists():
        rows = json.loads((out / "migration-agent-map.json").read_text())
        for row in rows:
            token_file = Path(row["token_file"])
            if not token_file.is_file() or token_file.stat().st_mode & 0o077:
                raise RuntimeError("migrated agent token missing or not private: " + str(token_file))
        print("Existing migrated credentials retained; offline import owns deployment and agent records")
        return
    endpoints = [endpoint] if isinstance(endpoint, str) else list(endpoint)
    endpoints = [value.rstrip("/") for value in endpoints]
    if not endpoints or any(not value.startswith(("http://", "https://")) for value in endpoints):
        raise ValueError("at least one HTTP or HTTPS server endpoint is required")
    token = (out / "admin.token").read_text().strip()
    preferred = 0

    def post(path, body, idempotency=None):
        nonlocal preferred
        headers = {"Authorization": "Bearer " + token, "Content-Type": "application/json"}
        if idempotency:
            headers["Idempotency-Key"] = idempotency
        last_error = None
        for offset in range(len(endpoints)):
            index = (preferred + offset) % len(endpoints)
            current = endpoints[index]
            context = ssl.create_default_context(cafile=str(out / "ca.crt")) if current.startswith("https://") else None
            request = urllib.request.Request(current + path, json.dumps(body).encode(), headers, method="POST")
            try:
                with urllib.request.urlopen(request, context=context, timeout=10) as response:
                    result = json.load(response)
                preferred = index
                return result["data"]
            except urllib.error.HTTPError as exc:
                try:
                    code = json.loads(exc.read()).get("error", {}).get("code")
                except (json.JSONDecodeError, UnicodeDecodeError):
                    code = None
                if code != "NOT_LEADER":
                    raise
                last_error = exc
            except (urllib.error.URLError, TimeoutError) as exc:
                last_error = exc
        raise last_error

    for group in ids["groups"]:
        name = group[0]
        allowed = ([host + "/32" for node, host in ids["addresses"].items() if node != "controller"]
                   if ids.get("addresses") else [ids["prefix"] + ".0/24"])
        deployment = post("/api/v1/deployments",
                          {"allowed_networks": allowed, "credential_profile": "default"},
                          "lab:" + ids["deployments"][name])
        previous = ids.setdefault("deployment_ids", {}).get(name)
        if previous and deployment["id"] != previous:
            raise RuntimeError("deployment identity changed; refusing to attach nodes")
        if not previous:
            ids["deployment_ids"][name] = deployment["id"]
            (out / "identity.json").write_text(json.dumps(ids, indent=2) + "\n")
            (out / "identity.json").chmod(0o600)
        for node in node_names(group):
            path = out / (node + "-agent.token")
            if path.exists():
                continue
            kind = "proxy" if "proxy" in node else "mysql"
            try:
                agent = post(f"/api/v1/deployments/{deployment['id']}/agents",
                             {"agent_id": ids["agents"][node], "kind": kind})
            except urllib.error.HTTPError as exc:
                if exc.code != 409:
                    raise
                # A prior response may have been lost. Rotate the bound agent
                # credential; never allocate a replacement agent identity.
                agent = post(f"/api/v1/agents/{ids['agents'][node]}/credentials/rotate",
                             {"request_id": "installer-recover:" + ids["agents"][node]})
            if agent.get("agent_id", ids["agents"][node]) != ids["agents"][node]:
                raise RuntimeError("server returned a different agent")
            path.write_text(agent["token"] + "\n")
            path.chmod(0o600)
        print(f"{name}: deployment {deployment['id']} cluster {deployment['cluster_id']} registered")


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--output", type=Path, default=Path(__file__).resolve().parent / "generated")
    group = parser.add_mutually_exclusive_group()
    group.add_argument("--server", default="http://127.0.0.1:18080")
    group.add_argument("--servers", nargs="+")
    args = parser.parse_args()
    install(args.output, args.servers or args.server)
