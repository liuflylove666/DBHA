package discovery

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"dbm-services/common/dbha-v2/pkg/storage/hamodel"
)

const heartbeatFallbackDelay = 365 * 24 * 60 * 60

// WriteHeartbeat retains the old probe's local write probe. Every SQL command
// uses one pinned MySQL connection, because sql_log_bin is a session variable.
// It is always OFF: writing the old replication-heartbeat table with ROW
// binlogging is intentionally excluded from the new discovery path.
func WriteHeartbeat(ctx context.Context, db *sql.DB, host string, port uint32) (map[string]any, error) {
	failed := map[string]any{"collection_state": "ERROR", "error_code": "HEARTBEAT_WRITE_FAILED", "write_success": false, "heartbeat_delay_seconds": heartbeatFallbackDelay}
	if db == nil {
		return failed, errors.New("nil mysql connection")
	}
	conn, err := db.Conn(ctx)
	if err != nil {
		return failed, err
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, "SET SESSION sql_log_bin=OFF"); err != nil {
		return failed, err
	}
	var marker int
	err = conn.QueryRowContext(ctx, "SELECT 1 FROM information_schema.TABLES WHERE TABLE_SCHEMA = ? AND TABLE_NAME = ? LIMIT 1", hamodel.ProbeMysqlDbName, hamodel.DbhaHeartbeatTableName).Scan(&marker)
	if err != nil {
		if !errors.Is(err, sql.ErrNoRows) {
			return failed, err
		}
		if _, err = conn.ExecContext(ctx, hamodel.CreateProbeMysqlDbSQL); err != nil {
			return failed, err
		}
		if _, err = conn.ExecContext(ctx, hamodel.CreateDbhaHeartbeatTableSQL); err != nil {
			return failed, err
		}
	}
	var serverID uint64
	if err := conn.QueryRowContext(ctx, "SELECT @@server_id").Scan(&serverID); err != nil {
		return failed, err
	}
	writeSQL := fmt.Sprintf("REPLACE INTO `%s`.`%s` (`host`, `port`, `server_id`, `update_time`) VALUES (?, ?, ?, SYSDATE())", hamodel.ProbeMysqlDbName, hamodel.DbhaHeartbeatTableName)
	if _, err := conn.ExecContext(ctx, writeSQL, host, port, serverID); err != nil {
		return failed, err
	}
	readSQL := fmt.Sprintf("SELECT GREATEST(CAST(TIMESTAMPDIFF(SECOND, update_time, SYSDATE()) AS SIGNED), 0) FROM `%s`.`%s` WHERE host = ? AND port = ?", hamodel.ProbeMysqlDbName, hamodel.DbhaHeartbeatTableName)
	var delay int64
	if err := conn.QueryRowContext(ctx, readSQL, host, port).Scan(&delay); err != nil {
		return failed, err
	}
	return map[string]any{"collection_state": "OK", "write_success": true, "heartbeat_delay_seconds": delay}, nil
}
