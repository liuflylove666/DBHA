# 自动元数据与统一控制端实施计划

依据 `automatic-metadata-prd.md`，在 `feat/automatic-metadata-control-plane` 分支实施。原有业务 SQL、复制初始化和探测 health 功能保留；新入口替代默认部署中的管理 MySQL 和四个控制服务。

## 顺序与责任边界

1. M1：锁定现有配置/元数据回归基线；增加统一服务、etcd 事务存储、身份/会话及管理 API。
2. M2：公共上报协议、probe 自动发现、拓扑交叉核验与 Proxy 启动许可。
3. M3：故障二次确认、持久化切换阶段、角色 CAS、恢复水位与维护接口。
4. M4：单/三集群与 EC2 配置、离线迁移、运维文档及验收。

新模块只依赖当前仓库已有 Go 库。角色仅由控制端核验/操作提交；无 fencing 边界和切换默认关闭保持不变。开发并行按协议/probe、控制端存储/API、拓扑/操作、部署分别拥有文件，集成和验收由主任务负责。

## 检查与发布门槛

- 修改前运行已有 Go/配置测试，记录既存失败；新协议、身份并发、事务和切换阶段补上行为测试。
- 分模块测试后运行全仓 Go test、vet、Python 配置测试、Shell 语法和二进制构建。
- 有真实 etcd 时执行事务/重启集成测试；真实 MySQL/Proxy 与 Linux 性能环境不可用时标记未验收，不能报告 PRD 全部通过。
- 验收按 PRD AT-01～32 记录证据。仅隔离测试环境允许故障注入，不操作外部生产实例。

## 当前进度

- M1–M4 的开发基线已完成：统一服务、etcd 分键事务、自动发现/会话、拓扑与切换、恢复门禁、迁移工具和部署脚本均已落地。
- 单集群 Docker 已验证健康冷启动、自动上报、两 Proxy 路由、应用写读、受控切换及 server/Proxy 重启后从 etcd 恢复。
- 三集群全新环境已验证 14 个常驻容器、3 个独立 READY 拓扑、6 个 Proxy 路由，以及只切换 cluster1 时 cluster2/3 不变。
- 真实 etcd 的 10 秒烟测达到 999/999 成功、0 错误/超时/丢弃，p95 9.16 ms；该结果来自 macOS Docker Store 管线，不替代 PRD 指定的 Linux 30 分钟端到端 gRPC 性能验收。
- 无 VIP 控制端 HA 已完成：dbha-server 多副本通过 etcd lease 单活竞选，owner 绑定 etcd cluster ID 且用 heartbeat 防止把快照恢复出的旧 lease 当成存活证明；受控恢复标记允许一个节点在水位回退时进入 RESTORE_UNVERIFIED，standby 只从该门禁 leader 接收新水位；probe/Proxy/安装器/CLI 使用静态地址池。最新本机双进程 `SIGKILL` leader 后约 29.1 秒接管。EC2 生成器已支持三 controller/三节点 etcd，真实 EC2 演练仍是发布门槛。
- 完整证据和未验收项记录在 `acceptance-status.md`。剩余发布门槛是目标 Linux/EC2、完整故障注入矩阵、历史快照实恢复和真实旧管理库迁移。
