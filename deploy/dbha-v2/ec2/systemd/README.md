# EC2 systemd 单元

最新安装和恢复命令见 [EC2 Docker 部署文档](../README.md)。当前单元的运行边界如下：

| 节点 | 启动顺序 | 说明 |
|---|---|---|
| controller | `dbha-etcd.service` → `dbha-server.service` | etcd 在 Docker 中运行；server 是宿主机静态二进制。server 只 `Wants` 本机 etcd，可在本机 etcd 失败时继续访问另外两个 endpoint |
| MySQL | `dbha-mysql.service` → `dbha-probe.service` | MySQL 在 Docker 中运行；probe 使用宿主机网络访问 127.0.0.1:3306 |
| Proxy | `dbha-probe.service` → `dbha-proxy.service` | probe 先上报 STARTING，Proxy supervisor 再从控制端地址池申请启动许可 |

持久目录：

- `/srv/dbha/etcd`：本机 etcd member 数据。
- `/srv/dbha/server`：本机 controller 水位与恢复 marker。
- `/srv/dbha/mysql`：本机 MySQL 数据。
- `/srv/dbha/probe`：不可复制到其他节点的 probe boot identity。
- `/run/dbha`：Proxy 与宿主机 probe 共享的路由重协调信号。

`dbha@.service` 与 `standalone-metadata.service` 仅供旧部署保留，新架构不得安装或启用。生产节点不使用实验 Compose 文件。
