# EC2 Docker 部署：三控制节点、自动发现与无 VIP 高可用

本文说明当前推荐的 EC2 部署方式：三台 controller 组成三节点 etcd，并运行三个常驻 `dbha-server`；两台 MySQL 和两台蓝鲸 Proxy 作为业务节点。所有客户端使用 controller 私网地址池，不使用 VIP、Keepalived、DNS 切换或云 API。默认使用隔离 VPC 内的明文 HTTP/gRPC，不要求 TLS。

当前交付采用宿主机 systemd 与 Docker 的组合：

| 节点 | 数量 | systemd 直接运行 | Docker 容器 |
|---|---:|---|---|
| controller | 3 | `dbha-server` | etcd 3.6.0 |
| MySQL | 2 | `dbha-probe` | MySQL 8.0 |
| Proxy | 2 | `dbha-probe` | 蓝鲸 MySQL Proxy + supervisor |

`dbha-server` 和 `dbha-probe` 是静态 Linux/amd64 二进制，数据服务由 Docker 运行。生产 EC2 不使用仓库中的实验 Compose 文件。管理 MySQL、standalone metadata、admin、receiver 和 analysis 不参与新部署。当前代码和本机故障切换测试已通过，真实 EC2 发布前仍须完成本文的三节点故障演练。

## 1. 网络、主机和安全组

推荐 Ubuntu 24.04 amd64。七台机器必须使用唯一私网 IPv4，并保持时间同步。将 [inventory.example.json](inventory.example.json) 复制为 `inventory.json`：

```json
{
  "controller": "10.80.10.10",
  "controllers": ["10.80.10.10", "10.80.10.11", "10.80.10.12"],
  "mysql1": "10.80.10.21",
  "mysql2": "10.80.20.22",
  "proxy1": "10.80.10.31",
  "proxy2": "10.80.20.32"
}
```

`controller` 必须等于 `controllers` 的第一项。删除 `controllers` 可生成兼容的单 controller 五机配置，但该模式没有控制端高可用，不作为生产推荐拓扑。

安全组仅开放私网流量：

| 端口 | 目标节点 | 允许来源 | 用途 |
|---:|---|---|---|
| 22/TCP | 全部 | 管理网；controller 到四台业务节点 | 运维与故障复核 |
| 8080/TCP | controller | 管理网、四台业务节点 | HTTP 管理、注册、路由 API |
| 50052/TCP | controller | 四台业务节点 | probe gRPC 上报 |
| 2379/TCP | controller | 三台 controller | etcd client |
| 2380/TCP | controller | 三台 controller | etcd peer |
| 3306/TCP | MySQL | controller、Proxy、MySQL 对端 | 核验、业务后端与复制 |
| 10000/TCP | Proxy | 应用网、controller | 应用数据端口与核验 |
| 11000/TCP | Proxy | controller | Proxy 管理端口 |

不要把 8080、50052、2379、2380、3306 或 11000 暴露到公网。由于本方案不启用 TLS，安全组、私网路由和主机防火墙是访问边界。

所有主机安装 Docker Engine、systemd、Python 3 和 OpenSSH，并执行 `sudo systemctl enable --now docker`。controller1 额外安装 `mysql-client`。构建机需要 Docker Buildx；如果直接在 Ubuntu amd64 仓库目录执行 `make build`，也可使用本机 Go 1.26。

## 2. 构建并固定部署产物

以下命令在仓库根目录执行。推荐用 Docker 构建 Linux/amd64 二进制，避免构建机架构影响：

