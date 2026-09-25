package server

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadConfigAllowsPlainTransport(t *testing.T) {
	path := filepath.Join(t.TempDir(), "server.json")
	config := `{
  "environment_id":"test",
  "etcd_endpoints":["http://127.0.0.1:2379"],
  "admin_token_file":"/tmp/admin.token",
  "state_dir":"/tmp/dbha-state",
  "allowed_networks":["127.0.0.0/8"],
  "profiles":{"default":{}}
}`
	if err := os.WriteFile(path, []byte(config), 0600); err != nil {
		t.Fatal(err)
	}
	loaded, err := LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.TLSCertFile != "" || loaded.TLSKeyFile != "" || loaded.CAFile != "" {
		t.Fatal("plain transport unexpectedly requires TLS files")
	}
}

func TestLoadConfigRejectsIncompleteTLSPair(t *testing.T) {
	path := filepath.Join(t.TempDir(), "server.json")
	config := `{
  "environment_id":"test",
  "etcd_endpoints":["http://127.0.0.1:2379"],
  "tls_cert_file":"/tmp/server.crt",
  "admin_token_file":"/tmp/admin.token",
  "state_dir":"/tmp/dbha-state",
  "allowed_networks":["127.0.0.0/8"],
  "profiles":{"default":{}}
}`
	if err := os.WriteFile(path, []byte(config), 0600); err != nil {
		t.Fatal(err)
	}
	_, err := LoadConfig(path)
	if err == nil || !strings.Contains(err.Error(), "both TLS certificate and key") {
		t.Fatalf("expected incomplete TLS pair error, got %v", err)
	}
}
