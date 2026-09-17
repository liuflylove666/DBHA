#!/usr/bin/env python3
"""Render an isolated three-cluster DBHA Docker laboratory."""
import argparse
import importlib.util
import json
import os
from pathlib import Path
import secrets

HERE = Path(__file__).resolve().parent
MODULE = HERE.parents[1] / "dbm-services/common/dbha-v2"
NET = "10.203.81"
CLUSTERS = (
    dict(number=1, id=101, domain="research-mysql-1.local", master=21, standby=22, proxies=(31, 32)),
    dict(number=2, id=102, domain="research-mysql-2.local", master=41, standby=42, proxies=(51, 52)),
    dict(number=3, id=103, domain="research-mysql-3.local", master=61, standby=62, proxies=(71, 72)),
)


def load_env(out):
    path = out / ".env"
    if path.exists():
        return dict(line.split("=", 1) for line in path.read_text().splitlines() if line and not line.startswith("#"))
    env = {key: secrets.token_hex(16) for key in (
        "MYSQL_ROOT_PASSWORD", "MYSQL_REPL_PASSWORD", "DBHA_PASSWORD", "PROXY_ADMIN_PASSWORD",
        "SSH_PASSWORD", "API_TOKEN", "APP_PASSWORD",
    )}
    env.update(MYSQL_REPL_USER="repl", DBHA_USER="dbha", PROXY_ADMIN_USER="admin", SSH_USER="root")
    path.write_text("".join(f"{key}={value}\n" for key, value in env.items()))
    return env


