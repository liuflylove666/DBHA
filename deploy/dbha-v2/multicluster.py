#!/usr/bin/env python3
"""Render a three-cluster lab with one dbha-server, one etcd and 12 nodes."""
import json
from pathlib import Path

from configure import MULTI, node_names, render

HERE = Path(__file__).resolve().parent
NET = "10.203.81"
OUT = HERE / "generated-multi"
HEALTH = {"test": ["CMD-SHELL", "MYSQL_PWD=$$MYSQL_ROOT_PASSWORD mysql -h127.0.0.1 -uroot -Nse 'SELECT 1' >/dev/null"], "interval": "5s", "timeout": "5s", "retries": 40, "start_period": "30s"}
NODE_ENV = {key: "${" + key + "}" for key in ("MYSQL_ROOT_PASSWORD", "MYSQL_REPL_USER", "MYSQL_REPL_PASSWORD", "DBHA_USER", "DBHA_PASSWORD", "APP_PASSWORD", "SSH_PASSWORD")}
NODE_ENV["MYSQL_ROOT_HOST"] = "%"


def render_compose(out=OUT):
    services = {
        "etcd": {"image": "gcr.io/etcd-development/etcd:v3.6.0",
                 "command": ["/usr/local/bin/etcd", "--name=multi-lab", "--data-dir=/etcd-data",
                             "--listen-client-urls=http://0.0.0.0:2379", f"--advertise-client-urls=http://{NET}.11:2379",
                             "--listen-peer-urls=http://0.0.0.0:2380", f"--initial-advertise-peer-urls=http://{NET}.11:2380",
                             f"--initial-cluster=multi-lab=http://{NET}.11:2380", "--auto-compaction-mode=periodic",
                             "--auto-compaction-retention=1h", "--quota-backend-bytes=2147483648"],
                 "volumes": ["etcd-data:/etcd-data"], "networks": {"lab": {"ipv4_address": f"{NET}.11"}},
                 "healthcheck": {"test": ["CMD", "/usr/local/bin/etcdctl", "endpoint", "health"], "interval": "5s", "timeout": "5s", "retries": 20}},
        "dbha-server": {"image": "dbha-v2-lab-runtime:local", "platform": "linux/amd64",
                        "command": ["dbha-server", "-c", "/etc/dbha/server.json"],
                        "volumes": [".:/etc/dbha:ro", "server-state:/var/lib/dbha-server", "server-logs:/var/log/dbha-server"],
                        "depends_on": {"etcd": {"condition": "service_healthy"}},
                        "networks": {"lab": {"ipv4_address": f"{NET}.12"}}, "ports": ["127.0.0.1:18081:8080"], "restart": "no",
                        "healthcheck": {"test": ["CMD", "curl", "-fsS", "http://localhost:8080/readyz"], "interval": "5s", "timeout": "5s", "retries": 30}},
    }
    volumes = {"etcd-data": {}, "server-state": {}, "server-logs": {}}
    for group in MULTI:
        names = node_names(group)
        for name, suffix in zip(names, group[1:]):
            kind = "proxy" if "proxy" in name else "mysql"
            mounts = [f"{name}-probe-state:/var/lib/dbha-probe",
                      f"./{name}-discovery.json:/etc/dbha/discovery.json:ro",
                      f"./{name}-agent.token:/etc/dbha/agent.token:ro",
                      f"./{name}-ssh_host_ed25519_key:/etc/ssh/ssh_host_ed25519_key:ro",
                      f"./{name}-ssh_host_ed25519_key.pub:/etc/ssh/ssh_host_ed25519_key.pub:ro"]
            volumes[name + "-probe-state"] = {}
            if kind == "mysql":
                mounts += [f"{name}-data:/var/lib/mysql", f"./{name}-dbha.password:/etc/dbha/dbha.password:ro"]
                volumes[name + "-data"] = {}
                services[name] = {"image": "dbha-v2-lab-mysql:local", "platform": "linux/amd64",
                    "environment": NODE_ENV, "restart": "no", "volumes": mounts,
                    "depends_on": {"dbha-server": {"condition": "service_healthy"}},
                    "command": [f"--server-id={suffix}", "--log-bin=mysql-bin", "--relay-log=dbha-relay-bin",
                                "--relay-log-index=dbha-relay-bin.index", "--gtid-mode=ON", "--enforce-gtid-consistency=ON",
                                "--log-slave-updates=ON", "--binlog-format=ROW", "--sync-binlog=1",
                                "--innodb-flush-log-at-trx-commit=1", "--default-authentication-plugin=mysql_native_password"],
                    "networks": {"lab": {"ipv4_address": f"{NET}.{suffix}"}}, "healthcheck": HEALTH}
            else:
                mounts += [f"./{name}-proxy-admin.password:/etc/dbha/proxy-admin.password:ro"]
                services[name] = {"image": "dbha-v2-lab-proxy:local", "platform": "linux/amd64",
                    "environment": {"PROXY_ADMIN_USER": "${PROXY_ADMIN_USER}", "PROXY_ADMIN_PASSWORD": "${PROXY_ADMIN_PASSWORD}",
                                    "SSH_PASSWORD": "${SSH_PASSWORD}"},
                    "restart": "no", "volumes": mounts,
                    "depends_on": {"dbha-server": {"condition": "service_healthy"}},
                    "ports": [f"127.0.0.1:{14306 + 2*(int(group[0][-1])-1) + (0 if name.endswith('1') else 1)}:10000"],
                    "networks": {"lab": {"ipv4_address": f"{NET}.{suffix}"}}}
        boot = group[0] + "-bootstrap"
        services[boot] = {"image": "dbha-v2-lab-mysql:local", "platform": "linux/amd64",
            "entrypoint": ["bash", "/usr/local/bin/mysql-replication.sh"],
            "environment": dict(NODE_ENV, MYSQL_MASTER=f"{NET}.{group[1]}", MYSQL_STANDBY=f"{NET}.{group[2]}"),
            "volumes": [boot + "-state:/state"],
            "depends_on": {names[0]: {"condition": "service_healthy"}, names[1]: {"condition": "service_healthy"}},
            "networks": ["lab"]}
        volumes[boot + "-state"] = {}
    compose = {"name": "dbha-v2-multi-lab", "services": services,
               "networks": {"lab": {"ipam": {"config": [{"subnet": NET + ".0/24"}]}}}, "volumes": volumes}
    (out / "compose.json").write_text(json.dumps(compose, indent=2) + "\n")
    return compose


if __name__ == "__main__":
    render(OUT, MULTI, NET)
    render_compose()
    print("Rendered three deployments without metadata seed or control MySQL.")
