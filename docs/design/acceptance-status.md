# 自动元数据与统一控制端：开发验收记录

日期：2026-09-17～2026-09-25。分支：`feat/automatic-metadata-control-plane`。范围依据 `automatic-metadata-prd.md`。

本记录区分代码/组件测试与真实业务端到端验收。组件测试通过不能替代 Linux 目标环境的发布门禁。

## 已实现

- `cmd/server` 合并认证接收、管理 API、自动拓扑确认、故障复核、切换执行与元数据读取；默认部署仅保留一个控制进程和 etcd。
- 新 probe 入口自动发现 MySQL UUID、全部复制通道、GTID/只读状态及 Proxy 实际后端；安装器生成组和身份，不填写节点主备角色。
- 管理状态改为 etcd 分键事务持久化。会话 generation/epoch、顺序校验、重复 ACK、冲突隔离、所有权与操作门禁均进入一致性检查。
- Proxy 启动取得可核验许可；请求意图持久化，丢响应/重启继续处理原请求。未核验释放的许可阻止切换。
- 切换持久化步骤意图、执行开始与验证结果，角色在单事务内提交；旧主保留为 RETURNED_UNVERIFIED。默认关闭自动切换。
- 迁移、快照恢复与普通重启分别使用恢复门禁及独立水位；明确恢复可在确认 Proxy 已停止后解除门禁，再申请新路由。
- 提供管理 CLI、受控离线导入、单/三集群 Compose、EC2 安装与 systemd 文件、备份和恢复说明。
- dbha-server 可运行多个常驻副本，通过 etcd owner lease 保持单一 active leader；owner 绑定 etcd cluster ID，heartbeat 为 standby 提供快照恢复后的动态存活证明，leader 丢失或连接到不同 etcd 集群时取消在途请求。follower 对 HTTP/gRPC 返回明确的 `NOT_LEADER`。probe、Proxy supervisor、一次性安装器和管理 CLI 支持静态控制端地址池，不依赖 VIP。
- EC2 生成器接受三个 controller 地址，生成唯一 node ID/广播地址、三节点 etcd initial cluster 和全部客户端地址池；单 controller 配置仍兼容。

## 已有验证证据

| 验证 | 结果与范围 |
|---|---|
| Go 全模块测试 | 在 `dbha-v2` 模块执行 `DBHA_TEST_ETCD=http://127.0.0.1:12379 go test -race ./... -count=1`，全部通过 |
| Go 静态检查 | `go vet ./...` 通过 |
| 无 VIP 控制端接管 | 最新二进制的两个真实 dbha-server 进程连接 Docker etcd：任一时刻仅一个 `/readyz=200`，follower 返回 `NOT_LEADER`，standby 只在观察到 owner heartbeat 前进后写水位，CLI follower-first 地址池成功；对 leader 执行 `SIGKILL` 后 standby 在 29.08 秒接管，接管后管理 API 正常。当前窗口来自 30 秒 owner lease TTL |
| 真实 etcd + race | 覆盖身份、并发重复上报、会话接管、失去 owner 后拒写、操作阶段、维护恢复、恢复门禁、许可 CAS，以及旧 owner 释放后的有界启动等待 |
| 控制传输 | 独立 Docker 明文实例的 ready、deployment 创建和管理 CLI 通过；HTTP 注册/明文 gRPC 上报客户端测试通过；单/三集群现有控制端原地升级后均健康，集群列表分别为 1/3；可选 HTTPS/TLS gRPC 兼容测试保留 |
| probe 行为 | 本地持久 boot、会话过期重新注册、采样故障分类、复制查询失败与零通道区分、多通道采集、同会话禁 binlog 心跳测试通过 |
| 部署配置 | Python 配置/安装器测试通过；Proxy 许可丢响应、重启和终止失败已有回归测试 |
| 迁移计划 | 保留角色和 ID、拒绝冲突、拒绝不能等价执行的启用规则、强制持久水位目录均有测试 |
| 真实 MySQL/Proxy | 单集群 `READY/HEALTHY`，两 Proxy 指向 server_id=21，应用写读通过；故障切换到 server_id=22 后两路由和写入通过，旧主保持停止 |
| 控制端/Proxy 重启 | 切换后重启 `dbha-server` 与两个 Proxy，路由仍指向 server_id=22；STARTUP_VALIDATION 自动清除，拓扑 epoch=3，切换开关为 false |
| 长暂停恢复 | 修复休眠后旧采样触发 `STALE_SAMPLE` 导致 probe 退出的问题；精确识别该 gRPC 状态并重新采样，race 回归和 Proxy 重建实测通过 |
| 三集群 | 全新数据卷运行 14 个常驻容器，仅含 server、etcd 和 12 个业务节点；3 个 READY 集群与 6 个路由通过，cluster1 切换时 cluster2/3 主库不变 |
| etcd 容量配置 | 单/三集群均启用 1h 周期自动压缩和 2 GiB backend quota；手动 compact/defrag 将实验 DB 从 728,727,552 降至 86,638,592 bytes；87 MB 快照、水位与 sha256 校验通过；`etcdutl` 恢复到两个临时 volume、复制和状态检查通过，运行中数据卷历史回滚及 recovery/reconcile 尚未执行 |
| 负载烟测 | 30 实例、目标 100 次/s、10 秒：999 attempts/999 success，0 error/timeout/drop，p50 6.92 ms、p95 9.16 ms、p99 12.74 ms、峰值 RSS 46,448,640 bytes |

