package discovery

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strings"
)

type MySQLConfig struct {
	Network      string `json:"network"`
	Address      string `json:"address"`
	User         string `json:"user"`
	PasswordFile string `json:"password_file"`
	Port         uint32 `json:"port"`
}

type ProxyConfig struct {
	UUID              string `json:"uuid"`
	DataPort          uint32 `json:"data_port"`
	AdminPort         uint32 `json:"admin_port"`
	AdminNetwork      string `json:"admin_network"`
	AdminAddress      string `json:"admin_address"`
	AdminUser         string `json:"admin_user"`
	AdminPasswordFile string `json:"admin_password_file"`
}

type Config struct {
	ServerURL                string      `json:"server_url"`
	ServerGRPC               string      `json:"server_grpc"`
	ServerURLs               []string    `json:"server_urls"`
	ServerGRPCEndpoints      []string    `json:"server_grpc_endpoints"`
	CAFile                   string      `json:"ca_file"`
	TokenFile                string      `json:"token_file"`
	AgentID                  string      `json:"agent_id"`
	StateFile                string      `json:"state_file"`
	Kind                     string      `json:"kind"`
	AdvertiseHost            string      `json:"advertise_host"`
	MySQL                    MySQLConfig `json:"mysql"`
	Proxy                    ProxyConfig `json:"proxy"`
	RouteReconcileSignalFile string      `json:"route_reconcile_signal_file"`
}

func Load(path string) (Config, error) {
	var c Config
	b, err := os.ReadFile(path)
	if err != nil {
		return c, err
	}
	if err = json.Unmarshal(b, &c); err != nil {
		return c, err
	}
	if c.Kind != "mysql" && c.Kind != "proxy" {
		return c, errors.New("kind must be mysql or proxy")
	}
	if c.AdvertiseHost == "" {
		return c, errors.New("advertise_host is required")
	}
	if c.Kind == "mysql" && (c.MySQL.Address == "" || c.MySQL.Port == 0 || c.MySQL.PasswordFile == "") {
		return c, errors.New("mysql endpoint, port and password_file are required")
	}
	if c.Kind == "proxy" && (c.Proxy.UUID == "" || c.Proxy.DataPort == 0 || c.Proxy.AdminPort == 0) {
		return c, errors.New("proxy uuid and ports are required")
	}
	return c, nil
}

func (c Config) ValidateRemote() error {
	urls, grpcEndpoints, err := c.serverEndpoints()
	if err != nil {
		return err
	}
	for _, raw := range urls {
		u, parseErr := url.Parse(raw)
		if parseErr != nil || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil || u.Host == "" {
			return fmt.Errorf("server URLs must be HTTP or HTTPS URLs without credentials")
		}
	}
	if len(grpcEndpoints) == 0 || c.TokenFile == "" || c.AgentID == "" || c.StateFile == "" {
		return fmt.Errorf("server endpoints, token, agent_id and state_file required")
	}
	return nil
}

func (c Config) serverEndpoints() ([]string, []string, error) {
	urls := append([]string(nil), c.ServerURLs...)
	grpcEndpoints := append([]string(nil), c.ServerGRPCEndpoints...)
	if len(urls) == 0 && len(grpcEndpoints) == 0 {
		urls, grpcEndpoints = []string{c.ServerURL}, []string{c.ServerGRPC}
	}
	if len(urls) == 0 || len(urls) != len(grpcEndpoints) {
		return nil, nil, fmt.Errorf("server_urls and server_grpc_endpoints must be non-empty and have equal length")
	}
	seenHTTP, seenGRPC := make(map[string]struct{}, len(urls)), make(map[string]struct{}, len(urls))
	for i := range urls {
		urls[i], grpcEndpoints[i] = strings.TrimSuffix(strings.TrimSpace(urls[i]), "/"), strings.TrimSpace(grpcEndpoints[i])
		if urls[i] == "" || grpcEndpoints[i] == "" {
			return nil, nil, fmt.Errorf("server endpoints must not be empty")
		}
		if _, ok := seenHTTP[urls[i]]; ok {
			return nil, nil, fmt.Errorf("duplicate server URL %q", urls[i])
		}
		if _, ok := seenGRPC[grpcEndpoints[i]]; ok {
			return nil, nil, fmt.Errorf("duplicate server gRPC endpoint %q", grpcEndpoints[i])
		}
		seenHTTP[urls[i]], seenGRPC[grpcEndpoints[i]] = struct{}{}, struct{}{}
	}
	return urls, grpcEndpoints, nil
}

func readSecret(path string) (string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(b)), nil
}
