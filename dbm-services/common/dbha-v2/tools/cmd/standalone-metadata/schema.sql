CREATE DATABASE IF NOT EXISTS dbha_metadata CHARACTER SET utf8mb4 COLLATE utf8mb4_unicode_ci;
USE dbha_metadata;

CREATE TABLE IF NOT EXISTS standalone_instances (
  host_id BIGINT NOT NULL,
  bk_cloud_id INT NOT NULL,
  bk_biz_id INT NOT NULL,
  logical_city_id INT NOT NULL DEFAULT 1,
  logical_city_name VARCHAR(64) NOT NULL DEFAULT 'research',
  cluster_id INT NOT NULL,
  cluster VARCHAR(255) NOT NULL,
  cluster_type VARCHAR(64) NOT NULL,
  machine_type VARCHAR(32) NOT NULL,
  access_layer VARCHAR(32) NOT NULL,
  instance_role VARCHAR(32) NOT NULL DEFAULT '',
  status VARCHAR(32) NOT NULL DEFAULT 'running',
  ip VARCHAR(64) NOT NULL,
  port INT NOT NULL,
  admin_port INT NOT NULL DEFAULT 0,
  created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
  updated_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
  PRIMARY KEY (bk_cloud_id, ip, port),
  KEY idx_cluster (bk_cloud_id, cluster_id),
  KEY idx_sync_shard (host_id),
  CONSTRAINT chk_status CHECK (status IN ('running','available','unavailable'))
) ENGINE=InnoDB;