```bash
release=$(git rev-parse --short HEAD)
mkdir -p deploy/dbha-v2/ec2/dist

docker buildx build --platform linux/amd64 --target runtime \
  -f deploy/dbha-v2/Dockerfile \
  -t "dbha-v2-runtime:${release}" --load .

cid=$(docker create "dbha-v2-runtime:${release}")
docker cp "$cid:/usr/local/bin/dbha-server" deploy/dbha-v2/ec2/dist/dbha-server
docker cp "$cid:/usr/local/bin/dbha-probe" deploy/dbha-v2/ec2/dist/dbha-probe
docker rm "$cid"
chmod 0755 deploy/dbha-v2/ec2/dist/dbha-server deploy/dbha-v2/ec2/dist/dbha-probe

docker buildx build --platform linux/amd64 \
  -t dbha-ec2-proxy:local --load deploy/dbha-v2/ec2/proxy
docker save dbha-ec2-proxy:local | gzip -c > deploy/dbha-v2/ec2/dist/dbha-ec2-proxy.tar.gz

(cd deploy/dbha-v2/ec2/dist && \
  sha256sum dbha-server dbha-probe dbha-ec2-proxy.tar.gz > SHA256SUMS)
```

`proxy/Dockerfile` 会校验蓝鲸 Proxy 发布包的固定 SHA256。三台 controller 必须使用同一份 `dbha-server`，四台业务节点必须使用同一份 `dbha-probe`。分发后在每台机器重新校验对应文件的 SHA256。

## 3. 生成配置

在构建机或受控运维机执行：

```bash
cd deploy/dbha-v2/ec2
cp inventory.example.json inventory.json
# 编辑 inventory.json 后生成；默认输出到 generated/
python3 configure.py --inventory inventory.json
python3 -m unittest test_configure.py
```

首次生成时，脚本通过 `ssh-keyscan` 取得四台业务节点的 ed25519 主机公钥。必须通过 EC2 串口控制台或可信管理通道核对指纹；不一致时停止部署。生成器会保留 deployment/agent UUID、Proxy UUID、管理员 token、密码和 SSH 固定公钥，使用相同 inventory 重跑不会重置身份。修改已生成环境的 IP 会被拒绝。

关键输出：

```text
generated/
├── controller/       # controller1 配置及复制初始化文件
├── controller2/      # controller2 配置
├── controller3/      # controller3 配置
├── mysql1/
├── mysql2/
├── proxy1/
├── proxy2/
└── _generated/       # 身份、管理员 token 和内部生成文件
```

所有私密文件模式为 0600。`generated/`、`inventory.json` 和 `dist/` 不得提交 Git。备份 `generated/_generated/identity.json` 与 `generated/_generated/admin.token`；丢失后不要重新生成一套身份覆盖现有 etcd 数据。

## 4. 安装三个 controller

先只分发 controller 配置。业务节点的 `agent.token` 要在控制端启动并完成注册后才会生成。

每台 controller 需要：

- `dist/dbha-server`
- 本机对应的 `generated/controller[N]/`
- `install-host.sh` 和完整 `systemd/` 目录

以 controller1 为例，在目标机执行：

```bash
sudo install -d -m 0755 /opt/dbha/bin
sudo install -m 0755 /tmp/dbha-server /opt/dbha/bin/dbha-server
sudo bash /tmp/dbha-ec2/install-host.sh controller /tmp/dbha-config
sudo docker pull gcr.io/etcd-development/etcd:v3.6.0
sudo systemctl enable dbha-etcd dbha-server
```

controller2、controller3 分别使用角色名 `controller2`、`controller3` 和对应配置目录。`install-host.sh` 会安装 `/etc/dbha` 下的 server/etcd 配置，创建 `/srv/dbha/etcd`、`/srv/dbha/server`，并安装两个 systemd 单元。

先在三台机器启动 etcd：

```bash
sudo systemctl start dbha-etcd
```

在任一 controller 检查三成员和 quorum：

```bash
endpoints=http://10.80.10.10:2379,http://10.80.10.11:2379,http://10.80.10.12:2379
sudo docker exec dbha-etcd /usr/local/bin/etcdctl \
  --endpoints="$endpoints" endpoint health --cluster
sudo docker exec dbha-etcd /usr/local/bin/etcdctl \
  --endpoints="$endpoints" endpoint status --cluster -w table
sudo docker exec dbha-etcd /usr/local/bin/etcdctl \
  --endpoints="$endpoints" member list -w table
```

