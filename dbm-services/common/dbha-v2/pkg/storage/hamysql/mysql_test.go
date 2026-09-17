/**
 * MIT License
 *
 * Copyright (c) 2023 腾讯蓝鲸
 *
 * Permission is hereby granted, free of charge, to any person obtaining a copy
 * of this software and associated documentation files (the "Software"), to deal
 * in the Software without restriction, including without limitation the rights
 * to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
 * copies of the Software, and to permit persons to whom the Software is
 * furnished to do so, subject to the following conditions:
 *
 * The above copyright notice and this permission notice shall be included in all
 * copies or substantial portions of the Software.
 *
 * THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
 * IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
 * FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
 * AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
 * LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
 * OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN THE
 * SOFTWARE.
 */

package hamysql_test

import (
	"context"
	"errors"
	"log"
	"net"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"dbm-services/common/dbha-v2/pkg/storage/hamysql"
)

type timeoutTestConfig struct {
	host           string
	port           int
	user           string
	password       string
	sqlText        string
	connectTimeout time.Duration
	execTimeout    time.Duration
}

func requiredEnv(t *testing.T, name string) string {
	t.Helper()
	value, ok := os.LookupEnv(name)
	if !ok {
		t.Skipf("external MySQL test requires %s", name)
	}
	return value
}

func getTimeoutTestConfig(t *testing.T) timeoutTestConfig {
	t.Helper()

	host := requiredEnv(t, "DBHA_MYSQL_TIMEOUT_HOST")
	portStr := requiredEnv(t, "DBHA_MYSQL_TIMEOUT_PORT")
	user := requiredEnv(t, "DBHA_MYSQL_TIMEOUT_USER")
	password := requiredEnv(t, "DBHA_MYSQL_TIMEOUT_PASSWORD")
	sqlText := requiredEnv(t, "DBHA_MYSQL_TIMEOUT_SQL")
	connectTimeoutStr := requiredEnv(t, "DBHA_MYSQL_CONNECT_TIMEOUT")
	execTimeoutStr := requiredEnv(t, "DBHA_MYSQL_EXEC_TIMEOUT")

	port, err := strconv.Atoi(portStr)
	if err != nil {
		t.Fatalf("invalid timeout port(%s), errmsg(%s)", portStr, err)
	}
	connectTimeout, err := time.ParseDuration(connectTimeoutStr)
	if err != nil {
		t.Fatalf("invalid connect timeout(%s), errmsg(%s)", connectTimeoutStr, err)
	}
	execTimeout, err := time.ParseDuration(execTimeoutStr)
	if err != nil {
		t.Fatalf("invalid exec timeout(%s), errmsg(%s)", execTimeoutStr, err)
	}

	return timeoutTestConfig{
		host:           host,
		port:           port,
		user:           user,
		password:       password,
		sqlText:        sqlText,
		connectTimeout: connectTimeout,
		execTimeout:    execTimeout,
	}
}

func TestNew(t *testing.T) {
	endpoints := requiredEnv(t, "DBHA_MYSQL_ENDPOINTS")
	user := requiredEnv(t, "DBHA_MYSQL_USER")
	password := requiredEnv(t, "DBHA_MYSQL_PASSWORD")
	host, portStr, err := net.SplitHostPort(strings.Split(endpoints, ",")[0])
	if err != nil {
		t.Fatalf("invalid MySQL endpoint %q: %v", endpoints, err)
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		t.Fatalf("invalid MySQL port %q: %v", portStr, err)
	}

	hadb, err := hamysql.NewGormDB(hamysql.OptionIP(host), hamysql.OptionPort(port), hamysql.OptionUser(user), hamysql.OptionPassword(password))
	if err != nil {
		t.Fatalf("create mysql instance failed, errmsg(%v)", err)
	}
	defer hadb.Close()

	tables, err := hadb.DB().Migrator().GetTables()
	if err != nil {
		t.Fatalf("failed to get all tables, errmsg(%s)", err)
	}

	for _, table := range tables {
		t.Logf("table(%s)", table)
	}

}