## PRD 验收对照

“组件覆盖”表示已验证关键规则，但尚未执行该条的全部故障注入或真实业务场景。

| AT | 当前证据 / 剩余验收 |
|---|---|
| 01–03 | 单集群和三集群全新部署均未写 seed/角色元数据；自动注册、初始路由与 14 容器精简形态实测通过 |
| 04–06 | 真实 3 组主从拓扑和跨组隔离通过；多通道拒绝有组件测试，真实多通道故障注入待验收 |
| 07–10 | 真实 etcd + HTTP/gRPC 组件覆盖顺序、重复、会话、身份与鉴权；完整跨组 HTTP 负例矩阵待验收 |
| 11–12 | 新鲜度、三轮时间窗、会话恢复组件覆盖；本机单/三集群冷启动通过，目标 Linux 冷启动耗时待测 |
| 13–15 | 单集群真实提升、双 Proxy 改路由、切换后写入和旧主保持停止通过；无 fencing 的既定边界不变 |
| 16–17 | 外部动作不明不得重放、事务角色提交组件覆盖；各步骤前后 kill 及提交响应丢失完整矩阵待验收 |
| 18 | 真实 owner 失效拒写和旧租约释放后的启动等待通过；2 GiB 配额已配置，真实配额耗尽及网络故障矩阵待验收 |
| 19 | 使用线性一致全量读取，不维护 watch 缓存；恢复水位/门禁和快照备份通过，真实历史快照恢复待验收 |
| 20 | 大小/并发内存上限已实现；本机 Store 管线 10 秒 100 次/s 无丢失，目标 Linux 30 分钟端到端 gRPC p95/RSS 压测未执行 |
| 21 | 非终态保留和终态清理组件覆盖；长期日志轮转及磁盘使用验收待执行 |
| 22 | 维护和 tombstone 校验已实现；完整退役后重报端到端待验收 |
| 23 | 离线迁移预检和计划测试通过；使用真实旧管理库的完整停机迁移待验收 |
| 24 | 模块 race/vet、配置与脚本测试、Compose 解析通过；本机单/三集群真实部署结果如上 |
| 25–27 | 许可、会话并发及恢复组件覆盖；切换后 server/Proxy 重启实测通过，暂停进程跨 epoch 与离线 Proxy 网络恢复完整矩阵待验收 |
| 28–30 | 安装器稳定身份、策略版本冲突、导入计数器测试覆盖；真实旧库迁移后的安装重跑待验收 |
| 31 | 非终态/终态清理及未知操作拒绝路径已实现；清理后旧 ID 完整重放场景待验收 |
| 32 | 独立水位、带随机代次且可识别已消费状态的外部标记、标记节点取得恢复 owner 后强制 RESTORE_UNVERIFIED、standby 从恢复门禁 leader 安全接收新水位、写水位后遗留同代标记不重复开门禁，以及停止 Proxy 的核验路径已测试；真实切换前快照恢复待验收 |
| 33 | 多进程单活、follower HTTP/gRPC、地址池、强制终止 leader 后接管、owner key 删除与 etcd cluster ID 改变撤销 active、在途请求取消、standby heartbeat 存活证明、带正 TTL 但无新 heartbeat 的恢复 lease 及不同 cluster ID owner 拒绝、无水位禁选均有本机真实 etcd/race 证据；三 controller EC2 配置生成通过，真实三节点 etcd/EC2 故障演练待验收 |

## 发布边界

本次功能基线限 MySQL 8.0.22+、GTID 单复制通道、一主一备和两个蓝鲸定制 Proxy。首版策略只接受当前执行器支持的数据库失败/SSH 二次检查失败触发；不能等价迁移的已启用规则会阻止导入。

macOS Docker 的结果只用于功能验证。七台真实 EC2、Linux 4 vCPU/8 GiB 的 30 分钟性能目标、全量故障注入及旧库实际迁移未验收前，不应宣称 PRD 32 条已全部完成或达到生产发布条件。

首版仍没有严格 fencing 和自动旧主 rejoin。多控制端 HA 已实现为单活 lease + 客户端地址池；etcd 事务只能约束管理状态，不能撤销已发出的 MySQL/Proxy 外部命令。真实三节点 etcd/EC2 演练完成前，该能力仍属于功能完成、目标环境未验收。
