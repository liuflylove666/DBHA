package discovery

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"
	"time"

	mysql "github.com/go-sql-driver/mysql"
)

// LocalMySQL describes the single local endpoint that the installer selected.
// No network discovery is performed.
type LocalMySQL struct {
	Network       string `json:"network" yaml:"network"`
	Address       string `json:"address" yaml:"address"`
	User          string `json:"user" yaml:"user"`
	Password      string `json:"password" yaml:"password"`
	AdvertiseHost string `json:"advertise_host" yaml:"advertise_host"`
	Port          uint32 `json:"port" yaml:"port"`
}

func OpenLocalMySQL(c LocalMySQL) (*sql.DB, error) {
	if c.Network != "tcp" && c.Network != "unix" {
		return nil, fmt.Errorf("unsupported local MySQL network")
	}
	if c.Address == "" || c.User == "" {
		return nil, fmt.Errorf("local MySQL endpoint and user required")
	}
	cfg := mysql.NewConfig()
	cfg.Net, cfg.Addr, cfg.User, cfg.Passwd = c.Network, c.Address, c.User, c.Password
	cfg.Timeout, cfg.ReadTimeout, cfg.WriteTimeout = 2*time.Second, 2*time.Second, 2*time.Second
	connector, err := mysql.NewConnector(cfg)
	if err != nil {
		return nil, err
	}
	db := sql.OpenDB(connector)
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	return db, nil
}

