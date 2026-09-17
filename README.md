# DBHA

本项目把蓝鲸 DBM 仓库中的 DBHA v2 提取为可独立构建、部署和演练的 MySQL 主从高可用研究项目。它通过独立 metadata API 提供 DBHA 所需的拓扑、状态更新和主备角色交换接口，运行时无需 DBM Django、CMDB、GSE 或 Kafka。

保留原有 `dbm-services/common/dbha-v2`、所需的 `dbm-services/common/go-pubpkg` 及 `deploy/dbha-v2` 目录层级和 Go import 名称。源码包含 `admin`、`receiver`、`analysis`、`probe` 和 `standalone-metadata`；实验环境另使用管理 MySQL、etcd、业务 MySQL 主备和蓝鲸发布的 Proxy 二进制。Proxy 不是由本项目源码构建，其来源与校验值见 [Docker 实验说明](deploy/dbha-v2/README.md#proxy-来源)。

## 源码构建与检查

构建需要 Go 1.26 和 Make；测试还需要 Python 3 及支持 CGO 的 C 编译器（如 GCC 或 Clang）。仓库已包含构建所需的 protobuf 生成代码，普通构建与测试不需要安装 `protoc`。在仓库根目录执行：

```bash
make build
make test
make check
```

`make build` 将五个 Go 程序输出到 `bin/`；`make test` 运行 Go 和配置生成测试；`make check` 运行 `go vet` 及部署 Shell 语法检查。具体目标以根目录 [Makefile](Makefile) 为准。构建会下载 Go 模块，需要访问相应模块源。交叉编译 Linux amd64 可执行 `GOOS=linux GOARCH=amd64 make build`。

外部 MySQL/Proxy 集成测试在未配置对应环境变量时明确跳过，不计作故障切换验收。切换日志测试使用 `DB_TEST_IP`、`DB_TEST_PORT`、`DB_TEST_USER`、`DB_TEST_PASSWD`；数据库连接和超时测试使用 `DBHA_MYSQL_*`，Proxy 测试使用 `PROXY_*`。独立元数据事务测试使用 `MYSQL_TEST_DSN`。仅为专用测试实例设置这些变量，完整主备切换请使用下方 Docker 演练。

2026-09-17 独立提取验证：五个程序在 macOS arm64 原生构建及 Linux amd64 交叉构建通过，41 个 Go 包测试、5 个 Python 测试、`make check` 和单/三集群 Compose 配置解析通过。本轮未启动 Docker 服务或执行真实主从故障演练。

## Docker 故障演练

需要 Docker Compose、Python 3，以及首次构建所需的网络。从仓库根目录执行：

```bash
bash deploy/dbha-v2/lab.sh up
bash deploy/dbha-v2/lab.sh check
bash deploy/dbha-v2/lab.sh enable-switching
bash deploy/dbha-v2/lab.sh failover-test
bash deploy/dbha-v2/lab.sh disable-switching
```

`up` 默认关闭自动切换。`failover-test` 会停止实验的旧主容器，检验提升、双 Proxy 路由和故障后写入。演练后旧主保持停止；恢复前需先核实数据和复制关系。配置与凭据在被 Git 忽略的 `.env`、`generated/` 目录中，切勿提交。详细步骤、三组集群实验和运行边界见 [Docker 实验说明](deploy/dbha-v2/README.md)。

五台 EC2 的源码编译及 systemd 部署步骤见 [EC2 安装手册](deploy/dbha-v2/ec2/README.md)。该手册是研究环境操作指南，尚未在五台真实 EC2 上完成部署验证。

## 已知边界

单机 Docker 实验不能抵抗宿主机故障。现有部署未实现旧主隔离、自动重新加入复制、管理面冗余或统一 VIP/DNS 入口；不保证网络分区时不会双主，也不承诺零数据丢失。自动切换只应在核实探测、SSH 二次确认与业务路由后用于受控研究环境。详细限制见 [安全与功能边界](deploy/dbha-v2/README.md#安全与功能边界)。

## 来源与许可

源码源于 [腾讯蓝鲸 blueking-dbm](https://github.com/TencentBlueKing/blueking-dbm) 的 MIT 许可代码，基线提交为 `fcbb761911dde0fe418e170044f26a2b4436600c`，并包含来源工作区新增的独立 metadata 适配器及 probe health 配置修复。保留原项目版权和 [MIT 许可证](LICENSE)；来源与修改说明见 [NOTICE.md](NOTICE.md)。文档中 2026-09-09 的 Docker 验证是来源工作区的历史记录，不代表本仓库的新检出或 EC2 环境已运行验证。
