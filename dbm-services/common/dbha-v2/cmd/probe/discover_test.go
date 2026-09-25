package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	probediscovery "dbm-services/common/dbha-v2/internal/probe/discovery"
)

func TestDiscoverHealthDistinguishesDBDownFromMissingCredentials(t *testing.T) {
	dir := t.TempDir()
	password := filepath.Join(dir, "password")
	if err := os.WriteFile(password, []byte("not-used"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfgPath := filepath.Join(dir, "discover.json")
	cfg := probediscovery.Config{Kind: "mysql", AdvertiseHost: "127.0.0.1", MySQL: probediscovery.MySQLConfig{Network: "tcp", Address: "127.0.0.1:1", User: "probe", PasswordFile: password, Port: 3306}}
	write := func() {
		b, err := json.Marshal(cfg)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(cfgPath, b, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write()
	if got := run([]string{"discover-health", "-c", cfgPath}); got != 2 {
		t.Fatalf("DB connection failure returned %d, want 2", got)
	}
	cfg.MySQL.PasswordFile = filepath.Join(dir, "missing")
	write()
	if got := run([]string{"discover-health", "-c", cfgPath}); got != 3 {
		t.Fatalf("missing credentials returned %d, want 3", got)
	}
}
