# EC2 systemd 单元

完整的源码构建、用户创建、权限和启动顺序见上一级 `README.md`，以该手册为准。

- `dbha@.service`：原生 admin、receiver、analysis、probe。每台机器只安装/启动对应实例。
- `standalone-metadata.service`：原生 API；systemd 读取 root:root 0600 的 `/etc/dbha/metadata.env`。
- `dbha-mysql.service`、`dbha-etcd.service`、`dbha-proxy.service`：systemd 监督 Docker host-network 容器。

原生程序使用 `dbha` 用户，PID 目录 `/run/dbha-<instance>`，日志目录 `/var/log/dbha`。数据节点的 `dbha` 用户必须可以通过 SSH 执行 health 命令，不能设置为 nologin。

native units 依赖网络初始化，不把远端服务的启动成功当作 systemd 本地依赖；必须按主手册逐项验证 MySQL、etcd、metadata 和 receiver。业务 MySQL 的开机启动不默认启用，以便实验旧主恢复前人工核查。