def render_control_configs(out, env, enable_switching):
    spec = importlib.util.spec_from_file_location("dbha_render_multi", MODULE / "scripts/render_configs.py")
    render = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(render)
    rc = MODULE / "etc/dbha-v2.server.rc.example"
    values = render.load_rc(rc)
    values.update({
        "COMMON_DISCOVERY_ENDPOINT": f"{NET}.11:2379", "COMMON_DISCOVERY_USER": "", "COMMON_DISCOVERY_PASSWORD": "",
        "COMMON_STORAGE_TCP_ENDPOINT": f"{NET}.10:3306", "COMMON_STORAGE_USER": "root", "COMMON_STORAGE_PASSWORD": env["MYSQL_ROOT_PASSWORD"],
        "COMMON_LOG_LEVEL": "info", "RECEIVER_SOURCE_PROBE_ENABLE": "true", "RECEIVER_SOURCE_KAFKA_ENABLE": "false",
        "RECEIVER_SOURCE_PROBE_ENDPOINT": f"{NET}.14:50052", "RECEIVER_SOURCE_PROBE_GRPC_KEEP_ALIVE_MIN_TIME": "5s",
        "ADMIN_GRPC_LISTEN_ADDRESS": f"{NET}.13:50051", "ADMIN_WEB_LISTEN_ADDRESS": f"{NET}.13:50060",
        "ADMIN_PROBE_MYSQL_USER": env["DBHA_USER"], "ADMIN_PROBE_MYSQL_PASSWORD": env["DBHA_PASSWORD"],
        "ADMIN_PROBE_PROXY_ADMIN_USER": env["PROXY_ADMIN_USER"], "ADMIN_PROBE_PROXY_ADMIN_PASSWORD": env["PROXY_ADMIN_PASSWORD"],
        "ADMIN_PROBE_MYSQL_INTERVAL": "5s", "ADMIN_PROBE_MYSQL_REPL_DELAY_INTERVAL": "5s",
        "ANALYSIS_WORKFLOW_ENABLE_SWITCHING": str(enable_switching).lower(), "ANALYSIS_WORKFLOW_ENABLE_WHITE_LIST": "false",
        "ANALYSIS_WORKFLOW_DBM_API_METADATA_HASH_CNT": "1",
        "ANALYSIS_WORKFLOW_HOST_LEVEL_SWITCH_MAX_HOST_NUM": "3", "ANALYSIS_WORKFLOW_HOST_LEVEL_SWITCH_MAX_INSTANCE_NUM": "3",
        "ANALYSIS_WORKFLOW_CLUSTER_LEVEL_SWITCH_MAX_INSTANCE_NUM": "3", "ANALYSIS_WORKFLOW_READ_DB_METRIC_OFFSET_DURATION": "-30s",
        "ANALYSIS_WORKFLOW_SWITCHFLOW_SLAVE_ALLOWED_IGNORE_CHECK_SUM": "true",
        "ANALYSIS_WORKFLOW_SWITCHFLOW_SLAVE_ALLOWED_IGNORE_SLAVE_DELAY": "false",
        "ANALYSIS_WORKFLOW_SWITCHFLOW_SLAVE_ALLOWED_MAX_HEARTBEAT_DELAY": "120",
        "ANALYSIS_WORKFLOW_SWITCHFLOW_SLAVE_ALLOWED_MAX_SECONDS_BEHIND_MASTER": "30",
        "ANALYSIS_DATABASE_MYSQL_CTRL_USER": env["DBHA_USER"], "ANALYSIS_DATABASE_MYSQL_CTRL_PASSWORD": env["DBHA_PASSWORD"],
        "ANALYSIS_DATABASE_MYSQL_CTRL_DELEGATE_USER": env["PROXY_ADMIN_USER"], "ANALYSIS_DATABASE_MYSQL_CTRL_DELEGATE_PASSWORD": env["PROXY_ADMIN_PASSWORD"],
        "ANALYSIS_DETECTOR_CHECK_PROBE_PROCESS_CMD": "/usr/local/bin/dbha-probe health -j -c /etc/dbha/probe.yaml",
        "ANALYSIS_DETECTOR_SSH_PORT": "22", "ANALYSIS_DETECTOR_SSH_USER": env["SSH_USER"], "ANALYSIS_DETECTOR_SSH_PASSWORD": env["SSH_PASSWORD"],
    })
    paths = {"METADATA": "metadata", "UPDATE_STATUS": "update-status", "SWAP_MYSQL_ROLE": "swap-mysql-role"}
    for key in list(values):
        if key.startswith("COMMON_DBM_API_") and key.endswith("_API"):
            values[key] = f"http://{NET}.12:8080/api/v1/" + paths.get(key[15:-4], "unsupported")
        elif key.startswith("COMMON_DBM_API_") and key.endswith("_TOKEN"):
            values[key] = env["API_TOKEN"]
    for index, service in enumerate(("ADMIN", "RECEIVER", "ANALYSIS")):
        values[f"{service}_APM_LISTEN_ADDRESS"] = f"http://{NET}.{13 + index}:{50080 + index}"
        values[f"{service}_PID_FILE"] = f"/run/dbha/{service.lower()}.pid"
        values[f"{service}_LOG_PATH"] = f"/var/log/dbha/{service.lower()}.log"
    render.apply_yaml_snippet_files(values, rc)
    render.apply_receiver_source_probe_block(values, rc)
    render.apply_receiver_source_kafka_block(values, rc)
    render.apply_receiver_sink_mysql_block(values, rc)
    for service in ("admin", "receiver", "analysis"):
        content = render.render_template((MODULE / f"etc/templates/{service}.yaml").read_text(), values)
        missing = render.find_missing_placeholders(content)
        if missing:
            raise ValueError(f"Unresolved placeholders in {service}: {missing}")
        (out / f"{service}.yaml").write_text(content)


