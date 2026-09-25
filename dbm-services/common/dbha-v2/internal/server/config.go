package server

import (
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Config is local deployment configuration. Credentials never enter etcd records.
type Config struct {
	EnvironmentID              string                       `json:"environment_id"`
	NodeID                     string                       `json:"node_id,omitempty"`
	AdvertiseHTTP              string                       `json:"advertise_http,omitempty"`
	AdvertiseGRPC              string                       `json:"advertise_grpc,omitempty"`
	HTTPListen                 string                       `json:"http_listen"`
	GRPCListen                 string                       `json:"grpc_listen"`
	EtcdEndpoints              []string                     `json:"etcd_endpoints"`
	EtcdCAFile                 string                       `json:"etcd_ca_file,omitempty"`
	EtcdCertFile               string                       `json:"etcd_cert_file,omitempty"`
	EtcdKeyFile                string                       `json:"etcd_key_file,omitempty"`
	TLSCertFile                string                       `json:"tls_cert_file,omitempty"`
	TLSKeyFile                 string                       `json:"tls_key_file,omitempty"`
	CAFile                     string                       `json:"ca_file,omitempty"`
	AdminTokenFile             string                       `json:"admin_token_file"`
	StateDir                   string                       `json:"state_dir"`
	LogFile                    string                       `json:"log_file"`
	AllowedNetworks            []string                     `json:"allowed_networks"`
	Profiles                   map[string]CredentialProfile `json:"profiles"`
	MaxReplicationDelaySeconds int                          `json:"max_replication_delay_seconds"`
	FailureThreshold           int                          `json:"failure_threshold"`
	EtcdQuotaBytes             int64                        `json:"etcd_quota_bytes"`
}

type CredentialProfile struct {
	MySQLUser          string `json:"mysql_user"`
	MySQLPassword      string `json:"mysql_password"`
	ProxyUser          string `json:"proxy_user"`
	ProxyPassword      string `json:"proxy_password"`
	SSHUser            string `json:"ssh_user"`
	SSHPassword        string `json:"ssh_password,omitempty"`
	SSHKeyFile         string `json:"ssh_key_file,omitempty"`
	SSHKnownHostsFile  string `json:"ssh_known_hosts_file"`
	SSHPort            int    `json:"ssh_port"`
	ProbeHealthCommand string `json:"probe_health_command"`
	ProxyPIDFile       string `json:"proxy_pid_file,omitempty"`
}

func LoadConfig(path string) (Config, error) {
	c := Config{HTTPListen: ":8080", GRPCListen: ":50052", StateDir: "/var/lib/dbha-server", LogFile: "/var/log/dbha-server/operations.jsonl", MaxReplicationDelaySeconds: 30, FailureThreshold: 3, EtcdQuotaBytes: 2 << 30}
	b, err := os.ReadFile(path)
	if err != nil {
		return c, err
	}
	dec := json.NewDecoder(strings.NewReader(string(b)))
	dec.DisallowUnknownFields()
	if err = dec.Decode(&c); err != nil {
		return c, err
	}
	if c.EnvironmentID == "" || strings.ContainsAny(c.EnvironmentID, "/\\") || len(c.EtcdEndpoints) == 0 {
		return c, errors.New("environment_id and etcd_endpoints are required")
	}
	if (c.TLSCertFile == "") != (c.TLSKeyFile == "") {
		return c, errors.New("both TLS certificate and key are required")
	}
	if c.AdminTokenFile == "" || c.StateDir == "" {
		return c, errors.New("admin_token_file and state_dir are required")
	}
	if c.MaxReplicationDelaySeconds < 1 || c.FailureThreshold < 2 || c.EtcdQuotaBytes < 1 {
		return c, errors.New("invalid decision or capacity limits")
	}
	if len(c.AllowedNetworks) == 0 {
		return c, errors.New("allowed_networks is required; observation addresses are not trusted")
	}
	for _, value := range c.AllowedNetworks {
		if _, _, err := net.ParseCIDR(value); err != nil {
			return c, fmt.Errorf("invalid allowed network: %s", value)
		}
	}
	for name, p := range c.Profiles {
		if p.SSHPort == 0 {
			p.SSHPort = 22
		}
		if p.ProbeHealthCommand == "" {
			p.ProbeHealthCommand = "/usr/local/bin/dbha-probe discover-health -c /etc/dbha/discovery.json"
		}
		if p.ProxyPIDFile == "" {
			p.ProxyPIDFile = "/run/dbha/mysql-proxy.pid"
		}
		c.Profiles[name] = p
	}
	if _, ok := c.Profiles["default"]; !ok {
		return c, errors.New("default credential profile is required")
	}
	return c, nil
}

func clientTLS(ca, cert, key string) (*tls.Config, error) {
	c := &tls.Config{MinVersion: tls.VersionTLS12}
	if ca != "" {
		b, err := os.ReadFile(ca)
		if err != nil {
			return nil, err
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(b) {
			return nil, errors.New("invalid CA certificate")
		}
		c.RootCAs = pool
	}
	if (cert == "") != (key == "") {
		return nil, errors.New("both certificate and key are required")
	}
	if cert != "" {
		pair, err := tls.LoadX509KeyPair(cert, key)
		if err != nil {
			return nil, err
		}
		c.Certificates = []tls.Certificate{pair}
	}
	return c, nil
}

func writePrivate(path string, b []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".dbha-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err = f.Write(b); err != nil {
		f.Close()
		return err
	}
	if err = f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	if err = os.Rename(f.Name(), path); err != nil {
		return err
	}
	d, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}

func readRestoreMarker(stateDir string) (string, bool, error) {
	b, err := os.ReadFile(filepath.Join(stateDir, "restore-required"))
	if os.IsNotExist(err) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	marker := strings.TrimSpace(string(b))
	if marker == "" {
		return "", false, errors.New("restore marker is empty")
	}
	return marker, true, nil
}

func removeRestoreMarker(stateDir string) error {
	path := filepath.Join(stateDir, "restore-required")
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return err
	}
	d, err := os.Open(stateDir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}

func adminToken(path string, allowCreate bool) (string, error) {
	b, err := os.ReadFile(path)
	if err == nil {
		st, err := os.Stat(path)
		if err != nil {
			return "", err
		}
		if st.Mode().Perm()&0077 != 0 {
			return "", errors.New("admin token file must have mode 0600")
		}
		v := strings.TrimSpace(string(b))
		if len(v) < 32 {
			return "", errors.New("invalid administrator token")
		}
		return v, nil
	}
	if !os.IsNotExist(err) || !allowCreate {
		return "", err
	}
	b = make([]byte, 32)
	if _, err = rand.Read(b); err != nil {
		return "", err
	}
	token := hex.EncodeToString(b)
	return token, writePrivate(path, []byte(token+"\n"))
}

const requestTimeout = 5 * time.Second
