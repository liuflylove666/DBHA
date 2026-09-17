#!/usr/bin/env python3
"""Render an isolated DBHA laboratory; generated credentials stay out of git."""
import argparse
import importlib.util
import json
import os
from pathlib import Path
import secrets

HERE = Path(__file__).resolve().parent
MODULE = HERE.parents[1] / "dbm-services/common/dbha-v2"


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--enable-switching", action="store_true", help="Allow automatic failover in this isolated lab")
    args = parser.parse_args()
    os.umask(0o077)
    env_path = HERE / ".env"
    if env_path.exists():
        env = dict(line.split("=", 1) for line in env_path.read_text().splitlines() if line and not line.startswith("#"))
    else:
        env = {key: secrets.token_hex(16) for key in ("MYSQL_ROOT_PASSWORD", "MYSQL_REPL_PASSWORD", "DBHA_PASSWORD", "PROXY_ADMIN_PASSWORD", "SSH_PASSWORD", "API_TOKEN", "APP_PASSWORD")}
        env.update(MYSQL_REPL_USER="repl", DBHA_USER="dbha", PROXY_ADMIN_USER="admin", SSH_USER="root")
        env_path.write_text("".join(f"{key}={value}\n" for key, value in env.items()))
    spec = importlib.util.spec_from_file_location("upstream_render", MODULE / "scripts/render_configs.py")
    render = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(render)
    rc = MODULE / "etc/dbha-v2.server.rc.example"
    values = render.load_rc(rc)
    values.update({
        "COMMON_DISCOVERY_ENDPOINT": "10.203.80.11:2379", "COMMON_DISCOVERY_USER": "", "COMMON_DISCOVERY_PASSWORD": "",
        "COMMON_STORAGE_TCP_ENDPOINT": "10.203.80.10:3306", "COMMON_STORAGE_USER": "root", "COMMON_STORAGE_PASSWORD": env["MYSQL_ROOT_PASSWORD"],
        "COMMON_LOG_LEVEL": "info", "RECEIVER_SOURCE_PROBE_ENABLE": "true", "RECEIVER_SOURCE_KAFKA_ENABLE": "false",
        "RECEIVER_SOURCE_PROBE_ENDPOINT": "10.203.80.14:50052", "RECEIVER_SOURCE_PROBE_GRPC_KEEP_ALIVE_MIN_TIME": "5s",
        "ADMIN_GRPC_LISTEN_ADDRESS": "10.203.80.13:50051", "ADMIN_WEB_LISTEN_ADDRESS": "10.203.80.13:50060",
        "ADMIN_PROBE_MYSQL_USER": env["DBHA_USER"], "ADMIN_PROBE_MYSQL_PASSWORD": env["DBHA_PASSWORD"],
        "ADMIN_PROBE_PROXY_ADMIN_USER": env["PROXY_ADMIN_USER"], "ADMIN_PROBE_PROXY_ADMIN_PASSWORD": env["PROXY_ADMIN_PASSWORD"],
        "ADMIN_PROBE_MYSQL_INTERVAL": "5s", "ADMIN_PROBE_MYSQL_REPL_DELAY_INTERVAL": "5s",
        "ANALYSIS_WORKFLOW_ENABLE_SWITCHING": str(args.enable_switching).lower(), "ANALYSIS_WORKFLOW_ENABLE_WHITE_LIST": "false",
        "ANALYSIS_WORKFLOW_DBM_API_METADATA_HASH_CNT": "1", "ANALYSIS_WORKFLOW_HOST_LEVEL_SWITCH_MAX_HOST_NUM": "1",
        "ANALYSIS_WORKFLOW_HOST_LEVEL_SWITCH_MAX_INSTANCE_NUM": "1", "ANALYSIS_WORKFLOW_READ_DB_METRIC_OFFSET_DURATION": "-30s",
        "ANALYSIS_WORKFLOW_SWITCHFLOW_SLAVE_ALLOWED_IGNORE_CHECK_SUM": "true",
        "ANALYSIS_WORKFLOW_SWITCHFLOW_SLAVE_ALLOWED_IGNORE_SLAVE_DELAY": "false",
        "ANALYSIS_WORKFLOW_SWITCHFLOW_SLAVE_ALLOWED_MAX_HEARTBEAT_DELAY": "120",
        "ANALYSIS_WORKFLOW_SWITCHFLOW_SLAVE_ALLOWED_MAX_SECONDS_BEHIND_MASTER": "30",
        "ANALYSIS_DATABASE_MYSQL_CTRL_USER": env["DBHA_USER"], "ANALYSIS_DATABASE_MYSQL_CTRL_PASSWORD": env["DBHA_PASSWORD"],
        "ANALYSIS_DATABASE_MYSQL_CTRL_DELEGATE_USER": env["PROXY_ADMIN_USER"], "ANALYSIS_DATABASE_MYSQL_CTRL_DELEGATE_PASSWORD": env["PROXY_ADMIN_PASSWORD"],
        "ANALYSIS_DETECTOR_CHECK_PROBE_PROCESS_CMD": "/usr/local/bin/dbha-probe health -j -c /etc/dbha/probe.yaml",
        "ANALYSIS_DETECTOR_SSH_PORT": "22", "ANALYSIS_DETECTOR_SSH_USER": env["SSH_USER"], "ANALYSIS_DETECTOR_SSH_PASSWORD": env["SSH_PASSWORD"],
    })
    api_paths = {"METADATA": "metadata", "UPDATE_STATUS": "update-status", "SWAP_MYSQL_ROLE": "swap-mysql-role"}
    for key in list(values):
        if key.startswith("COMMON_DBM_API_") and key.endswith("_API"):
            name = key[len("COMMON_DBM_API_"):-len("_API")]
            values[key] = "http://10.203.80.12:8080/api/v1/" + api_paths.get(name, "unsupported")
        elif key.startswith("COMMON_DBM_API_") and key.endswith("_TOKEN"):
            values[key] = env["API_TOKEN"]
    for index, service in enumerate(("ADMIN", "RECEIVER", "ANALYSIS")):
        values[f"{service}_APM_LISTEN_ADDRESS"] = f"http://10.203.80.{13 + index}:{50080 + index}"
        values[f"{service}_PID_FILE"] = f"/run/dbha/{service.lower()}.pid"
        values[f"{service}_LOG_PATH"] = f"/var/log/dbha/{service.lower()}.log"
    render.apply_yaml_snippet_files(values, rc)
    render.apply_receiver_source_probe_block(values, rc)
    render.apply_receiver_source_kafka_block(values, rc)
    render.apply_receiver_sink_mysql_block(values, rc)
    out = HERE / "generated"
    out.mkdir(exist_ok=True)
    for service in ("admin", "receiver", "analysis"):
        text = render.render_template((MODULE / f"etc/templates/{service}.yaml").read_text(), values)
        if render.find_missing_placeholders(text):
            raise ValueError(f"Unresolved template placeholders in {service}: {render.find_missing_placeholders(text)}")
        (out / f"{service}.yaml").write_text(text)
    for name, last, proxy, role in (("mysql-master", 21, False, "backend_master"), ("mysql-standby", 22, False, "backend_slave"), ("proxy1", 31, True, ""), ("proxy2", 32, True, "")):
        ip = f"10.203.80.{last}"
        endpoint = dict(proto="tcp", clusterType="tendbha", machineType="proxy" if proxy else "backend", instanceRole=role, accessLayer="proxy" if proxy else "storage", ip=ip, ports=["10000" if proxy else "3306"], adminPorts=["11000"] if proxy else [])
        harvest = dict(user=env["PROXY_ADMIN_USER"] if proxy else env["DBHA_USER"], password=env["PROXY_ADMIN_PASSWORD"] if proxy else env["DBHA_PASSWORD"], interval="5s", heartbeatInterval="1s", replDelayInterval="5s", timeout="3s", endpoints=[endpoint])
        config = dict(name="probe", version="lab", serviceID=name, pidFile="/run/dbha/probe.pid", reporter=dict(name="grpc", endpoint="10.203.80.14:50052", dataID=0, connTimeout="5s", bkCloudID=0), client=dict(pingTime="10s", pingTimeout="5s", receiverReconnectInterval="5s", receiverMaxReconnectAttempts=100, maxReceiveMessageSize=10485760, maxSendMessageSize=10485760), admin=dict(endpoints=["10.203.80.13:50051"], bkCloudID=0, localIP=ip, syncInterval="0s"), harvester={"mysqlProxyAdmin" if proxy else "mysql": harvest}, log=dict(path="/var/log/dbha/probe.log", level="info", fileCount=3, fileSize=20))
        if proxy:
            # Service-port authentication reaches MySQL; admin-port credentials differ.
            service_harvest = dict(harvest, user=env["DBHA_USER"], password=env["DBHA_PASSWORD"], endpoints=[dict(endpoint, adminPorts=[])])
            harvest["endpoints"] = [dict(endpoint, ports=[])]
            config["harvester"]["mysql"] = service_harvest
        (out / f"{name}-probe.yaml").write_text(json.dumps(config, indent=2) + "\n")
    print("Rendered configs; credentials preserved in .env. Automatic switching:", args.enable_switching)


if __name__ == "__main__":
    main()
