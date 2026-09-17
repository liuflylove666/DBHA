# 来源与版权说明

本项目包含源自 [TencentBlueKing/blueking-dbm](https://github.com/TencentBlueKing/blueking-dbm) 的 DBHA v2 代码。提取所用的上游基线提交为 `fcbb761911dde0fe418e170044f26a2b4436600c`。原代码依 [MIT License](LICENSE) 发布，版权声明为 `Copyright (c) 2023 腾讯蓝鲸`；本项目保留该许可证和原有文件中的版权信息。

本独立项目在上述基线及来源工作区修改的基础上，包含 MySQL standalone metadata API、独立部署与演练配置，以及 probe health 子命令读取配置路径的修复。`dbm-services/common/go-pubpkg` 仅保留 DBHA v2 所需的相关包。Go module 路径和 import 名称保持与上游兼容，以便独立源码构建。

独立仓库另增加根目录 Go workspace、构建/检查入口和部署文档；修正 GORM 慢查询日志的参数错位，并将继承的配置、网卡和日志测试改为可在干净检出中运行，外部数据库测试改为显式配置后执行。

Docker 实验依赖的蓝鲸定制 MySQL Proxy 二进制从[上游发布包](https://github.com/TencentBlueKing/blueking-dbm/releases/download/v1.0.0/mysql-proxy-0.82.15.tar.gz)下载；它不是本仓库内可编译的 Proxy 源码。相关版本及 SHA256 见 [部署说明](deploy/dbha-v2/README.md#proxy-来源)。