def probe_config(name, ip, env, proxy=False, role=""):
    endpoint = dict(proto="tcp", clusterType="tendbha", machineType="proxy" if proxy else "backend",
                    instanceRole=role, accessLayer="proxy" if proxy else "storage", ip=ip,
                    ports=["10000" if proxy else "3306"], adminPorts=["11000"] if proxy else [])
    harvest = dict(user=env["PROXY_ADMIN_USER"] if proxy else env["DBHA_USER"],
                   password=env["PROXY_ADMIN_PASSWORD"] if proxy else env["DBHA_PASSWORD"], interval="5s",
                   heartbeatInterval="1s", replDelayInterval="5s", timeout="3s", endpoints=[endpoint])
    config = dict(
        name="probe", version="multi-lab", serviceID=name, pidFile="/run/dbha/probe.pid",
        reporter=dict(name="grpc", endpoint=f"{NET}.14:50052", dataID=0, connTimeout="5s", bkCloudID=0),
        client=dict(pingTime="10s", pingTimeout="5s", receiverReconnectInterval="5s", receiverMaxReconnectAttempts=100,
                    maxReceiveMessageSize=10485760, maxSendMessageSize=10485760),
        admin=dict(endpoints=[f"{NET}.13:50051"], bkCloudID=0, localIP=ip, syncInterval="0s"),
        harvester={"mysqlProxyAdmin" if proxy else "mysql": harvest},
        log=dict(path="/var/log/dbha/probe.log", level="info", fileCount=3, fileSize=20),
    )
    if proxy:
        config["harvester"]["mysql"] = dict(harvest, user=env["DBHA_USER"], password=env["DBHA_PASSWORD"],
                                               endpoints=[dict(endpoint, adminPorts=[])])
        harvest["endpoints"] = [dict(endpoint, ports=[])]
    return config


def render_seed(out):
    rows = []
    for c in CLUSTERS:
        nodes = ((c["master"], "backend", "storage", "backend_master", 3306, 0),
                 (c["standby"], "backend", "storage", "backend_slave", 3306, 0),
                 (c["proxies"][0], "proxy", "proxy", "", 10000, 11000),
                 (c["proxies"][1], "proxy", "proxy", "", 10000, 11000))
        for index, (last, machine, layer, role, port, admin_port) in enumerate(nodes, 1):
            rows.append(f"  ({c['id'] * 100 + index},0,1,{c['id']},'{c['domain']}','tendbha','{machine}','{layer}','{role}','running','{NET}.{last}',{port},{admin_port})")
    sql = "USE dbha_metadata;\n\nINSERT IGNORE INTO standalone_instances\n  (host_id,bk_cloud_id,bk_biz_id,cluster_id,cluster,cluster_type,machine_type,access_layer,instance_role,status,ip,port,admin_port)\nVALUES\n"
    (out / "seed.sql").write_text(sql + ",\n".join(rows) + ";\n")


def node_env():
    return {"MYSQL_ROOT_PASSWORD": "${MYSQL_ROOT_PASSWORD:?Run multicluster.py first}", "MYSQL_ROOT_HOST": "%",
            "MYSQL_REPL_USER": "${MYSQL_REPL_USER}", "MYSQL_REPL_PASSWORD": "${MYSQL_REPL_PASSWORD}",
            "DBHA_USER": "${DBHA_USER}", "DBHA_PASSWORD": "${DBHA_PASSWORD}", "APP_PASSWORD": "${APP_PASSWORD}",
            "SSH_PASSWORD": "${SSH_PASSWORD}"}


def runtime(command, dependencies, last=None, environment=None):
    result = dict(image="dbha-v2-lab-runtime:local", platform="linux/amd64", command=command,
                  volumes=[".:/etc/dbha:ro"], restart="no", depends_on=dependencies,
                  networks={"lab": {"ipv4_address": f"{NET}.{last}"}} if last else ["lab"])
    if environment:
        result["environment"] = environment
    return result


