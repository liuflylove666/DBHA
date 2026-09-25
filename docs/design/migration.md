# DBHA 旧管理 MySQL 到 etcd 的离线迁移

本文是 `automatic-metadata-prd.md` 第 13 节的执行说明。工具位于 `dbm-services/common/dbha-v2/tools/cmd/import-management`，默认仅预检；只有显式 `--apply` 才写 etcd。迁移必须在旧控制进程停写、自动切换关闭且没有进行中切换后执行。旧 MySQL 数据卷和备份在验收完成前保留。

## 输入与身份衔接

工具读取旧管理 MySQL 的 `standalone_instances`（`--source standalone`）或 `t_dbm_metadata`（`--source hamodel`），以及同一 schema 中存在的策略、黑白名单和跳过项。它对旧库开启只读事务，不更新旧表。MySQL `server_uuid` 不在旧表中；工具使用单独的业务 MySQL 只读账号查询两台实际业务库的 UUID、GTID、只读状态及所有复制通道。任一节点不可访问、角色与旧库不一致、复制通道数不符合要求或身份缺失，预检失败，不生成猜测的 ID。

安装器先为每组 4 个节点生成稳定的 `agent_id`、独立的 256 位十六进制 token 文件，以及两个 Proxy 的持久 `proxy_uuid`。将下列 JSON 数组作为 `--agent-map`；每条 `kind + host + port` 必须与旧管理表的一个活动行精确匹配。`token_file` 必须仅对文件所有者可读。工具只将 token 的 SHA-256 哈希写入 etcd，不输出 token。安装器随后必须把相同的 `agent_id`、`token_file`、`proxy_uuid` 写进对应节点的 `discover` 配置；不得为已经导入的节点重新生成身份。MySQL 的 `instance_id` 来自真实 `server_uuid`，Proxy 的 `instance_id` 来自映射中的 `proxy_uuid`。

```json
[
  {"kind":"mysql","host":"10.203.80.21","port":3306,"agent_id":"aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaa1","token_file":"/secure/tokens/mysql-primary.token"},
  {"kind":"mysql","host":"10.203.80.22","port":3306,"agent_id":"aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaa2","token_file":"/secure/tokens/mysql-standby.token"},
  {"kind":"proxy","host":"10.203.80.23","port":10000,"agent_id":"aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaa3","proxy_uuid":"bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbb3","token_file":"/secure/tokens/proxy-1.token"},
  {"kind":"proxy","host":"10.203.80.24","port":10000,"agent_id":"aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaa4","proxy_uuid":"bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbb4","token_file":"/secure/tokens/proxy-2.token"}
]
```

每组必须恰好两台 MySQL 和两个 Proxy。若原系统的活跃规则位于 DBHA v1 远程白名单，工具无法从旧管理库读取；执行 `--apply` 前必须停止并确认它已关闭。旧表中的任一活跃切换策略或黑白名单行也会使预检失败，因为首版新策略执行器尚不能等价运行它们。已禁用策略可作为只读历史策略导入，但不会自动启用。旧 `t_skip_dbinstance` 的活动行按真实 MySQL UUID 映射为新排除项；无法映射即失败。

## 命令

在 Go 模块目录构建工具：

```sh
go build -o ./bin/dbha-import-management ./tools/cmd/import-management
```

旧库 DSN 放入 `LEGACY_MYSQL_DSN` 环境变量，业务只读密码放入 0600 文件。默认源为研究环境的 `dbha_metadata.standalone_instances`；使用正式缓存表时改为 `--source hamodel --legacy-schema dbha_data`。以下先预检，`--logs-out` 显式导出旧切换日志为新建的 JSONL 文件，不写入新鲜样本：

```sh
./bin/dbha-import-management \
  --agent-map /secure/agent-map.json \
  --business-user dbha_reader \
  --business-password-file /secure/business-reader.password \
  --etcd-endpoints 127.0.0.1:2379 \
  --environment lab \
  --logs-out /secure/old-switch-logs.jsonl
```

预检成功后，先备份旧管理库、节点配置和目标 etcd 快照，检查原自动切换已关闭、旧控制写进程已停止，再执行相同的源、凭据和 etcd 参数（去掉已完成的 `--logs-out`，避免覆盖审计文件），并加：

```sh
  --apply --state-dir /var/lib/dbha-server \
  --confirm-old-control-stopped --confirm-v1-whitelist-disabled
```

`--state-dir` 必须是后续 `dbha-server` 使用的**同一个持久状态卷**，工具在写 etcd 前检查该目录可写。导入事务成功后，工具将当前 etcd 集群 ID、revision 和拓扑 epoch 写成控制端水位文件，并保留 `MIGRATION_UNVERIFIED` 门禁。若水位写入失败，工具会明确报 **“IMPORT COMMITTED but control watermark was not persisted”**；此时数据已经导入，不得启动控制端，也不能把该错误当成可安全重试的未提交事务。先修复状态卷，再由维护流程在持有迁移 owner 的情况下完成水位，或从导入前备份整体恢复。

远程 etcd 使用 `--etcd-ca`、`--etcd-cert`、`--etcd-key` 一起提供 TLS 身份。工具发现目标环境已有控制端 owner 时直接拒绝；必须停止控制端，而不是绕过所有权锁。导入对目标命名空间执行一次基于 owner/version 的 etcd 事务；其中任一部署、集群、凭据、agent、实例、端点索引、策略或排除项键冲突都会拒绝整个导入。计数器只提升到现有值与导入的最大 `cluster_id` 中较大的一个，绝不降低。单次 etcd 事务有 120 个键的实现上限；超出时工具拒绝，不分批写入半套集群。

## 启动与验收

导入的集群保留旧 `cluster_id`，但 `SwitchingEnabled=false`、`RecoveryGate=MIGRATION_UNVERIFIED`、健康状态为 `UNKNOWN`，实例标为 `RETURNED_UNVERIFIED`。工具不把旧采样当作当前健康证据，也不通过旧角色直接开放路由或自动切换。启动新 `dbha-server` 与节点 `dbha-probe discover` 后，使用管理端的 `recovery/reconcile` 执行独立只读校验：核对新 probe 的 UUID、复制源/通道、两侧只读状态、Proxy 实际后端与导入的角色。全部符合后才可清除迁移门禁；切换开关仍保持关闭，需要另行显式启用。

若新路径已完成外部主从切换，不得直接重新启动旧控制端回退。必须先停止新控制端并核对实际主库及两侧路由，再将最新角色同步回旧环境。历史 JSONL 只作审计保存，不作为新 etcd 上报或故障判定输入。
