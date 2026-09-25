# Docker 研究环境

默认实验常驻服务：`dbha-server`、etcd、两台业务 MySQL、两台蓝鲸 Proxy，共 **6 个容器**。每个业务节点的 probe 与本地进程同容器运行。三集群实验共享一组 server/etcd，加上 12 个业务节点，共 **14 个常驻容器**；一次性的复制 bootstrap 作业不计入常驻数量。没有管理 MySQL、metadata API、admin、receiver 或 analysis。

从仓库根目录执行：

```bash
bash deploy/dbha-v2/lab.sh up
bash deploy/dbha-v2/lab.sh check
bash deploy/dbha-v2/lab.sh enable-switching
bash deploy/dbha-v2/lab.sh failover-test
bash deploy/dbha-v2/lab.sh disable-switching
```

`up` 首次生成稳定 deployment/agent/Proxy UUID、管理员 token 和四个 SSH 主机密钥。它启动 etcd/server，通过内网 HTTP 安装器创建部署组与 agent，启动业务 MySQL 并配置 GTID 单通道复制，再让 Probe 自动上报并由 Proxy 取得启动许可。配置、凭据与状态保存在 Git 忽略的 `generated/` 和 Docker volumes。重跑不会重置身份；如已发生切换，`up` 根据初始主库记录拒绝重新启动旧主。自动切换默认关闭。

切换后重建单个 Proxy 时使用 `docker compose --env-file generated/.env up -d --no-deps --force-recreate proxy1`（或 `proxy2`）。Proxy 服务不依赖一次性 bootstrap 作业，避免 Compose 沿依赖链启动已经隔离的旧主；无论何种运维命令，故障旧主都必须保持停止，直至人工隔离并重建复制。

三集群启动与验证：

```bash
bash deploy/dbha-v2/multi.sh up
bash deploy/dbha-v2/multi.sh check
```

`multi.sh` 构建与单集群相同的 runtime/MySQL/Proxy 镜像；依次启动 etcd/server，运行 HTTP 安装器，启动六台 MySQL、三个 bootstrap 作业、六台 Proxy。安装器只创建身份，不预写任何主备角色或 Proxy 后端。`multi.sh enable-switching` 和 `failover-test` 只适用于隔离实验，默认关闭切换。

已有管理 MySQL 的迁移需要在旧控制停写后运行 `migration_agent_map.py`，生成离线导入工具读取的 agent-map 和 0600 token，再执行 [迁移说明](../../docs/design/migration.md)。存在该映射时 `install.py` 复用导入身份，不重复签发。迁移后先核验、解除 MIGRATION_UNVERIFIED 门禁，再启用切换。

管理状态备份在单集群 lab 运行 `bash deploy/dbha-v2/backup.sh`：脚本调用 etcd snapshot，校验快照，将快照和独立 `control-watermark.json` 以 0600 存在被 Git 忽略的 `backups/`，保留最新 **7 份**。可由外部调度器每日运行一次；本仓库不会自动安装计划任务。Compose 已配置 1 小时周期自动压缩和 2 GiB backend quota。维护窗口可在备份后按当前 revision 手动压缩并整理碎片：

```bash
cd deploy/dbha-v2
etcd_id=$(docker compose --env-file generated/.env ps -q etcd)
revision=$(docker exec "$etcd_id" /usr/local/bin/etcdctl endpoint status -w json | python3 -c 'import json,sys; print(json.load(sys.stdin)[0]["Status"]["header"]["revision"])')
docker exec "$etcd_id" /usr/local/bin/etcdctl compact "$revision"
docker exec "$etcd_id" /usr/local/bin/etcdctl defrag
```

快照恢复必须先另做一次当前状态备份，校验所选快照的 sha256，再停 dbha-server；在 etcd 仍运行时执行离线命令写入持久门禁标记：

```bash
cd deploy/dbha-v2
docker compose --env-file generated/.env stop dbha-server
docker compose --env-file generated/.env run --rm --no-deps dbha-server \
  dbha-server -c /etc/dbha/server.json prepare-restore
```

随后停止 etcd，把快照恢复到 Compose 数据卷。下面的 `snapshot_name` 只接受 `backups/` 中已通过相邻 `.sha256` 文件校验的文件名：

```bash
snapshot_name=20260920T000000Z.db
shasum -a 256 -c "backups/${snapshot_name%.db}.sha256"
docker compose --env-file generated/.env stop etcd
volume=$(docker volume ls -q --filter name=dbha-v2-lab_etcd-data | head -n1)
test -n "$volume"
restore_volume="${volume}-restore"
docker volume create "$restore_volume"
docker run --rm --entrypoint /usr/local/bin/etcdutl \
  -v "$restore_volume:/restore" -v "$PWD/backups:/backups:ro" \
  gcr.io/etcd-development/etcd:v3.6.0 \
  snapshot restore "/backups/$snapshot_name" --data-dir=/restore/data \
  --name=lab --initial-cluster=lab=http://10.203.80.11:2380 \
  --initial-advertise-peer-urls=http://10.203.80.11:2380
docker run --rm -v "$volume:/target" -v "$restore_volume:/restore:ro" \
  debian:bookworm-slim sh -ec '
    find /target -mindepth 1 -maxdepth 1 -exec rm -rf -- {} +
    cp -a /restore/data/. /target/
  '
docker volume rm "$restore_volume"
docker compose --env-file generated/.env up -d --wait --wait-timeout 180 etcd dbha-server
```

重启后 `recovery_gate` 必须是 `RESTORE_UNVERIFIED` 且切换关闭。从 `GET /api/v1/clusters` 取得所有受门禁集群的 `cluster_id` 和 `topology_epoch`，组成 `{"gate":"RESTORE_UNVERIFIED","clusters":[...]}`，再通过 `dbha-server ctl ... POST /api/v1/recovery/reconcile --data-file ...` 核验真实主备与全部 Proxy 路由。只有成功响应会更新独立水位、删除恢复标记并解除门禁；仍需另行显式启用切换。不能只替换数据卷并跳过标记和核验。快照与水位不是一个原子写入，恢复流程始终要求重新核验实际数据库。

实验只支持 MySQL 8.0 GTID 单通道、一主一备和两个蓝鲸定制 Proxy。当前 Compose 保持单 dbha-server/单 etcd，适合功能研究，不用于验证控制端 HA；三 controller、三节点 etcd、Docker 数据服务和无 VIP 地址池的最新步骤见 [EC2 Docker 部署说明](ec2/README.md)。当前尚未在真实 EC2 做故障演练。故障后旧主保持停止，必须隔离并重建复制才可重新加入。
