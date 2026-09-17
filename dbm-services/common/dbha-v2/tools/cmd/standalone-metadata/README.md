# Standalone metadata API

This command replaces the three DBM APIs needed by the DBHA v2 TenDBHA research flow. It stores
topology and role changes in MySQL and deliberately rejects name-service, CLB, Polaris, dumper and
Tendis operations.

Initialize the management MySQL once with `schema.sql`, then `seed.sql`. The seed uses
`INSERT IGNORE`, so container restarts never reset a role changed by failover.

Environment:

* `MYSQL_DSN` (required), for example `root:password@tcp(metadata-store:3306)/dbha_metadata?parseTime=true`
* `API_TOKEN` (required); DBHA may send it as `db_cloud_token` or `Authorization: Bearer ...`
* `LISTEN_ADDR` (optional, default `:8080`)

DBHA analysis endpoints:

* `dbmApiMetadata.api`: `http://metadata-api:8080/api/v1/metadata`
* `dbmApiUpdateStatus.api`: `http://metadata-api:8080/api/v1/update-status`
* `dbmApiSwapMysqlRole.api`: `http://metadata-api:8080/api/v1/swap-mysql-role`

Build from the `dbha-v2` module root:

```sh
docker build -f tools/cmd/standalone-metadata/Dockerfile -t dbha-standalone-metadata .
```
