#!/usr/bin/env python3
"""Generate native configs for one or more controllers and four business hosts."""
import argparse
import importlib.util
import ipaddress
import json
import os
from pathlib import Path
import shutil
import subprocess

HERE = Path(__file__).resolve().parent
LAB = HERE.parent
SPEC = importlib.util.spec_from_file_location("lab_configure", LAB / "configure.py")
lab = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(lab)
NAMES = {"mysql1": "lab-mysql-master", "mysql2": "lab-mysql-standby",
         "proxy1": "lab-proxy1", "proxy2": "lab-proxy2"}


def scan(host):
    result = subprocess.run(["ssh-keyscan", "-T", "5", "-t", "ed25519", host],
                            capture_output=True, text=True, check=True, timeout=10)
    rows = [line for line in result.stdout.splitlines() if line.startswith(host + " ssh-ed25519 ")]
    if len(rows) != 1:
        raise ValueError(f"cannot pin one SSH ed25519 host key for {host}")
    return rows[0]


def generate(inventory, output, host_keys=None):
    os.umask(0o077)
    required = {"controller", "mysql1", "mysql2", "proxy1", "proxy2"}
    if set(inventory) not in (required, required | {"controllers"}):
        raise ValueError("inventory must contain the base host addresses and optional controllers")
    controllers = inventory.get("controllers", [inventory["controller"]])
    if not isinstance(controllers, list) or not controllers or controllers[0] != inventory["controller"]:
        raise ValueError("controllers must start with the primary controller address")
    for address in [inventory[key] for key in required] + controllers:
        ipaddress.ip_address(address)
    all_hosts = [inventory[key] for key in required] + controllers[1:]
    if len(set(all_hosts)) != len(all_hosts):
        raise ValueError("all EC2 host addresses must be distinct")
    addresses = {"controller": inventory["controller"], **{NAMES[key]: inventory[key] for key in NAMES}}
    output.mkdir(parents=True, exist_ok=True)
    src = output / "_generated"
    ids = lab.render(src, addresses=addresses, prefix=inventory["controller"].rsplit(".", 1)[0], controllers=controllers)
    env = dict(line.split("=", 1) for line in (src / ".env").read_text().splitlines())
    keys = host_keys or {name: scan(inventory[name]) for name in NAMES}
    if set(keys) != set(NAMES):
        raise ValueError("host keys must cover all four nodes")
    lab.private(src / "ssh_known_hosts", "\n".join(keys[name] for name in NAMES) + "\n")
    controller_nodes = [("controller" if index == 1 else f"controller{index}",
                         f"controller{index}", address)
                        for index, address in enumerate(controllers, 1)]
    initial_cluster = ",".join(f"{name}=http://{address}:2380" for _, name, address in controller_nodes)
    for directory, name, address in controller_nodes:
        ctrl_node = output / directory
        ctrl_node.mkdir(exist_ok=True)
        for filename in ("admin.token", "ssh_known_hosts"):
            shutil.copy2(src / filename, ctrl_node / filename)
        server = json.loads((src / "server.json").read_text())
        server.update(node_id=name, advertise_http=f"http://{address}:8080",
                      advertise_grpc=f"{address}:50052", state_dir="/srv/dbha/server",
                      log_file="/var/log/dbha/operations.jsonl")
        server["profiles"]["default"]["ssh_user"] = "dbha"
        server["profiles"]["default"]["probe_health_command"] = "/opt/dbha/bin/dbha-probe discover-health -c /etc/dbha/discovery.json"
        lab.private(ctrl_node / "server.json", json.dumps(server, indent=2) + "\n")
        lab.private(ctrl_node / "etcd.env",
                    f"ETCD_NAME={name}\nETCD_DATA_DIR=/etcd-data\n"
                    f"ETCD_LISTEN_CLIENT_URLS=http://{address}:2379\n"
                    f"ETCD_ADVERTISE_CLIENT_URLS=http://{address}:2379\n"
                    f"ETCD_LISTEN_PEER_URLS=http://{address}:2380\n"
                    f"ETCD_INITIAL_ADVERTISE_PEER_URLS=http://{address}:2380\n"
                    f"ETCD_INITIAL_CLUSTER={initial_cluster}\n")
    ctrl = output / "controller"
    for name, node in NAMES.items():
        dest = output / name
        dest.mkdir(exist_ok=True)
        config = json.loads((src / (node + "-discovery.json")).read_text())
        config["token_file"] = "/etc/dbha/agent.token"
        config["state_file"] = "/srv/dbha/probe/identity.json"
        config["route_reconcile_signal_file"] = "/run/dbha/route-reconcile.json"
        if name.startswith("mysql"):
            config["mysql"]["password_file"] = "/etc/dbha/dbha.password"
        else:
            config["proxy"]["admin_password_file"] = "/etc/dbha/proxy-admin.password"
        lab.private(dest / "discovery.json", json.dumps(config, indent=2) + "\n")
        for source, target in ((node + "-dbha.password", "dbha.password"),
                               (node + "-proxy-admin.password", "proxy-admin.password")):
            shutil.copy2(src / source, dest / target)
        lab.private(dest / "ssh-password", env["SSH_PASSWORD"] + "\n")
        if name.startswith("mysql"):
            lab.private(dest / "mysql.env", f"MYSQL_ROOT_PASSWORD={env['MYSQL_ROOT_PASSWORD']}\nMYSQL_ROOT_HOST=%\n")
            myid = 21 if name == "mysql1" else 22
            lab.private(dest / "mysql.cnf",
                        "[mysqld]\nbind-address=0.0.0.0\n"
                        + f"server-id={myid}\nlog-bin=mysql-bin\ngtid-mode=ON\nenforce-gtid-consistency=ON\n"
                        + "log-slave-updates=ON\nbinlog-format=ROW\nsync-binlog=1\ninnodb-flush-log-at-trx-commit=1\ndefault-authentication-plugin=mysql_native_password\n")
            init = dest / "mysql-init"
            init.mkdir(exist_ok=True)
            sql = "SET SESSION sql_log_bin=0;\nCREATE DATABASE IF NOT EXISTS lab;\n"
            for user, password, grant in (
                ("dbha", env["DBHA_PASSWORD"], "ALL PRIVILEGES ON *.*"),
                ("repl", env["MYSQL_REPL_PASSWORD"], "REPLICATION SLAVE, REPLICATION CLIENT ON *.*"),
                ("app", env["APP_PASSWORD"], "ALL PRIVILEGES ON lab.*")):
                sql += f"CREATE USER '{user}'@'%' IDENTIFIED BY '{password}';\nGRANT {grant} TO '{user}'@'%';\n"
            lab.private(init / "10-accounts.sql", sql)
        else:
            lab.private(dest / "proxy.env", f"PROXY_ADMIN_USER=admin\nPROXY_ADMIN_PASSWORD={env['PROXY_ADMIN_PASSWORD']}\n")
    lab.private(ctrl / "bootstrap-replication.sql",
                f"CHANGE REPLICATION SOURCE TO SOURCE_HOST='{inventory['mysql1']}', SOURCE_PORT=3306, "
                f"SOURCE_USER='repl', SOURCE_PASSWORD='{env['MYSQL_REPL_PASSWORD']}', SOURCE_AUTO_POSITION=1;\n"
                "START REPLICA;\nSET PERSIST read_only=ON;\nSET PERSIST super_read_only=ON;\n")
    lab.private(ctrl / "root-client.cnf", f"[client]\nuser=root\npassword={env['MYSQL_ROOT_PASSWORD']}\n")
    lab.private(ctrl / "app-client.cnf", f"[client]\nuser=app\npassword={env['APP_PASSWORD']}\n")
    return ids


if __name__ == "__main__":
    p = argparse.ArgumentParser(description=__doc__)
    p.add_argument("--inventory", type=Path, required=True)
    p.add_argument("--output", type=Path, default=HERE / "generated")
    args = p.parse_args()
    generate(json.loads(args.inventory.read_text()), args.output)
    print("Generated native configs and pinned SSH host keys. Register agents after server starts.")