三个成员健康后，在三台机器启动 server：

```bash
sudo systemctl start dbha-server
```

所有 `/healthz` 应返回 200，只有一个 `/readyz` 返回 200；另外两个 follower 返回 503：

```bash
for ip in 10.80.10.10 10.80.10.11 10.80.10.12; do
  printf '%s health=%s ready=%s\n' "$ip" \
    "$(curl -sS -o /dev/null -w '%{http_code}' "http://$ip:8080/healthz")" \
    "$(curl -sS -o /dev/null -w '%{http_code}' "http://$ip:8080/readyz")"
done
```

follower 的业务 API 返回 `503 NOT_LEADER`。这是正常状态，不应把 follower 从客户端地址池删除。

## 5. 注册 deployment 和四个 agent

控制端健康后，在保存 `generated/` 的运维机运行一次幂等注册：

```bash
cd deploy/dbha-v2/ec2
python3 register.py --output generated --servers \
  http://10.80.10.10:8080 \
  http://10.80.10.11:8080 \
  http://10.80.10.12:8080
```

脚本只在连接失败、超时或明确收到 `NOT_LEADER` 时换址。它创建 deployment、签发 agent 凭据并把 token 写入各业务节点目录，不填写 MySQL 主备角色或 Proxy 后端。重复执行复用相同 deployment 和已存在的 token。

确认以下文件均存在后再安装业务节点：

```bash
test -s generated/mysql1/agent.token
test -s generated/mysql2/agent.token
test -s generated/proxy1/agent.token
test -s generated/proxy2/agent.token
```

## 6. 安装 MySQL 节点

向 mysql1/mysql2 分发 `dist/dbha-probe`、对应的 `generated/mysqlN/`、`install-host.sh` 和 `systemd/`。每台 MySQL 节点执行，角色名按本机选择：

```bash
sudo install -d -m 0755 /opt/dbha/bin
sudo install -m 0755 /tmp/dbha-probe /opt/dbha/bin/dbha-probe
sudo bash /tmp/dbha-ec2/install-host.sh mysql1 /tmp/dbha-config
sudo docker pull mysql:8.0-debian
sudo systemctl enable dbha-mysql dbha-probe
```

安装器会创建 `/srv/dbha/mysql`、`/srv/dbha/probe`，并安装 MySQL 初始化账户、`mysql.cnf`、discovery 配置和 probe token。首次初始化前确认数据盘为空；已有 MySQL 或复制关系不能套用本初始化流程。

当前生成配置使用受限的 `dbha` SSH 密码账户执行故障复核。密码取本机配置目录的 `ssh-password`：

```bash
sudo sh -c 'printf "dbha:%s\n" "$(cat /tmp/dbha-config/ssh-password)" | chpasswd'
sudo rm -f /tmp/dbha-config/ssh-password
```

只对 `dbha` 用户开放密码登录，并把 22/TCP 限制到 controller 私网地址。禁止 root 密码登录。随后从 controller 验证固定主机公钥：

```bash
sudo -u dbha ssh \
  -o StrictHostKeyChecking=yes \
  -o UserKnownHostsFile=/etc/dbha/ssh_known_hosts \
  dbha@10.80.10.21 true
```

启动两台 MySQL：

```bash
sudo systemctl start dbha-mysql
sudo docker exec dbha-mysql sh -c \
  'mysqladmin ping -h127.0.0.1 -uroot -p"$MYSQL_ROOT_PASSWORD"'
```

仅对于两台全新实例，在 controller1 先确认 mysql2 没有现有复制通道、两侧 GTID 已开启，然后初始化复制：

```bash
sudo mysql --defaults-extra-file=/etc/dbha/root-client.cnf -h10.80.20.22 \
  -e 'SHOW REPLICA STATUS\G'
sudo mysql --defaults-extra-file=/etc/dbha/root-client.cnf -h10.80.20.22 \
  < /etc/dbha/bootstrap-replication.sql
sudo mysql --defaults-extra-file=/etc/dbha/root-client.cnf -h10.80.20.22 \
  -e 'SHOW REPLICA STATUS\G'
```