def mysql(name, last):
    return dict(
        image="dbha-v2-lab-mysql:local", platform="linux/amd64", environment=node_env(), restart="no",
        command=[f"--server-id={last}", "--log-bin=mysql-bin", "--gtid-mode=ON", "--enforce-gtid-consistency=ON",
                 "--log-slave-updates=ON", "--binlog-format=ROW", "--sync-binlog=1", "--innodb-flush-log-at-trx-commit=1",
                 "--default-authentication-plugin=mysql_native_password"],
        volumes=[f"{name}-data:/var/lib/mysql", f"./{name}-probe.yaml:/etc/dbha/probe.yaml:ro"],
        depends_on={"receiver": {"condition": "service_started"}}, networks={"lab": {"ipv4_address": f"{NET}.{last}"}},
        healthcheck={"test": ["CMD-SHELL", "MYSQL_PWD=$$MYSQL_ROOT_PASSWORD mysql -h127.0.0.1 -uroot -Nse 'SELECT 1' >/dev/null"],
                     "interval": "5s", "timeout": "5s", "retries": 40, "start_period": "30s"})


def render_compose(out):
    services = {
        "metadata-store": dict(image="mysql:8.0-debian", platform="linux/amd64",
            command=["--sql-mode=STRICT_TRANS_TABLES,ERROR_FOR_DIVISION_BY_ZERO,NO_ENGINE_SUBSTITUTION"],
            environment={"MYSQL_ROOT_PASSWORD": "${MYSQL_ROOT_PASSWORD}", "MYSQL_ROOT_HOST": "%"},
            volumes=["metadata-data:/var/lib/mysql", "../../../dbm-services/common/dbha-v2/tools/cmd/standalone-metadata/schema.sql:/docker-entrypoint-initdb.d/10-schema.sql:ro", "./seed.sql:/docker-entrypoint-initdb.d/20-seed.sql:ro"],
            networks={"lab": {"ipv4_address": f"{NET}.10"}},
            healthcheck={"test": ["CMD-SHELL", "MYSQL_PWD=$$MYSQL_ROOT_PASSWORD mysql -h127.0.0.1 -uroot -Nse 'SELECT COUNT(*) FROM dbha_metadata.standalone_instances' >/dev/null"], "interval": "5s", "timeout": "5s", "retries": 40, "start_period": "30s"}),
        "etcd": dict(image="gcr.io/etcd-development/etcd:v3.6.0",
            command=["/usr/local/bin/etcd", "--name=multi-lab", "--data-dir=/etcd-data", "--listen-client-urls=http://0.0.0.0:2379", f"--advertise-client-urls=http://{NET}.11:2379", "--listen-peer-urls=http://0.0.0.0:2380", f"--initial-advertise-peer-urls=http://{NET}.11:2380", f"--initial-cluster=multi-lab=http://{NET}.11:2380"],
            volumes=["etcd-data:/etcd-data"], networks={"lab": {"ipv4_address": f"{NET}.11"}},
            healthcheck={"test": ["CMD", "/usr/local/bin/etcdctl", "endpoint", "health"], "interval": "5s", "timeout": "5s", "retries": 20}),
    }
    services["metadata-api"] = runtime(["standalone-metadata"], {"metadata-store": {"condition": "service_healthy"}}, 12,
        {"MYSQL_DSN": f"root:${{MYSQL_ROOT_PASSWORD}}@tcp({NET}.10:3306)/dbha_metadata?parseTime=true", "API_TOKEN": "${API_TOKEN}"})
    services["metadata-api"]["healthcheck"] = {"test": ["CMD", "curl", "-fsS", "http://127.0.0.1:8080/healthz"], "interval": "5s", "timeout": "5s", "retries": 20}
    services["migrate"] = runtime(["dbha-admin", "migrate", "--type", "all", "-c", "/etc/dbha/admin.yaml"], {"metadata-store": {"condition": "service_healthy"}})
    services["admin"] = runtime(["dbha-admin", "-c", "/etc/dbha/admin.yaml"], {"migrate": {"condition": "service_completed_successfully"}, "etcd": {"condition": "service_healthy"}, "metadata-api": {"condition": "service_healthy"}}, 13)
    services["receiver"] = runtime(["dbha-receiver", "-c", "/etc/dbha/receiver.yaml"], {"migrate": {"condition": "service_completed_successfully"}, "etcd": {"condition": "service_healthy"}}, 14)
    analysis_deps = {"admin": {"condition": "service_started"}}
    for c in CLUSTERS:
        analysis_deps[f"cluster{c['number']}-bootstrap"] = {"condition": "service_completed_successfully"}
    services["analysis"] = runtime(["dbha-analysis", "-c", "/etc/dbha/analysis.yaml"], analysis_deps, 15)
    volumes = {"metadata-data": {}, "etcd-data": {}}
    host_port = 14306
    for c in CLUSTERS:
        prefix = f"cluster{c['number']}"
        master, standby = f"{prefix}-mysql-master", f"{prefix}-mysql-standby"
        services[master], services[standby] = mysql(master, c["master"]), mysql(standby, c["standby"])
        volumes[f"{master}-data"], volumes[f"{standby}-data"] = {}, {}
        bootstrap = f"{prefix}-bootstrap"
        services[bootstrap] = dict(image="dbha-v2-lab-mysql:local", platform="linux/amd64",
            entrypoint=["bash", "/usr/local/bin/mysql-replication.sh"],
            environment=dict(node_env(), MYSQL_MASTER=f"{NET}.{c['master']}", MYSQL_STANDBY=f"{NET}.{c['standby']}"),
            volumes=[f"{bootstrap}-state:/state"], depends_on={master: {"condition": "service_healthy"}, standby: {"condition": "service_healthy"}}, networks=["lab"])
        volumes[f"{bootstrap}-state"] = {}
        for index, last in enumerate(c["proxies"], 1):
            name = f"{prefix}-proxy{index}"
            services[name] = dict(image="dbha-v2-lab-proxy:local", platform="linux/amd64", restart="no",
                environment={"PROXY_ADMIN_USER": "${PROXY_ADMIN_USER}", "PROXY_ADMIN_PASSWORD": "${PROXY_ADMIN_PASSWORD}", "SSH_PASSWORD": "${SSH_PASSWORD}", "API_TOKEN": "${API_TOKEN}", "METADATA_URL": f"http://{NET}.12:8080/api/v1/metadata", "METADATA_CLUSTER_ADDRESS": c["domain"]},
                volumes=[f"./{name}-probe.yaml:/etc/dbha/probe.yaml:ro"], ports=[f"127.0.0.1:{host_port}:10000"],
                depends_on={"metadata-api": {"condition": "service_healthy"}, "receiver": {"condition": "service_started"}, bootstrap: {"condition": "service_completed_successfully"}},
                networks={"lab": {"ipv4_address": f"{NET}.{last}"}})
            host_port += 1
    compose = {"name": "dbha-v2-multi-lab", "services": services,
               "networks": {"lab": {"ipam": {"config": [{"subnet": f"{NET}.0/24"}]}}}, "volumes": volumes}
    (out / "compose.json").write_text(json.dumps(compose, indent=2) + "\n")


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--enable-switching", action="store_true", help="Allow automatic failover in all three clusters")
    args = parser.parse_args()
    os.umask(0o077)
    out = HERE / "generated-multi"
    out.mkdir(exist_ok=True)
    env = load_env(out)
    render_control_configs(out, env, args.enable_switching)
    render_seed(out)
    for c in CLUSTERS:
        prefix = f"cluster{c['number']}"
        nodes = ((f"{prefix}-mysql-master", c["master"], False, "backend_master"),
                 (f"{prefix}-mysql-standby", c["standby"], False, "backend_slave"),
                 (f"{prefix}-proxy1", c["proxies"][0], True, ""), (f"{prefix}-proxy2", c["proxies"][1], True, ""))
        for name, last, proxy, role in nodes:
            (out / f"{name}-probe.yaml").write_text(json.dumps(probe_config(name, f"{NET}.{last}", env, proxy, role), indent=2) + "\n")
    render_compose(out)
    print(f"Rendered 3 clusters in {out}; automatic switching: {args.enable_switching}")


if __name__ == "__main__":
    main()
