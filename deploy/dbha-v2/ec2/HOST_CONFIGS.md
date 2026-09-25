# EC2 七节点逐机配置文件参考

本文展示 [EC2 部署手册](README.md) 示例 inventory 为七台机器生成的完整配置内容。内容与当前 `configure.py` 输出一致，用于部署复核和故障排查。

> 不要把本文中的占位符直接复制到生产机器。运行 `python3 configure.py --inventory inventory.json` 生成真实 UUID、token、密码和 SSH 公钥；运行 `register.py` 后生成四个 `agent.token`。所有敏感文件必须保持 `0600`。

占位符含义：

| 占位符 | 来源 |
|---|---|
| `<ADMIN_TOKEN>` | `generated/_generated/admin.token` |
| `<*_AGENT_UUID>`、`<*_INSTANCE_UUID>` | `generated/_generated/identity.json` |
| `<*_AGENT_TOKEN>` | `register.py` 签发并写入对应业务节点目录 |
| `<*_PASSWORD>` | `generated/_generated/.env` |
| `<*_ED25519_PUBLIC_KEY>` | 经可信通道核对后的业务节点 SSH 主机公钥 |

三个 controller 的 `admin.token`、`ssh_known_hosts`、etcd 地址池和凭据配置相同；`node_id`、通告地址、`ETCD_NAME` 和监听地址各不相同。四个业务节点的 controller 地址池相同，`agent_id`、`advertise_host`、类型配置和 token 各不相同。

## 1. controller1（10.80.10.10）

配置来源：`generated/controller/`。安装的 systemd 单元：[`dbha-etcd.service`](systemd/dbha-etcd.service)、[`dbha-server.service`](systemd/dbha-server.service)。

### `generated/controller/server.json` → `/etc/dbha/server.json`

```json
{
  "environment_id": "lab",
  "http_listen": ":8080",
  "grpc_listen": ":50052",
  "etcd_endpoints": [
    "http://10.80.10.10:2379",
    "http://10.80.10.11:2379",
    "http://10.80.10.12:2379"
  ],
  "admin_token_file": "/etc/dbha/admin.token",
  "state_dir": "/srv/dbha/server",
  "log_file": "/var/log/dbha/operations.jsonl",
  "allowed_networks": [
    "10.80.10.21/32",
    "10.80.20.22/32",
    "10.80.10.31/32",
    "10.80.20.32/32"
  ],
  "max_replication_delay_seconds": 30,
  "failure_threshold": 3,
  "profiles": {
    "default": {
      "mysql_user": "dbha",
      "mysql_password": "<DBHA_PASSWORD>",
      "proxy_user": "admin",
      "proxy_password": "<PROXY_ADMIN_PASSWORD>",
      "ssh_user": "dbha",
      "ssh_password": "<SSH_PASSWORD>",
      "ssh_known_hosts_file": "/etc/dbha/ssh_known_hosts",
      "ssh_port": 22,
      "probe_health_command": "/opt/dbha/bin/dbha-probe discover-health -c /etc/dbha/discovery.json"
    }
  },
  "node_id": "controller1",
  "advertise_http": "http://10.80.10.10:8080",
  "advertise_grpc": "10.80.10.10:50052"
}
```

### `generated/controller/etcd.env` → `/etc/dbha/etcd.env`

```dotenv
ETCD_NAME=controller1
ETCD_DATA_DIR=/etcd-data
ETCD_LISTEN_CLIENT_URLS=http://10.80.10.10:2379
ETCD_ADVERTISE_CLIENT_URLS=http://10.80.10.10:2379
ETCD_LISTEN_PEER_URLS=http://10.80.10.10:2380
ETCD_INITIAL_ADVERTISE_PEER_URLS=http://10.80.10.10:2380
ETCD_INITIAL_CLUSTER=controller1=http://10.80.10.10:2380,controller2=http://10.80.10.11:2380,controller3=http://10.80.10.12:2380
```

### `generated/controller/admin.token` → `/etc/dbha/admin.token`