最后启动两台 MySQL probe：

```bash
sudo systemctl start dbha-probe
sudo -u dbha /opt/dbha/bin/dbha-probe discover-health -c /etc/dbha/discovery.json
```

## 7. 安装 Proxy 节点

向 proxy1/proxy2 分发 `dist/dbha-probe`、`dist/dbha-ec2-proxy.tar.gz`、对应的 `generated/proxyN/`、`install-host.sh` 和 `systemd/`。每台 Proxy 节点执行：

```bash
sudo install -d -m 0755 /opt/dbha/bin
sudo install -m 0755 /tmp/dbha-probe /opt/dbha/bin/dbha-probe
gzip -dc /tmp/dbha-ec2-proxy.tar.gz | sudo docker load
sudo bash /tmp/dbha-ec2/install-host.sh proxy1 /tmp/dbha-config
sudo systemctl enable dbha-probe dbha-proxy
```

同样设置 `dbha` SSH 密码并从三个 controller 校验固定主机公钥。然后先启动 probe，再启动 Proxy：

```bash
sudo systemctl start dbha-probe
sudo systemctl start dbha-proxy
```

Proxy 容器内 supervisor 会从 `server_urls` 地址池取得启动许可，使用控制端确认的当前主库启动 Proxy，并报告完成。probe 写入 `/run/dbha/route-reconcile.json` 时，supervisor 会停止本机受控 Proxy 并重新取许可；宿主机 probe 和 SSH 保持运行。

## 8. 验收与启用切换

在任一 controller 使用同一个静态地址池查询：

```bash
servers=http://10.80.10.10:8080,http://10.80.10.11:8080,http://10.80.10.12:8080
sudo -u dbha /opt/dbha/bin/dbha-server -c /etc/dbha/server.json ctl \
  --servers "$servers" GET /api/v1/clusters
```

验收条件：

- 集群状态为 `READY`，`recovery_gate=NONE`。
- 主备 UUID、复制方向和 endpoint 与实际 MySQL 一致。
- 两个 Proxy 都是 ACTIVE，路由指向同一个正式主库。
- 两个 Proxy 的 10000 端口均能完成应用写入、读取和 `SELECT @@server_id`。
- 三台 server 仅一个 `/readyz=200`，三个 `/healthz=200`。
- 自动切换仍为关闭状态。

显式启用切换前，从集群响应取得当前 `cluster_id` 和 `topology_epoch`：

```bash
cat >/tmp/enable-switching.json <<'JSON'
{"expected_epoch":0,"enabled":true}
JSON

sudo -u dbha /opt/dbha/bin/dbha-server -c /etc/dbha/server.json ctl \
  --servers "$servers" --data-file /tmp/enable-switching.json \
  PUT /api/v1/clusters/1/switching
```

示例中的 `cluster_id=1` 和 `expected_epoch=0` 必须替换为实时值。管理 CLI 只对连接失败和 `NOT_LEADER` 换址，不会用其他节点掩盖鉴权、版本冲突或业务错误。

## 9. 无 VIP HA 故障演练

`dbha-server` 使用 30 秒 etcd owner lease。正常停止会主动释放 owner，硬故障则在 lease 到期后接管。最近本机双进程实测的硬故障接管时间为约 29 秒。

计划维护：

```bash
# 在当前 leader 上执行
sudo systemctl stop dbha-server
```

确认另一台 controller 的 `/readyz` 变为 200，Probe、Proxy supervisor 和管理 CLI 自动切换地址。重新启动原节点后，它应作为 follower 加入：

```bash
sudo systemctl start dbha-server
```

硬故障演练应在维护窗口停止或关机当前 leader EC2，而不是只向 systemd 托管进程发送 `SIGKILL`，因为 `Restart=on-failure` 会自动拉起进程。验收以下场景：

