USE dbha_metadata;

INSERT IGNORE INTO standalone_instances
  (host_id,bk_cloud_id,bk_biz_id,cluster_id,cluster,cluster_type,machine_type,access_layer,instance_role,status,ip,port,admin_port)
VALUES
  (21,0,1,1,'research-mysql.local','tendbha','backend','storage','backend_master','running','10.203.80.21',3306,0),
  (22,0,1,1,'research-mysql.local','tendbha','backend','storage','backend_slave','running','10.203.80.22',3306,0),
  (31,0,1,1,'research-mysql.local','tendbha','proxy','proxy','','running','10.203.80.31',10000,11000),
  (32,0,1,1,'research-mysql.local','tendbha','proxy','proxy','','running','10.203.80.32',10000,11000);