```text
<ADMIN_TOKEN>
```

### `generated/controller/ssh_known_hosts` → `/etc/dbha/ssh_known_hosts`

```text
10.80.10.21 ssh-ed25519 <MYSQL1_ED25519_PUBLIC_KEY>
10.80.20.22 ssh-ed25519 <MYSQL2_ED25519_PUBLIC_KEY>
10.80.10.31 ssh-ed25519 <PROXY1_ED25519_PUBLIC_KEY>
10.80.20.32 ssh-ed25519 <PROXY2_ED25519_PUBLIC_KEY>
```

### `generated/controller/root-client.cnf` → `/etc/dbha/root-client.cnf`

```ini
[client]
user=root
password=<MYSQL_ROOT_PASSWORD>
```

### `generated/controller/app-client.cnf` → `/etc/dbha/app-client.cnf`

```ini
[client]
user=app
password=<APP_PASSWORD>
```

### `generated/controller/bootstrap-replication.sql` → `/etc/dbha/bootstrap-replication.sql`

```sql
CHANGE REPLICATION SOURCE TO SOURCE_HOST='10.80.10.21', SOURCE_PORT=3306, SOURCE_USER='repl', SOURCE_PASSWORD='<MYSQL_REPL_PASSWORD>', SOURCE_AUTO_POSITION=1;
START REPLICA;
SET PERSIST read_only=ON;
SET PERSIST super_read_only=ON;
```

## 2. controller2（10.80.10.11）

配置来源：`generated/controller2/`。安装的 systemd 单元：[`dbha-etcd.service`](systemd/dbha-etcd.service)、[`dbha-server.service`](systemd/dbha-server.service)。

### `generated/controller2/server.json` → `/etc/dbha/server.json`

```json
{
  "environment_id": "lab",
  "http_listen": ":8080",
  "grpc_listen": ":50052",
  "etcd_endpoints": [
    "http://10.80.10.10:2379",
    "http://10.80.10.11:2379",
    "http://10.80.10.12:2379"
  ],
  "admin_token_file": "/etc/dbha/admin.token",
  "state_dir": "/srv/dbha/server",
  "log_file": "/var/log/dbha/operations.jsonl",
  "allowed_networks": [
    "10.80.10.21/32",
    "10.80.20.22/32",
    "10.80.10.31/32",
    "10.80.20.32/32"
  ],
  "max_replication_delay_seconds": 30,
  "failure_threshold": 3,
  "profiles": {
    "default": {
      "mysql_user": "dbha",
      "mysql_password": "<DBHA_PASSWORD>",
      "proxy_user": "admin",
      "proxy_password": "<PROXY_ADMIN_PASSWORD>",
      "ssh_user": "dbha",
      "ssh_password": "<SSH_PASSWORD>",
      "ssh_known_hosts_file": "/etc/dbha/ssh_known_hosts",
      "ssh_port": 22,
      "probe_health_command": "/opt/dbha/bin/dbha-probe discover-health -c /etc/dbha/discovery.json"
    }
  },
  "node_id": "controller2",
  "advertise_http": "http://10.80.10.11:8080",
  "advertise_grpc": "10.80.10.11:50052"
}
```

### `generated/controller2/etcd.env` → `/etc/dbha/etcd.env`

```dotenv
ETCD_NAME=controller2
ETCD_DATA_DIR=/etcd-data
ETCD_LISTEN_CLIENT_URLS=http://10.80.10.11:2379
ETCD_ADVERTISE_CLIENT_URLS=http://10.80.10.11:2379
ETCD_LISTEN_PEER_URLS=http://10.80.10.11:2380
ETCD_INITIAL_ADVERTISE_PEER_URLS=http://10.80.10.11:2380
ETCD_INITIAL_CLUSTER=controller1=http://10.80.10.10:2380,controller2=http://10.80.10.11:2380,controller3=http://10.80.10.12:2380
```

### `generated/controller2/admin.token` → `/etc/dbha/admin.token`