1. 关闭一个 controller：etcd 保持 2/3 quorum，standby 在租约窗口内接管。
2. 仅停止 leader 本机 etcd：server 可使用另外两个 etcd endpoint；不得出现两个 `/readyz=200`。
3. 恢复节点：它同步当前 owner heartbeat 和水位后保持 follower。
4. 同时失去两个 etcd 成员：控制端 ready 失败并停止新切换，现有 Proxy 路由不被自动改写。

每次演练都检查 `/api/v1/leader`、`/metrics` 中的 `dbha_server_leader`、etcd member/quorum 和两个 Proxy 的实际后端。

## 10. 滚动升级

始终先备份 etcd 和 controller 水位。三个 server 使用同一提交构建的二进制，逐台执行：

```bash
sudo systemctl stop dbha-server
sudo install -m 0755 /tmp/dbha-server.new /opt/dbha/bin/dbha-server
sudo systemctl start dbha-server
curl -fsS http://127.0.0.1:8080/healthz
```

先升级 follower，确认其重新同步后再处理另一 follower，最后切走并升级 leader。不要同时停止两个 etcd 成员。业务节点的 probe 也逐台升级；升级 Proxy 镜像时先确认另一 Proxy 可用，并保持旧主隔离状态不变。

## 11. 备份与三节点 etcd 恢复

每天至少保存一次 etcd snapshot，并分别备份三个 controller 的 `/srv/dbha/server/control-watermark.json`。示例在 controller1 执行：

```bash
stamp=$(date -u +%Y%m%dT%H%M%SZ)
sudo install -d -m 0700 /srv/dbha/backups
sudo docker exec dbha-etcd /usr/local/bin/etcdctl \
  --endpoints=http://127.0.0.1:2379 \
  snapshot save "/tmp/dbha-${stamp}.db"
sudo docker cp "dbha-etcd:/tmp/dbha-${stamp}.db" "/srv/dbha/backups/dbha-${stamp}.db"
sudo cp -a /srv/dbha/server/control-watermark.json \
  "/srv/dbha/backups/control-watermark-${stamp}-controller1.json"
sudo sha256sum "/srv/dbha/backups/dbha-${stamp}.db" \
  > "/srv/dbha/backups/dbha-${stamp}.sha256"
```

将 snapshot、sha256 和三个独立水位复制到独立备份存储。恢复前先备份当前状态，并停止两台 Proxy，避免恢复管理状态期间继续提供未核验路由；宿主机 probe 保持运行。

三节点恢复顺序：

1. 停止两台 `dbha-proxy` 和三台 `dbha-server`，保持业务节点 probe 与旧 etcd 暂时可用。
2. 在至少一台 controller 执行 `dbha-server -c /etc/dbha/server.json prepare-restore`，写入随机代次的 `restore-required`。
3. 停止三台 `dbha-etcd`。
4. 在每台机器用同一个 snapshot、相同 initial cluster 和新的共同 cluster token 恢复本机成员。
5. 启动三个 etcd 并确认 3/3 健康。
6. 先启动带 marker 的 server，确认所有集群为 `RESTORE_UNVERIFIED` 且切换关闭，再启动两个 standby。
7. 使用 `/api/v1/recovery/reconcile` 核验真实 MySQL 和已停止的 Proxy；成功后启动两个 Proxy，重新核验路由，仍需显式重新启用切换。

停止控制服务并写 marker：

```bash
sudo systemctl stop dbha-proxy        # 两台 Proxy 执行
sudo systemctl stop dbha-server       # 三台都执行
sudo -u dbha /opt/dbha/bin/dbha-server -c /etc/dbha/server.json prepare-restore
sudo systemctl stop dbha-etcd         # 三台都执行
```

以下命令在每台 controller 分别执行，替换 `member_name`、`member_ip`、snapshot 名和 token。三台必须使用完全相同的 `initial_cluster` 与 `restore_token`：