func TestSqlxDBForProxy(t *testing.T) {
	host := requiredEnv(t, "PROXY_HOST")
	portStr := requiredEnv(t, "PROXY_PORT")
	user := requiredEnv(t, "PROXY_USER")
	password := requiredEnv(t, "PROXY_PASSWORD")
	port, err := strconv.Atoi(portStr)
	if err != nil {
		t.Fatalf("invalid port(%s), errmsg(%s)", os.Getenv("PROXY_PORT"), err)
	}

	log.Println("This mysql connection configuration is designed for proxy or tdbctl node only, " +
		"make sure there is no extra sql is executed when building this connection")
	proxyDB, err := hamysql.NewSqlxDB(
		hamysql.OptionProto("tcp"),
		hamysql.OptionIP(host),
		hamysql.OptionPort(port),
		hamysql.OptionUser(user),
		hamysql.OptionPassword(password),
		hamysql.OptionCharset(""),
	)
	if err != nil {
		t.Fatalf("failed to connect to proxy(%s:%d), errmsg: %s",
			host, port, err.Error())
	}

	defer proxyDB.Close()

	// test query
	var version []string
	if err := proxyDB.DB().Select(&version, "SELECT VERSION()"); err != nil {
		t.Fatalf("failed to query version, errmsg: %s", err)
	}
	log.Println("proxy version: ", version[0])
}

func TestGormDBTimeout(t *testing.T) {
	cfg := getTimeoutTestConfig(t)

	log.Println("host:", cfg.host)
	log.Println("port:", cfg.port)
	log.Println("user:", cfg.user)
	log.Println("sql:", cfg.sqlText)
	log.Println("connect timeout:", cfg.connectTimeout)
	log.Println("exec timeout:", cfg.execTimeout)

	start := time.Now()

	db, err := hamysql.NewGormDB(
		hamysql.OptionProto("tcp"),
		hamysql.OptionIP(cfg.host),
		hamysql.OptionPort(cfg.port),
		hamysql.OptionUser(cfg.user),
		hamysql.OptionPassword(cfg.password),
		hamysql.OptionTimeout(cfg.connectTimeout),
	)
	if err != nil {
		elapsed := time.Since(start)
		log.Printf("gorm create failed, timeout=%t, err=%v, elapsed=%s",
			os.IsTimeout(err) || errors.Is(err, context.DeadlineExceeded), err, elapsed)
		t.Fatalf("gorm connection failed: %v", err)
	}
	defer db.Close()

	ctx, cancel := context.WithTimeout(context.Background(), cfg.execTimeout)
	defer cancel()

	err = db.DBWithContext(ctx).Exec(cfg.sqlText).Error
	elapsed := time.Since(start)
	log.Printf("gorm exec success=%t, timeout=%t, ctxErr=%v, err=%v, elapsed=%s",
		err == nil,
		os.IsTimeout(err) || errors.Is(err, context.DeadlineExceeded) || errors.Is(ctx.Err(), context.DeadlineExceeded),
		ctx.Err(),
		err,
		elapsed,
	)
}

func TestSqlxDBTimeout(t *testing.T) {
	cfg := getTimeoutTestConfig(t)

	log.Println("host:", cfg.host)
	log.Println("port:", cfg.port)
	log.Println("user:", cfg.user)
	log.Println("sql:", cfg.sqlText)
	log.Println("connect timeout:", cfg.connectTimeout)
	log.Println("exec timeout:", cfg.execTimeout)

	start := time.Now()

	db, err := hamysql.NewSqlxDB(
		hamysql.OptionProto("tcp"),
		hamysql.OptionIP(cfg.host),
		hamysql.OptionPort(cfg.port),
		hamysql.OptionUser(cfg.user),
		hamysql.OptionPassword(cfg.password),
		hamysql.OptionTimeout(cfg.connectTimeout),
	)
	if err != nil {
		elapsed := time.Since(start)
		log.Printf("sqlx create failed, timeout=%t, err=%v, elapsed=%s",
			os.IsTimeout(err) || errors.Is(err, context.DeadlineExceeded), err, elapsed)
		t.Fatalf("sqlx connection failed: %v", err)
	}
	defer db.Close()

	ctx, cancel := context.WithTimeout(context.Background(), cfg.execTimeout)
	defer cancel()

	_, err = db.DB().ExecContext(ctx, cfg.sqlText)
	elapsed := time.Since(start)
	log.Printf("sqlx exec success=%t, timeout=%t, ctxErr=%v, err=%v, elapsed=%s",
		err == nil,
		os.IsTimeout(err) || errors.Is(err, context.DeadlineExceeded) || errors.Is(ctx.Err(), context.DeadlineExceeded),
		ctx.Err(),
		err,
		elapsed,
	)
}