```text
<ADMIN_TOKEN>
```

### `generated/controller2/ssh_known_hosts` → `/etc/dbha/ssh_known_hosts`

```text
10.80.10.21 ssh-ed25519 <MYSQL1_ED25519_PUBLIC_KEY>
10.80.20.22 ssh-ed25519 <MYSQL2_ED25519_PUBLIC_KEY>
10.80.10.31 ssh-ed25519 <PROXY1_ED25519_PUBLIC_KEY>
10.80.20.32 ssh-ed25519 <PROXY2_ED25519_PUBLIC_KEY>
```

## 3. controller3（10.80.10.12）

配置来源：`generated/controller3/`。安装的 systemd 单元：[`dbha-etcd.service`](systemd/dbha-etcd.service)、[`dbha-server.service`](systemd/dbha-server.service)。

### `generated/controller3/server.json` → `/etc/dbha/server.json`

```json
{
  "environment_id": "lab",
  "http_listen": ":8080",
  "grpc_listen": ":50052",
  "etcd_endpoints": [
    "http://10.80.10.10:2379",
    "http://10.80.10.11:2379",
    "http://10.80.10.12:2379"
  ],
  "admin_token_file": "/etc/dbha/admin.token",
  "state_dir": "/srv/dbha/server",
  "log_file": "/var/log/dbha/operations.jsonl",
  "allowed_networks": [
    "10.80.10.21/32",
    "10.80.20.22/32",
    "10.80.10.31/32",
    "10.80.20.32/32"
  ],
  "max_replication_delay_seconds": 30,
  "failure_threshold": 3,
  "profiles": {
    "default": {
      "mysql_user": "dbha",
      "mysql_password": "<DBHA_PASSWORD>",
      "proxy_user": "admin",
      "proxy_password": "<PROXY_ADMIN_PASSWORD>",
      "ssh_user": "dbha",
      "ssh_password": "<SSH_PASSWORD>",
      "ssh_known_hosts_file": "/etc/dbha/ssh_known_hosts",
      "ssh_port": 22,
      "probe_health_command": "/opt/dbha/bin/dbha-probe discover-health -c /etc/dbha/discovery.json"
    }
  },
  "node_id": "controller3",
  "advertise_http": "http://10.80.10.12:8080",
  "advertise_grpc": "10.80.10.12:50052"
}
```

### `generated/controller3/etcd.env` → `/etc/dbha/etcd.env`

```dotenv
ETCD_NAME=controller3
ETCD_DATA_DIR=/etcd-data
ETCD_LISTEN_CLIENT_URLS=http://10.80.10.12:2379
ETCD_ADVERTISE_CLIENT_URLS=http://10.80.10.12:2379
ETCD_LISTEN_PEER_URLS=http://10.80.10.12:2380
ETCD_INITIAL_ADVERTISE_PEER_URLS=http://10.80.10.12:2380
ETCD_INITIAL_CLUSTER=controller1=http://10.80.10.10:2380,controller2=http://10.80.10.11:2380,controller3=http://10.80.10.12:2380
```

### `generated/controller3/admin.token` → `/etc/dbha/admin.token`

```text
<ADMIN_TOKEN>
```

### `generated/controller3/ssh_known_hosts` → `/etc/dbha/ssh_known_hosts`

```text
10.80.10.21 ssh-ed25519 <MYSQL1_ED25519_PUBLIC_KEY>
10.80.20.22 ssh-ed25519 <MYSQL2_ED25519_PUBLIC_KEY>
10.80.10.31 ssh-ed25519 <PROXY1_ED25519_PUBLIC_KEY>
10.80.20.32 ssh-ed25519 <PROXY2_ED25519_PUBLIC_KEY>
```

## 4. mysql1（10.80.10.21）

配置来源：`generated/mysql1/`。安装的 systemd 单元：[`dbha-mysql.service`](systemd/dbha-mysql.service)、[`dbha-probe.service`](systemd/dbha-probe.service)。