```bash
snapshot=dbha-20260925T000000Z.db
member_name=controller1
member_ip=10.80.10.10
initial_cluster='controller1=http://10.80.10.10:2380,controller2=http://10.80.10.11:2380,controller3=http://10.80.10.12:2380'
restore_token='dbha-restore-20260925T000000Z'

cd /srv/dbha/backups
sudo sha256sum -c "${snapshot%.db}.sha256"
sudo mv /srv/dbha/etcd "/srv/dbha/etcd.before-${restore_token}"

sudo docker run --rm --entrypoint /usr/local/bin/etcdutl \
  -v /srv/dbha/backups:/backups:ro \
  -v /srv/dbha:/srv-dbha \
  gcr.io/etcd-development/etcd:v3.6.0 \
  snapshot restore "/backups/$snapshot" \
  --data-dir=/srv-dbha/etcd \
  --name="$member_name" \
  --initial-cluster="$initial_cluster" \
  --initial-cluster-token="$restore_token" \
  --initial-advertise-peer-urls="http://$member_ip:2380"
```

三个成员都完成后启动 etcd 和 server。恢复出的 etcd cluster ID 会变化；旧 leader、旧 lease 和旧水位不会被直接信任。如果 snapshot 中仍有旧 owner lease，带 marker 的节点可能等待最多一个约 30 秒的 lease 窗口，禁止手工删除 owner key。它取得恢复 owner 后会强制设置 RESTORE_UNVERIFIED，standby 只有观察到当前 cluster ID 的 live owner heartbeat 后才接受新水位。

构造恢复请求时必须列出全部处于门禁的集群：

```json
{
  "gate": "RESTORE_UNVERIFIED",
  "clusters": [
    {"cluster_id": 1, "expected_epoch": 3}
  ]
}
```

```bash
servers=http://10.80.10.10:8080,http://10.80.10.11:8080,http://10.80.10.12:8080
sudo -u dbha /opt/dbha/bin/dbha-server -c /etc/dbha/server.json ctl \
  --servers "$servers" --data-file /tmp/reconcile.json \
  POST /api/v1/recovery/reconcile

# reconcile 成功后在两台 Proxy 执行
sudo systemctl start dbha-proxy
```

恢复 marker 带随机代次；水位保存已消费代次。即使进程在写水位后、删除 marker 前崩溃，同一 marker 也不会在以后重新打开恢复门禁。不要复制、删除或手工降低水位文件来强制启动。

## 12. 常用排障

```bash
sudo systemctl status dbha-etcd dbha-server dbha-probe dbha-mysql dbha-proxy
sudo journalctl -u dbha-server -n 200 --no-pager
sudo journalctl -u dbha-probe -n 200 --no-pager
sudo docker logs --tail 200 dbha-etcd
sudo docker logs --tail 200 dbha-mysql
sudo docker logs --tail 200 dbha-proxy
```

- 三台 `/healthz=200` 但全部 `/readyz=503`：检查 etcd quorum、owner key、状态水位和 `restore-required`，不要删除 `/srv/dbha/server`。
- probe 不能上报：检查 50052/TCP、`server_grpc_endpoints`、agent token 权限和本机 `/srv/dbha/probe/identity.json`。
- Proxy 不启动：检查 probe 是否先上报 STARTING、恢复门禁、启动许可、`/run/dbha` 共享挂载和 supervisor 日志。
- 集群不进入 READY：检查 MySQL GTID、复制通道、read_only/super_read_only、固定 SSH host key 和两个 Proxy 实际后端。
- 重跑生成器提示 inventory 不匹配：不要编辑旧 identity；先确认是扩容、迁移还是新环境，再使用对应流程。
- 已切换的旧主必须保持隔离，人工重建复制并通过维护 API 核验后才能重新加入。当前实现不提供严格 STONITH 或自动 rejoin。

systemd 单元的职责和依赖关系见 [systemd/README.md](systemd/README.md)。功能边界与验收记录见 [自动元数据 PRD](../../../docs/design/automatic-metadata-prd.md) 和 [验收状态](../../../docs/design/acceptance-status.md)。
