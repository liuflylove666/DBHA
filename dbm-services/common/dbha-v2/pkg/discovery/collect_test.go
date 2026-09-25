package discovery

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"io"
	"net"
	"strings"
	"testing"
)

type queryConnector struct{ failReplica, failProxyNetwork, failProxyPermission, proxyEmpty bool }

func (c queryConnector) Connect(context.Context) (driver.Conn, error) {
	return queryConn{c.failReplica, c.failProxyNetwork, c.failProxyPermission, c.proxyEmpty}, nil
}
func (c queryConnector) Driver() driver.Driver { return queryDriver{} }

type queryDriver struct{}

func (queryDriver) Open(string) (driver.Conn, error) { return queryConn{}, nil }

type queryConn struct{ failReplica, failProxyNetwork, failProxyPermission, proxyEmpty bool }

func (queryConn) Prepare(string) (driver.Stmt, error) { return nil, errors.New("unexpected Prepare") }
func (queryConn) Close() error                        { return nil }
func (queryConn) Begin() (driver.Tx, error)           { return nil, errors.New("unexpected Begin") }
func (c queryConn) QueryContext(_ context.Context, query string, _ []driver.NamedValue) (driver.Rows, error) {
	if strings.Contains(query, "backends") {
		if query != "select * from backends" {
			return nil, errors.New("vendor admin.lua rejected non-exact query")
		}
		if c.failProxyNetwork {
			return nil, &net.OpError{Op: "dial", Net: "tcp", Err: errors.New("connection refused")}
		}
		if c.failProxyPermission {
			return nil, errors.New("access denied for proxy admin")
		}
		values := [][]driver.Value{{int64(1), "10.0.0.1:3306", "up", "rw", "11111111-1111-4111-8111-111111111111", int64(0)}}
		if c.proxyEmpty {
			values = nil
		}
		return &queryRows{columns: []string{"backend_ndx", "address", "state", "type", "uuid", "connected_clients"}, values: values}, nil
	}
	if len(query) > 4 && query[:4] == "SHOW" {
		if c.failReplica {
			return nil, errors.New("replication permission denied")
		}
		return &queryRows{columns: []string{"Channel_Name", "Source_UUID", "Source_Host", "Source_Port", "Replica_IO_Running", "Replica_SQL_Running", "Seconds_Behind_Source", "Retrieved_Gtid_Set", "Executed_Gtid_Set"}, values: [][]driver.Value{{"", "upstream-1", "10.0.0.1", "3306", "Yes", "Yes", "2", "a:1-10", "a:1-9"}, {"other", "upstream-2", "10.0.0.2", "3307", "No", "Yes", nil, "b:1", "b:1"}}}, nil
	}
	return &queryRows{columns: []string{"uuid", "id", "version", "read_only", "super_read_only", "gtid_mode"}, values: [][]driver.Value{{"self", "22", "8.0.41", "1", "1", "ON"}}}, nil
}

func TestCollectProxyParsesExactVendorSixColumnResponse(t *testing.T) {
	db := sql.OpenDB(queryConnector{})
	defer db.Close()
	o, err := CollectProxy(context.Background(), db, "10.0.0.3", 10000, 11000, "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbb3")
	if err != nil {
		t.Fatal(err)
	}
	if o.CollectionState != "OK" || o.BackendQueryState != "OK" || len(o.Backends) != 1 || o.Backends[0].Address != "10.0.0.1:3306" || o.Backends[0].UUID != "11111111-1111-4111-8111-111111111111" {
		t.Fatalf("vendor response parsed incorrectly: %+v", o)
	}
	noBackends := sql.OpenDB(queryConnector{proxyEmpty: true})
	defer noBackends.Close()
	o, err = CollectProxy(context.Background(), noBackends, "10.0.0.3", 10000, 11000, "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbb3")
	if err == nil || o.CollectionState == "OK" || o.BackendQueryState != "NOT_READY" || o.ErrorCode != "BACKENDS_EMPTY" {
		t.Fatalf("empty backend result marked healthy: %+v %v", o, err)
	}
}

func TestCollectProxyRegistersStartingOnlyOnNetworkFailure(t *testing.T) {
	db := sql.OpenDB(queryConnector{failProxyNetwork: true})
	defer db.Close()
	o, err := CollectProxy(context.Background(), db, "10.0.0.3", 10000, 11000, "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbb3")
	if err == nil || o.ProcessState != "STARTING" || o.BackendQueryState != "NOT_READY" || o.ProxyUUID == "" {
		t.Fatalf("network-down proxy lost STARTING identity: %+v %v", o, err)
	}
	permissionDB := sql.OpenDB(queryConnector{failProxyPermission: true})
	defer permissionDB.Close()
	o, err = CollectProxy(context.Background(), permissionDB, "10.0.0.3", 10000, 11000, "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbb3")
	if err == nil || o.ProcessState == "STARTING" || o.ErrorCode != "BACKEND_QUERY_FAILED" {
		t.Fatalf("SQL error falsely marked proxy stopped: %+v %v", o, err)
	}
}

type queryRows struct {
	columns []string
	values  [][]driver.Value
	index   int
}

func (r *queryRows) Columns() []string { return r.columns }
func (r *queryRows) Close() error      { return nil }
func (r *queryRows) Next(dest []driver.Value) error {
	if r.index == len(r.values) {
		return io.EOF
	}
	copy(dest, r.values[r.index])
	r.index++
	return nil
}

func TestCollectMySQLReadsEveryChannel(t *testing.T) {
	db := sql.OpenDB(queryConnector{})
	defer db.Close()
	obs, err := CollectMySQL(context.Background(), db, "10.0.0.3", 3306)
	if err != nil {
		t.Fatal(err)
	}
	if obs.CollectionState != "OK" || obs.ReplicationQueryState != "OK" || len(obs.Channels) != 2 {
		t.Fatalf("incomplete observation: %+v", obs)
	}
	if obs.Channels[0].SecondsBehindSource == nil || *obs.Channels[0].SecondsBehindSource != 2 || obs.Channels[1].SecondsBehindSource != nil {
		t.Fatalf("lag nullability lost: %+v", obs.Channels)
	}
	if obs.Channels[0].RetrievedGTIDSet != "a:1-10" {
		t.Fatal("retrieved GTID lost")
	}
}

func TestCollectMySQLDistinguishesQueryErrorFromZeroChannels(t *testing.T) {
	db := sql.OpenDB(queryConnector{failReplica: true})
	defer db.Close()
	obs, err := CollectMySQL(context.Background(), db, "10.0.0.3", 3306)
	if err == nil || obs.ReplicationQueryState != "ERROR" || obs.CollectionState != "ERROR" {
		t.Fatalf("query failure treated as empty channels: %+v, %v", obs, err)
	}
}