### `generated/mysql1/discovery.json` → `/etc/dbha/discovery.json`

```json
{
  "server_url": "http://10.80.10.10:8080",
  "server_grpc": "10.80.10.10:50052",
  "server_urls": [
    "http://10.80.10.10:8080",
    "http://10.80.10.11:8080",
    "http://10.80.10.12:8080"
  ],
  "server_grpc_endpoints": [
    "10.80.10.10:50052",
    "10.80.10.11:50052",
    "10.80.10.12:50052"
  ],
  "token_file": "/etc/dbha/agent.token",
  "agent_id": "<MYSQL1_AGENT_UUID>",
  "state_file": "/srv/dbha/probe/identity.json",
  "kind": "mysql",
  "advertise_host": "10.80.10.21",
  "route_reconcile_signal_file": "/run/dbha/route-reconcile.json",
  "mysql": {
    "network": "tcp",
    "address": "127.0.0.1:3306",
    "user": "dbha",
    "password_file": "/etc/dbha/dbha.password",
    "port": 3306
  }
}
```

### `generated/mysql1/agent.token` → `/etc/dbha/agent.token`

```text
<MYSQL1_AGENT_TOKEN>
```

### `generated/mysql1/dbha.password` → `/etc/dbha/dbha.password`

```text
<DBHA_PASSWORD>
```

### `generated/mysql1/proxy-admin.password` → `/etc/dbha/proxy-admin.password`

```text
<PROXY_ADMIN_PASSWORD>
```

### `generated/mysql1/mysql.env` → `/etc/dbha/mysql.env`

```dotenv
MYSQL_ROOT_PASSWORD=<MYSQL_ROOT_PASSWORD>
MYSQL_ROOT_HOST=%
```

### `generated/mysql1/mysql.cnf` → `/etc/dbha/mysql.cnf`

```ini
[mysqld]
bind-address=0.0.0.0
server-id=21
log-bin=mysql-bin
gtid-mode=ON
enforce-gtid-consistency=ON
log-slave-updates=ON
binlog-format=ROW
sync-binlog=1
innodb-flush-log-at-trx-commit=1
default-authentication-plugin=mysql_native_password
```

### `generated/mysql1/mysql-init/10-accounts.sql` → `/etc/dbha/mysql-init/10-accounts.sql`

```sql
SET SESSION sql_log_bin=0;
CREATE DATABASE IF NOT EXISTS lab;
CREATE USER 'dbha'@'%' IDENTIFIED BY '<DBHA_PASSWORD>';
GRANT ALL PRIVILEGES ON *.* TO 'dbha'@'%';
CREATE USER 'repl'@'%' IDENTIFIED BY '<MYSQL_REPL_PASSWORD>';
GRANT REPLICATION SLAVE, REPLICATION CLIENT ON *.* TO 'repl'@'%';
CREATE USER 'app'@'%' IDENTIFIED BY '<APP_PASSWORD>';
GRANT ALL PRIVILEGES ON lab.* TO 'app'@'%';
```

### `generated/mysql1/ssh-password` → `/tmp/dbha-config/ssh-password`

```text
<SSH_PASSWORD>
```

## 5. mysql2（10.80.20.22）

配置来源：`generated/mysql2/`。安装的 systemd 单元：[`dbha-mysql.service`](systemd/dbha-mysql.service)、[`dbha-probe.service`](systemd/dbha-probe.service)。

### `generated/mysql2/discovery.json` → `/etc/dbha/discovery.json`