// CollectMySQL queries identity, writeability and every replication channel.
// An inaccessible replication query is an ERROR, never an empty result.
func CollectMySQL(ctx context.Context, db *sql.DB, host string, port uint32) (Observation, error) {
	o := Observation{Kind: "mysql", AdvertiseHost: host, Port: port, CollectionState: "ERROR", ReplicationQueryState: "ERROR"}
	if db == nil {
		o.ErrorCode = "CONNECT_FAILED"
		return o, fmt.Errorf("nil mysql connection")
	}
	var readOnly, superReadOnly string
	err := db.QueryRowContext(ctx, "SELECT @@GLOBAL.server_uuid, @@GLOBAL.server_id, @@GLOBAL.version, @@GLOBAL.read_only, @@GLOBAL.super_read_only, @@GLOBAL.gtid_mode").Scan(&o.ServerUUID, &o.ServerID, &o.Version, &readOnly, &superReadOnly, &o.GTIDMode)
	if err != nil {
		o.ErrorCode = "IDENTITY_QUERY_FAILED"
		return o, err
	}
	o.ReadOnly = readOnly == "1" || strings.EqualFold(readOnly, "ON")
	o.SuperReadOnly = superReadOnly == "1" || strings.EqualFold(superReadOnly, "ON")
	rows, err := db.QueryContext(ctx, "SHOW REPLICA STATUS")
	if err != nil {
		o.ErrorCode = "REPLICATION_QUERY_FAILED"
		return o, err
	}
	defer rows.Close()
	cols, err := rows.Columns()
	if err != nil {
		o.ErrorCode = "REPLICATION_QUERY_FAILED"
		return o, err
	}
	for rows.Next() {
		values := make([]sql.NullString, len(cols))
		ptrs := make([]any, len(cols))
		for i := range values {
			ptrs[i] = &values[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			o.ErrorCode = "REPLICATION_QUERY_FAILED"
			o.Channels = nil
			return o, err
		}
		fields := make(map[string]string, len(cols))
		for i, col := range cols {
			fields[strings.ToLower(col)] = values[i].String
		}
		portNum, _ := strconv.ParseUint(fields["source_port"], 10, 32)
		var lag *int64
		if v := fields["seconds_behind_source"]; v != "" {
			if n, err := strconv.ParseInt(v, 10, 64); err == nil {
				lag = &n
			}
		}
		o.Channels = append(o.Channels, Channel{ChannelName: fields["channel_name"], SourceUUID: fields["source_uuid"], SourceHost: fields["source_host"], SourcePort: uint32(portNum), IORunning: strings.EqualFold(fields["replica_io_running"], "Yes"), SQLRunning: strings.EqualFold(fields["replica_sql_running"], "Yes"), SecondsBehindSource: lag, RetrievedGTIDSet: fields["retrieved_gtid_set"], ExecutedGTIDSet: fields["executed_gtid_set"]})
	}
	if rows.Err() != nil {
		o.ErrorCode = "REPLICATION_QUERY_FAILED"
		o.Channels = nil
		return o, rows.Err()
	}
	o.ReplicationQueryState, o.CollectionState, o.ErrorCode = "OK", "OK", ""
	return o, nil
}

// CollectProxy reads the existing BlueKing proxy admin table. A proxy that
// has not started is represented separately by its caller as STARTING.
func CollectProxy(ctx context.Context, db *sql.DB, host string, dataPort, adminPort uint32, uuid string) (Observation, error) {
	o := Observation{Kind: "proxy", ProxyUUID: uuid, AdvertiseHost: host, DataPort: dataPort, AdminPort: adminPort, ProcessState: "RUNNING", BackendQueryState: "ERROR", CollectionState: "ERROR"}
	if db == nil {
		o.ProcessState, o.BackendQueryState, o.ErrorCode = "STARTING", "NOT_READY", "PROXY_NOT_READY"
		return o, nil
	}
	// The bundled mysql-proxy admin.lua only recognizes this exact statement.
	rows, err := db.QueryContext(ctx, "select * from backends")
	if err != nil {
		var netErr *net.OpError
		if errors.As(err, &netErr) {
			o.ProcessState, o.BackendQueryState, o.ErrorCode = "STARTING", "NOT_READY", "PROXY_NOT_READY"
			return o, err
		}
		o.ErrorCode = "BACKEND_QUERY_FAILED"
		return o, err
	}
	defer rows.Close()
	columns, err := rows.Columns()
	if err != nil {
		o.ErrorCode = "BACKEND_QUERY_FAILED"
		return o, err
	}
	columnIndex := map[string]int{}
	for i, name := range columns {
		columnIndex[strings.ToLower(name)] = i
	}
	for _, required := range []string{"address", "state", "type"} {
		if _, ok := columnIndex[required]; !ok {
			o.ErrorCode = "BACKEND_SCHEMA_UNSUPPORTED"
			return o, fmt.Errorf("proxy backend column %s missing", required)
		}
	}
	for rows.Next() {
		values := make([]sql.NullString, len(columns))
		ptrs := make([]any, len(columns))
		for i := range values {
			ptrs[i] = &values[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			o.ErrorCode = "BACKEND_QUERY_FAILED"
			o.Backends = nil
			return o, err
		}
		field := func(name string) string {
			i, ok := columnIndex[name]
			if !ok {
				return ""
			}
			return values[i].String
		}
		b := Backend{Address: field("address"), State: field("state"), Type: field("type"), UUID: field("uuid")}
		if b.Address == "" || b.State == "" || b.Type == "" {
			o.ErrorCode = "BACKEND_SCHEMA_UNSUPPORTED"
			o.Backends = nil
			return o, fmt.Errorf("proxy backend row missing address, state or type")
		}
		o.Backends = append(o.Backends, b)
	}
	if rows.Err() != nil {
		o.ErrorCode = "BACKEND_QUERY_FAILED"
		o.Backends = nil
		return o, rows.Err()
	}
	if len(o.Backends) == 0 {
		o.BackendQueryState = "NOT_READY"
		o.ErrorCode = "BACKENDS_EMPTY"
		return o, fmt.Errorf("proxy reported no backends")
	}
	o.BackendQueryState, o.CollectionState = "OK", "OK"
	return o, nil
}

// CollectSample reuses the local connection for a real liveness and load
// measurement rather than inferring health from an open TCP port.
func CollectSample(ctx context.Context, db *sql.DB) (map[string]any, error) {
	var value string
	if err := db.QueryRowContext(ctx, "SHOW GLOBAL STATUS LIKE 'Threads_connected'").Scan(new(string), &value); err != nil {
		return nil, err
	}
	n, err := strconv.ParseUint(value, 10, 64)
	if err != nil {
		return nil, err
	}
	return map[string]any{"threads_connected": n, "collection_state": "OK"}, nil
}
