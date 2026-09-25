#!/usr/bin/env python3
"""Render stable identities and discovery config; never seed roles."""
import argparse
import ipaddress
import json
import os
from pathlib import Path
import secrets
import subprocess
import uuid

HERE = Path(__file__).resolve().parent
SINGLE = (("lab", 21, 22, 31, 32),)
MULTI = tuple((f"cluster{i}", 20*i+1, 20*i+2, 20*i+11, 20*i+12) for i in (1, 2, 3))


def private(path, value):
    path.parent.mkdir(parents=True, exist_ok=True)
    path.write_text(value)
    path.chmod(0o600)


def run(*cmd):
    subprocess.run(cmd, check=True, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)


def render(out, groups=SINGLE, prefix="10.203.80", addresses=None, controllers=None):
    os.umask(0o077)
    out.mkdir(parents=True, exist_ok=True)
    ipaddress.ip_network(prefix + ".0/24")
    inventory = [list(g) for g in groups]
    identity_path = out / "identity.json"
    if identity_path.exists():
        ids = json.loads(identity_path.read_text())
        if ids["prefix"] != prefix or ids["groups"] != inventory or ids.get("addresses") != addresses:
            raise ValueError("existing identity belongs to a different inventory")
    else:
        ids = {"prefix": prefix, "groups": inventory, "addresses": addresses, "deployments": {}, "agents": {}, "proxy_uuids": {}}
        for group in groups:
            ids["deployments"][group[0]] = str(uuid.uuid4())
            for name in node_names(group):
                ids["agents"][name] = str(uuid.uuid4())
                if "proxy" in name:
                    ids["proxy_uuids"][name] = str(uuid.uuid4())
        private(identity_path, json.dumps(ids, indent=2) + "\n")
    env_path = out / ".env"
    if env_path.exists():
        env = dict(row.split("=", 1) for row in env_path.read_text().splitlines() if row and not row.startswith("#"))
    else:
        env = {key: secrets.token_hex(24) for key in ("MYSQL_ROOT_PASSWORD", "DBHA_PASSWORD", "SSH_PASSWORD", "APP_PASSWORD")}
        # MySQL 8 rejects replication-source passwords longer than 32 characters.
        env["MYSQL_REPL_PASSWORD"] = secrets.token_hex(16)
        # The bundled Proxy's legacy admin authentication also has a 32-byte
        # password field; use 128 bits of entropy without truncating at login.
        env["PROXY_ADMIN_PASSWORD"] = secrets.token_hex(16)
        env.update(MYSQL_REPL_USER="repl", DBHA_USER="dbha", PROXY_ADMIN_USER="admin")
        private(env_path, "".join(f"{key}={value}\n" for key, value in env.items()))
    if not (out / "admin.token").exists():
        private(out / "admin.token", secrets.token_hex(32) + "\n")
    if groups == SINGLE:
        env["PROXY1_AGENT_ID"] = ids["agents"]["lab-proxy1"]
        env["PROXY2_AGENT_ID"] = ids["agents"]["lab-proxy2"]
        private(env_path, "".join(f"{key}={value}\n" for key, value in env.items()))
    controller = addresses["controller"] if addresses else prefix + ".12"
    controllers = controllers or [controller]
    for host in controllers:
        ipaddress.ip_address(host)
    if len(set(controllers)) != len(controllers):
        raise ValueError("controller addresses must be distinct")
    etcd = controller if addresses else prefix + ".11"
    allowed = [host + "/32" for name, host in addresses.items() if name != "controller"] if addresses else [prefix + ".0/24"]
    known_hosts = []
    for group in groups:
        for name, suffix in zip(node_names(group), group[1:]):
            host = addresses[name] if addresses else f"{prefix}.{suffix}"
            key = out / (name + "-ssh_host_ed25519_key")
            if not key.exists():
                run("ssh-keygen", "-q", "-t", "ed25519", "-N", "", "-f", str(key))
            key.chmod(0o600)
            (out / (key.name + ".pub")).chmod(0o600)
            known_hosts.append(host + " " + (out / (key.name + ".pub")).read_text().strip())
            kind = "proxy" if "proxy" in name else "mysql"
            config = {
                "server_url": f"http://{controller}:8080", "server_grpc": f"{controller}:50052",
                "server_urls": [f"http://{host}:8080" for host in controllers],
                "server_grpc_endpoints": [f"{host}:50052" for host in controllers],
                "token_file": "/etc/dbha/agent.token",
                "agent_id": ids["agents"][name], "state_file": "/var/lib/dbha-probe/identity.json",
                "kind": kind, "advertise_host": host,
                "route_reconcile_signal_file": "/run/dbha/route-reconcile.json",
            }
            if kind == "mysql":
                config["mysql"] = {"network": "tcp", "address": "127.0.0.1:3306", "user": env["DBHA_USER"], "password_file": "/etc/dbha/dbha.password", "port": 3306}
            else:
                config["proxy"] = {"uuid": ids["proxy_uuids"][name], "data_port": 10000, "admin_port": 11000, "admin_network": "tcp", "admin_address": "127.0.0.1:11000", "admin_user": env["PROXY_ADMIN_USER"], "admin_password_file": "/etc/dbha/proxy-admin.password"}
            private(out / (name + "-discovery.json"), json.dumps(config, indent=2) + "\n")
            private(out / (name + "-dbha.password"), env["DBHA_PASSWORD"] + "\n")
            private(out / (name + "-proxy-admin.password"), env["PROXY_ADMIN_PASSWORD"] + "\n")
    private(out / "ssh_known_hosts", "\n".join(known_hosts) + "\n")
    server = {
        "environment_id": "lab", "http_listen": ":8080", "grpc_listen": ":50052",
        "etcd_endpoints": [f"http://{host}:2379" for host in controllers] if addresses else [f"http://{etcd}:2379"],
        "admin_token_file": "/etc/dbha/admin.token",
        "state_dir": "/var/lib/dbha-server", "log_file": "/var/log/dbha-server/operations.jsonl",
        "allowed_networks": allowed, "max_replication_delay_seconds": 30,
        "failure_threshold": 3,
        "profiles": {"default": {
            "mysql_user": env["DBHA_USER"], "mysql_password": env["DBHA_PASSWORD"],
            "proxy_user": env["PROXY_ADMIN_USER"], "proxy_password": env["PROXY_ADMIN_PASSWORD"],
            "ssh_user": "root", "ssh_password": env["SSH_PASSWORD"],
            "ssh_known_hosts_file": "/etc/dbha/ssh_known_hosts", "ssh_port": 22,
            "probe_health_command": "/usr/local/bin/dbha-probe discover-health -c /etc/dbha/discovery.json",
        }},
    }
    private(out / "server.json", json.dumps(server, indent=2) + "\n")
    return ids


def node_names(group):
    base = group[0]
    return (base + "-mysql-master", base + "-mysql-standby", base + "-proxy1", base + "-proxy2")


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--enable-switching", action="store_true", help="Deprecated; use the live API after READY")
    parser.parse_args()
    render(HERE / "generated")
    print("Rendered stable identities and discovery configuration.")
