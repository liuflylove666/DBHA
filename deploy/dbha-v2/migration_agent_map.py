#!/usr/bin/env python3
"""Prepare stable, offline agent credentials for one-time management import."""
import argparse
import json
from pathlib import Path
import secrets

from configure import node_names


def generate(out):
    ids = json.loads((out / "identity.json").read_text())
    rows = []
    for group in ids["groups"]:
        for name, suffix in zip(node_names(group), group[1:]):
            kind = "proxy" if "proxy" in name else "mysql"
            token_file = out / (name + "-agent.token")
            if not token_file.exists():
                token_file.write_text(secrets.token_hex(32) + "\n")
                token_file.chmod(0o600)
            if token_file.stat().st_mode & 0o077:
                raise ValueError(f"agent token is not private: {token_file}")
            token = token_file.read_text().strip()
            if len(token) != 64 or any(c not in "0123456789abcdef" for c in token):
                raise ValueError(f"invalid agent token: {token_file}")
            row = {"kind": kind,
                   "host": ids["addresses"][name] if ids.get("addresses") else f"{ids['prefix']}.{suffix}",
                   "port": 10000 if kind == "proxy" else 3306,
                   "agent_id": ids["agents"][name], "token_file": str(token_file.resolve())}
            if kind == "proxy":
                row["proxy_uuid"] = ids["proxy_uuids"][name]
            rows.append(row)
    path = out / "migration-agent-map.json"
    path.write_text(json.dumps(rows, indent=2) + "\n")
    path.chmod(0o600)
    return path


if __name__ == "__main__":
    p = argparse.ArgumentParser(description=__doc__)
    p.add_argument("--output", type=Path, default=Path(__file__).resolve().parent / "generated")
    args = p.parse_args()
    print(generate(args.output))
