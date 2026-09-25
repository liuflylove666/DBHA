package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"regexp"

	_ "github.com/go-sql-driver/mysql"
)

type legacySnapshot struct {
	Instances  []legacyInstance
	Policies   []legacyPolicy
	Exclusions []legacyExclusion
}

var schemaNamePattern = regexp.MustCompile(`^[A-Za-z0-9_]+$`)

func readLegacy(ctx context.Context, dsn, schema, source string) (legacySnapshot, error) {
	var snap legacySnapshot
	if !schemaNamePattern.MatchString(schema) {
		return snap, fmt.Errorf("invalid legacy schema")
	}
	if source != "standalone" && source != "hamodel" {
		return snap, fmt.Errorf("source must be standalone or hamodel")
	}
	db, err := sql.Open("mysql", dsn)
	if err != nil {
		return snap, err
	}
	defer db.Close()
	tx, err := db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true, Isolation: sql.LevelRepeatableRead})
	if err != nil {
		return snap, err
	}
	defer tx.Rollback()
	table := "standalone_instances"
	where := ""
	if source == "hamodel" {
		table = "t_dbm_metadata"
		where = " WHERE deleted_at IS NULL"
	}
	if ok, e := tableExists(ctx, tx, schema, table); e != nil {
		return snap, e
	} else if !ok {
		return snap, fmt.Errorf("required legacy table %s missing", table)
	}
	query := fmt.Sprintf("SELECT cluster_id,cluster,cluster_type,machine_type,access_layer,instance_role,status,ip,port,admin_port,bk_biz_id,bk_cloud_id FROM `%s`.`%s`%s", schema, table, where)
	rows, err := tx.QueryContext(ctx, query)
	if err != nil {
		return snap, err
	}
	for rows.Next() {
		var r legacyInstance
		if err := rows.Scan(&r.ClusterID, &r.ClusterName, &r.ClusterType, &r.MachineType, &r.AccessLayer, &r.Role, &r.Status, &r.Host, &r.Port, &r.AdminPort, &r.BkBizID, &r.BkCloudID); err != nil {
			rows.Close()
			return snap, err
		}
		snap.Instances = append(snap.Instances, r)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return snap, err
	}
	rows.Close()
	if ok, e := tableExists(ctx, tx, schema, "t_db_switching_strategy"); e != nil {
		return snap, e
	} else if ok {
		query = fmt.Sprintf("SELECT id,status,name,bk_biz_id,trigger_event_name,trigger_event_name_reason,trigger_count,priority,scope,action,description FROM `%s`.`t_db_switching_strategy` WHERE deleted_at IS NULL", schema)
		rows, err = tx.QueryContext(ctx, query)
		if err != nil {
			return snap, err
		}
		for rows.Next() {
			var id int
			var status, name, event, reason, scope, action, description string
			var biz, count, priority int
			if err := rows.Scan(&id, &status, &name, &biz, &event, &reason, &count, &priority, &scope, &action, &description); err != nil {
				rows.Close()
				return snap, err
			}
			value, _ := json.Marshal(map[string]any{"name": name, "bk_biz_id": biz, "trigger_event_name": event, "trigger_event_name_reason": reason, "trigger_count": count, "priority": priority, "scope": scope, "action": action, "description": description})
			snap.Policies = append(snap.Policies, legacyPolicy{ID: fmt.Sprintf("strategy:%d", id), Status: status, Value: value})
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return snap, err
		}
		rows.Close()
	}
	if ok, e := tableExists(ctx, tx, schema, "t_db_black_white_list"); e != nil {
		return snap, e
	} else if ok {
		query = fmt.Sprintf("SELECT id,status,switch_version,bk_biz_id,bk_cloud_id,cluster_id,cluster_name FROM `%s`.`t_db_black_white_list`", schema)
		rows, err = tx.QueryContext(ctx, query)
		if err != nil {
			return snap, err
		}
		for rows.Next() {
			var id, biz, cloud, cluster int
			var status, version, name string
			if err := rows.Scan(&id, &status, &version, &biz, &cloud, &cluster, &name); err != nil {
				rows.Close()
				return snap, err
			}
			value, _ := json.Marshal(map[string]any{"switch_version": version, "bk_biz_id": biz, "bk_cloud_id": cloud, "cluster_id": cluster, "cluster_name": name})
			snap.Policies = append(snap.Policies, legacyPolicy{ID: fmt.Sprintf("whitelist:%d", id), Status: status, Value: value})
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return snap, err
		}
		rows.Close()
	}
	if ok, e := tableExists(ctx, tx, schema, "t_skip_dbinstance"); e != nil {
		return snap, e
	} else if ok {
		query = fmt.Sprintf("SELECT instance_ip,instance_port FROM `%s`.`t_skip_dbinstance` WHERE deleted_at IS NULL", schema)
		rows, err = tx.QueryContext(ctx, query)
		if err != nil {
			return snap, err
		}
		for rows.Next() {
			var x legacyExclusion
			if err := rows.Scan(&x.Host, &x.Port); err != nil {
				rows.Close()
				return snap, err
			}
			x.Reason = "imported legacy skip entry"
			snap.Exclusions = append(snap.Exclusions, x)
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return snap, err
		}
		rows.Close()
	}
	if err := tx.Commit(); err != nil {
		return snap, err
	}
	return snap, nil
}

func tableExists(ctx context.Context, tx *sql.Tx, schema, table string) (bool, error) {
	var count int
	err := tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM information_schema.tables WHERE table_schema=? AND table_name=?", schema, table).Scan(&count)
	return count == 1, err
}

// exportLogs deliberately leaves historical records outside etcd. It never
// turns old observations into current samples.
func exportLogs(ctx context.Context, dsn, schema, path string) error {
	if path == "" {
		return nil
	}
	if !schemaNamePattern.MatchString(schema) {
		return fmt.Errorf("invalid schema")
	}
	db, err := sql.Open("mysql", dsn)
	if err != nil {
		return err
	}
	defer db.Close()
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	success := false
	defer func() {
		f.Close()
		if !success {
			os.Remove(path)
		}
	}()
	enc := json.NewEncoder(f)
	for _, table := range []string{"t_db_switching_log", "t_db_switching_snapshot_log"} {
		var count int
		if err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM information_schema.tables WHERE table_schema=? AND table_name=?", schema, table).Scan(&count); err != nil {
			return err
		}
		if count == 0 {
			continue
		}
		rows, err := db.QueryContext(ctx, fmt.Sprintf("SELECT * FROM `%s`.`%s` ORDER BY id", schema, table))
		if err != nil {
			return err
		}
		cols, err := rows.Columns()
		if err != nil {
			rows.Close()
			return err
		}
		for rows.Next() {
			raw := make([]any, len(cols))
			ptr := make([]any, len(cols))
			for i := range raw {
				ptr[i] = &raw[i]
			}
			if err := rows.Scan(ptr...); err != nil {
				rows.Close()
				return err
			}
			record := map[string]any{}
			for i, k := range cols {
				v := raw[i]
				if b, ok := v.([]byte); ok {
					record[k] = string(b)
				} else {
					record[k] = v
				}
			}
			if err := enc.Encode(map[string]any{"source_table": table, "row": record}); err != nil {
				rows.Close()
				return err
			}
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return err
		}
		rows.Close()
	}
	if err := f.Sync(); err != nil {
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	success = true
	return nil
}
