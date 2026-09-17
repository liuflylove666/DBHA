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

package config_test

import (
	"strings"
	"testing"
	"time"

	"dbm-services/common/dbha-v2/internal/admin/config"

	"github.com/spf13/viper"
)

func TestConfig(t *testing.T) {
	const yaml = `name: admin
grpc:
  listenAddress: 127.0.0.1:50051
probeMysql:
  interval: 10s
  timeout: 2s
`
	v := viper.New()
	v.SetConfigType("yaml")
	if err := v.ReadConfig(strings.NewReader(yaml)); err != nil {
		t.Fatalf("read config: %v", err)
	}
	var cfg config.Configuration
	if err := v.Unmarshal(&cfg); err != nil {
		t.Fatalf("unmarshal config: %v", err)
	}
	if cfg.Name != "admin" || cfg.Grpc.ListenAddress != "127.0.0.1:50051" ||
		cfg.ProbeMysql.Interval != 10*time.Second || cfg.ProbeMysql.Timeout != 2*time.Second {
		t.Fatalf("unexpected admin config: %+v", cfg)
	}
}
