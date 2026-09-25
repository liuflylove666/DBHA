# etcd Ingest 负载烟测

`TestEtcdIngestLoad` 是显式启用的开发期烟测，默认跳过。它为每两个实例创建一个隔离的 deployment，使用随机 DBHA environment namespace，完成 agent 签发、注册和 topology 上报后，再通过真实 `Store.Ingest` 按固定目标速率写入 etcd。

默认参数是 30 个等价 proxy 实例、100 次/秒。每个实例串行递增 sample 序号，payload 在 1–4 KiB 之间；队列饱和时计为 dropped，不会从总尝试数中移除。输出包含 attempts、success、errors、timeouts、dropped、实际尝试/成功速率、p50/p95/p99、Go heap 起止值，以及操作系统可提供时的进程峰值 RSS。测试不设置未经确认的性能通过阈值；任何错误、超时或丢弃都保留在输出中供评估。

本机 10 秒烟测：

```bash
DBHA_TEST_ETCD=http://127.0.0.1:12379 \
DBHA_LOAD_SECONDS=10 \
go test -run '^TestEtcdIngestLoad$' -count=1 -v ./internal/server
```

Linux 4 CPU / 8 GB、持续 1800 秒：

```bash
GOMAXPROCS=4 \
DBHA_TEST_ETCD=http://127.0.0.1:2379 \
DBHA_LOAD_SECONDS=1800 \
DBHA_LOAD_RATE=100 \
DBHA_LOAD_INSTANCES=30 \
go test -run '^TestEtcdIngestLoad$' -count=1 -timeout 35m -v ./internal/server
```

`DBHA_LOAD_SECONDS` 必须是正整数，只有设置它才会执行测试。`DBHA_LOAD_RATE` 默认 100，`DBHA_LOAD_INSTANCES` 默认 30；实例数必须是大于等于 2 的偶数，以符合每个 deployment 两个实例的预算。

该测试覆盖 Store、CAS、序号校验和 etcd 持久化管线，不经过 gRPC、网络代理或序列化传输层，因此结果不能表述为完整 PRD 性能通过。完整验收仍需在目标 Linux 规格上运行 1800 秒，并另做默认明文 gRPC 端到端测试；启用 TLS 的部署再补测 TLS 模式。
