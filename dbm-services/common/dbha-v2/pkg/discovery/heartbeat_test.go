package discovery

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"io"
	"strings"
	"testing"
)

type heartbeatConnector struct{ conn *heartbeatConn }

func (c heartbeatConnector) Connect(context.Context) (driver.Conn, error) { return c.conn, nil }
func (c heartbeatConnector) Driver() driver.Driver                        { return queryDriver{} }

type heartbeatConn struct {
	statements []string
	binlogOff  bool
}

func (*heartbeatConn) Prepare(string) (driver.Stmt, error) {
	return nil, errors.New("unexpected prepare")
}
func (*heartbeatConn) Close() error              { return nil }
func (*heartbeatConn) Begin() (driver.Tx, error) { return nil, errors.New("unexpected begin") }
func (c *heartbeatConn) ExecContext(_ context.Context, q string, _ []driver.NamedValue) (driver.Result, error) {
	c.statements = append(c.statements, q)
	if q == "SET SESSION sql_log_bin=OFF" {
		c.binlogOff = true
		return driver.RowsAffected(1), nil
	}
	if strings.HasPrefix(q, "REPLACE INTO") && !c.binlogOff {
		return nil, errors.New("binlog still enabled")
	}
	if strings.Contains(q, "sql_log_bin=ON") {
		return nil, errors.New("unsafe binlog enable")
	}
	return driver.RowsAffected(1), nil
}
func (c *heartbeatConn) QueryContext(_ context.Context, q string, _ []driver.NamedValue) (driver.Rows, error) {
	c.statements = append(c.statements, q)
	var v driver.Value = int64(1)
	if strings.Contains(q, "@@server_id") {
		v = int64(42)
	}
	if strings.Contains(q, "TIMESTAMPDIFF") {
		v = int64(0)
	}
	return &heartbeatRows{v: v}, nil
}

type heartbeatRows struct {
	v    driver.Value
	used bool
}

func (*heartbeatRows) Columns() []string { return []string{"value"} }
func (*heartbeatRows) Close() error      { return nil }
func (r *heartbeatRows) Next(dest []driver.Value) error {
	if r.used {
		return io.EOF
	}
	dest[0] = r.v
	r.used = true
	return nil
}

func TestWriteHeartbeatUsesSameSessionAndNeverEnablesBinlog(t *testing.T) {
	conn := &heartbeatConn{}
	db := sql.OpenDB(heartbeatConnector{conn: conn})
	defer db.Close()
	sample, err := WriteHeartbeat(context.Background(), db, "10.0.0.1", 3306)
	if err != nil {
		t.Fatal(err)
	}
	if sample["collection_state"] != "OK" || sample["write_success"] != true {
		t.Fatalf("unexpected heartbeat sample: %+v", sample)
	}
	if len(conn.statements) < 5 || conn.statements[0] != "SET SESSION sql_log_bin=OFF" {
		t.Fatalf("session write guard missing: %+v", conn.statements)
	}
	for _, q := range conn.statements {
		if strings.Contains(q, "sql_log_bin=ON") || strings.Contains(q, "dbha_repl_heartbeat") {
			t.Fatalf("unsafe replication heartbeat command: %s", q)
		}
	}
}