```json
{
  "server_url": "http://10.80.10.10:8080",
  "server_grpc": "10.80.10.10:50052",
  "server_urls": [
    "http://10.80.10.10:8080",
    "http://10.80.10.11:8080",
    "http://10.80.10.12:8080"
  ],
  "server_grpc_endpoints": [
    "10.80.10.10:50052",
    "10.80.10.11:50052",
    "10.80.10.12:50052"
  ],
  "token_file": "/etc/dbha/agent.token",
  "agent_id": "<MYSQL2_AGENT_UUID>",
  "state_file": "/srv/dbha/probe/identity.json",
  "kind": "mysql",
  "advertise_host": "10.80.20.22",
  "route_reconcile_signal_file": "/run/dbha/route-reconcile.json",
  "mysql": {
    "network": "tcp",
    "address": "127.0.0.1:3306",
    "user": "dbha",
    "password_file": "/etc/dbha/dbha.password",
    "port": 3306
  }
}
```

### `generated/mysql2/agent.token` → `/etc/dbha/agent.token`

```text
<MYSQL2_AGENT_TOKEN>
```

### `generated/mysql2/dbha.password` → `/etc/dbha/dbha.password`

```text
<DBHA_PASSWORD>
```

### `generated/mysql2/proxy-admin.password` → `/etc/dbha/proxy-admin.password`

```text
<PROXY_ADMIN_PASSWORD>
```

### `generated/mysql2/mysql.env` → `/etc/dbha/mysql.env`

```dotenv
MYSQL_ROOT_PASSWORD=<MYSQL_ROOT_PASSWORD>
MYSQL_ROOT_HOST=%
```

### `generated/mysql2/mysql.cnf` → `/etc/dbha/mysql.cnf`

```ini
[mysqld]
bind-address=0.0.0.0
server-id=22
log-bin=mysql-bin
gtid-mode=ON
enforce-gtid-consistency=ON
log-slave-updates=ON
binlog-format=ROW
sync-binlog=1
innodb-flush-log-at-trx-commit=1
default-authentication-plugin=mysql_native_password
```

### `generated/mysql2/mysql-init/10-accounts.sql` → `/etc/dbha/mysql-init/10-accounts.sql`

```sql
SET SESSION sql_log_bin=0;
CREATE DATABASE IF NOT EXISTS lab;
CREATE USER 'dbha'@'%' IDENTIFIED BY '<DBHA_PASSWORD>';
GRANT ALL PRIVILEGES ON *.* TO 'dbha'@'%';
CREATE USER 'repl'@'%' IDENTIFIED BY '<MYSQL_REPL_PASSWORD>';
GRANT REPLICATION SLAVE, REPLICATION CLIENT ON *.* TO 'repl'@'%';
CREATE USER 'app'@'%' IDENTIFIED BY '<APP_PASSWORD>';
GRANT ALL PRIVILEGES ON lab.* TO 'app'@'%';
```

### `generated/mysql2/ssh-password` → `/tmp/dbha-config/ssh-password`

```text
<SSH_PASSWORD>
```

## 6. proxy1（10.80.10.31）

配置来源：`generated/proxy1/`。安装的 systemd 单元：[`dbha-probe.service`](systemd/dbha-probe.service)、[`dbha-proxy.service`](systemd/dbha-proxy.service)。

### `generated/proxy1/discovery.json` → `/etc/dbha/discovery.json`

```json
{
  "server_url": "http://10.80.10.10:8080",
  "server_grpc": "10.80.10.10:50052",
  "server_urls": [
    "http://10.80.10.10:8080",
    "http://10.80.10.11:8080",
    "http://10.80.10.12:8080"
  ],
  "server_grpc_endpoints": [
    "10.80.10.10:50052",
    "10.80.10.11:50052",
    "10.80.10.12:50052"
  ],
  "token_file": "/etc/dbha/agent.token",
  "agent_id": "<PROXY1_AGENT_UUID>",
  "state_file": "/srv/dbha/probe/identity.json",
  "kind": "proxy",
  "advertise_host": "10.80.10.31",
  "route_reconcile_signal_file": "/run/dbha/route-reconcile.json",
  "proxy": {
    "uuid": "<PROXY1_INSTANCE_UUID>",
    "data_port": 10000,
    "admin_port": 11000,
    "admin_network": "tcp",
    "admin_address": "127.0.0.1:11000",
    "admin_user": "admin",
    "admin_password_file": "/etc/dbha/proxy-admin.password"
  }
}
```

