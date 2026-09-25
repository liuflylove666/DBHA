package server

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"path/filepath"
	"strings"
	"time"

	clientv3 "go.etcd.io/etcd/client/v3"
)

type ControlHTTPError struct {
	Status int
	Code   string
}

func (e *ControlHTTPError) Error() string {
	if e.Code == "" {
		return fmt.Sprintf("control request returned HTTP %d", e.Status)
	}
	return fmt.Sprintf("control request returned HTTP %d: %s", e.Status, e.Code)
}

func ControlRequest(ctx context.Context, c Config, base, method, path string, body []byte, headers map[string]string) ([]byte, error) {
	if base == "" {
		_, port, e := net.SplitHostPort(c.HTTPListen)
		if e != nil {
			return nil, e
		}
		scheme := "http"
		if c.TLSCertFile != "" {
			scheme = "https"
		}
		base = scheme + "://127.0.0.1:" + port
	}
	u, e := url.Parse(base)
	if e != nil || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil || u.Host == "" {
		return nil, errors.New("server must be an HTTP or HTTPS URL without credentials")
	}
	if !strings.HasPrefix(path, "/api/v1/") {
		return nil, errors.New("API path must start with /api/v1/")
	}
	token, e := adminToken(c.AdminTokenFile, false)
	if e != nil {
		return nil, e
	}
	transport := &http.Transport{}
	if u.Scheme == "https" {
		tlsConfig, err := clientTLS(c.CAFile, "", "")
		if err != nil {
			return nil, err
		}
		transport.TLSClientConfig = tlsConfig
	}
	client := &http.Client{Transport: transport, Timeout: 20 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	req, e := http.NewRequestWithContext(ctx, method, strings.TrimRight(base, "/")+path, bytes.NewReader(body))
	if e != nil {
		return nil, e
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	response, e := client.Do(req)
	if e != nil {
		return nil, fmt.Errorf("control request failed: %w", e)
	}
	defer response.Body.Close()
	result, e := io.ReadAll(io.LimitReader(response.Body, 8<<20))
	if e != nil {
		return nil, e
	}
	if response.StatusCode >= 300 {
		var envelope struct {
			Error struct {
				Code string `json:"code"`
			} `json:"error"`
		}
		_ = json.Unmarshal(result, &envelope)
		return result, &ControlHTTPError{Status: response.StatusCode, Code: envelope.Error.Code}
	}
	return result, nil
}

// ControlRequestAny retries only transport failures and explicit follower
// responses. Reusing the exact body and headers preserves the API's existing
// idempotency and expected-revision guards.
func ControlRequestAny(ctx context.Context, c Config, bases []string, method, path string, body []byte, headers map[string]string) ([]byte, error) {
	if len(bases) == 0 {
		return ControlRequest(ctx, c, "", method, path, body, headers)
	}
	var lastBody []byte
	var lastErr error
	for _, base := range bases {
		result, err := ControlRequest(ctx, c, base, method, path, body, headers)
		if err == nil {
			return result, nil
		}
		lastBody, lastErr = result, err
		var responseErr *ControlHTTPError
		if errors.As(err, &responseErr) && responseErr.Code != "NOT_LEADER" {
			return result, err
		}
	}
	return lastBody, lastErr
}

// OfflineControl acquires the same owner lock as the service, so these local
// maintenance actions cannot race a running controller.
func OfflineControl(ctx context.Context, c Config, action string) error {
	tlsConfig, err := clientTLS(c.EtcdCAFile, c.EtcdCertFile, c.EtcdKeyFile)
	if err != nil {
		return err
	}
	if c.EtcdCAFile == "" && c.EtcdCertFile == "" {
		tlsConfig = nil
	}
	client, err := clientv3.New(clientv3.Config{Endpoints: c.EtcdEndpoints, TLS: tlsConfig, DialTimeout: requestTimeout})
	if err != nil {
		return err
	}
	defer client.Close()
	store, err := NewStore(ctx, client, c.EnvironmentID)
	if err != nil {
		return err
	}
	defer store.Close()
	switch action {
	case "prepare-restore":
		marker, err := newToken()
		if err != nil {
			return err
		}
		return writePrivate(filepath.Join(c.StateDir, "restore-required"), []byte(marker+"\n"))
	case "reset-admin-token":
		token, err := newToken()
		if err != nil {
			return err
		}
		sum := sha256.Sum256([]byte(token))
		// Write the local recovery credential first; if the etcd write fails,
		// this same offline command can be rerun while the service is stopped.
		if err = writePrivate(c.AdminTokenFile, []byte(token+"\n")); err != nil {
			return err
		}
		return store.Update(ctx, func(st *State) error {
			st.Credentials["admin"] = Credential{ID: "admin", Admin: true, TokenHash: hex.EncodeToString(sum[:])}
			return nil
		})
	default:
		return errors.New("unknown offline operation")
	}
}