### `generated/proxy1/agent.token` → `/etc/dbha/agent.token`

```text
<PROXY1_AGENT_TOKEN>
```

### `generated/proxy1/dbha.password` → `/etc/dbha/dbha.password`

```text
<DBHA_PASSWORD>
```

### `generated/proxy1/proxy-admin.password` → `/etc/dbha/proxy-admin.password`

```text
<PROXY_ADMIN_PASSWORD>
```

### `generated/proxy1/proxy.env` → `/etc/dbha/proxy.env`

```dotenv
PROXY_ADMIN_USER=admin
PROXY_ADMIN_PASSWORD=<PROXY_ADMIN_PASSWORD>
```

### `generated/proxy1/ssh-password` → `/tmp/dbha-config/ssh-password`

```text
<SSH_PASSWORD>
```

## 7. proxy2（10.80.20.32）

配置来源：`generated/proxy2/`。安装的 systemd 单元：[`dbha-probe.service`](systemd/dbha-probe.service)、[`dbha-proxy.service`](systemd/dbha-proxy.service)。

### `generated/proxy2/discovery.json` → `/etc/dbha/discovery.json`

```json
{
  "server_url": "http://10.80.10.10:8080",
  "server_grpc": "10.80.10.10:50052",
  "server_urls": [
    "http://10.80.10.10:8080",
    "http://10.80.10.11:8080",
    "http://10.80.10.12:8080"
  ],
  "server_grpc_endpoints": [
    "10.80.10.10:50052",
    "10.80.10.11:50052",
    "10.80.10.12:50052"
  ],
  "token_file": "/etc/dbha/agent.token",
  "agent_id": "<PROXY2_AGENT_UUID>",
  "state_file": "/srv/dbha/probe/identity.json",
  "kind": "proxy",
  "advertise_host": "10.80.20.32",
  "route_reconcile_signal_file": "/run/dbha/route-reconcile.json",
  "proxy": {
    "uuid": "<PROXY2_INSTANCE_UUID>",
    "data_port": 10000,
    "admin_port": 11000,
    "admin_network": "tcp",
    "admin_address": "127.0.0.1:11000",
    "admin_user": "admin",
    "admin_password_file": "/etc/dbha/proxy-admin.password"
  }
}
```

### `generated/proxy2/agent.token` → `/etc/dbha/agent.token`

```text
<PROXY2_AGENT_TOKEN>
```

### `generated/proxy2/dbha.password` → `/etc/dbha/dbha.password`

```text
<DBHA_PASSWORD>
```

### `generated/proxy2/proxy-admin.password` → `/etc/dbha/proxy-admin.password`

```text
<PROXY_ADMIN_PASSWORD>
```

### `generated/proxy2/proxy.env` → `/etc/dbha/proxy.env`

```dotenv
PROXY_ADMIN_USER=admin
PROXY_ADMIN_PASSWORD=<PROXY_ADMIN_PASSWORD>
```

### `generated/proxy2/ssh-password` → `/tmp/dbha-config/ssh-password`

```text
<SSH_PASSWORD>
```

## 8. 文件权限和运行时状态

- `install-host.sh` 将密钥、token、密码、JSON 和环境文件以 `0600` 安装；`mysql.cnf` 与 systemd 单元为 `0644`。
- `/etc/dbha` 为 `0700`。controller 的服务状态写入 `/srv/dbha/server`，etcd 数据写入 `/srv/dbha/etcd`。
- MySQL 数据写入 `/srv/dbha/mysql`；Probe 身份与会话水位写入 `/srv/dbha/probe/identity.json`，它们不是静态配置，不能在机器间复制。
- `/run/dbha/route-reconcile.json` 是 Probe 与 Proxy supervisor 的运行时信号文件，不需要预创建内容。
- `ssh-password` 仅用于设置受限 `dbha` 系统账户，设置完成后必须从 `/tmp/dbha-config` 删除。
