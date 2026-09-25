package server

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
)

func TestControlRequestAnySkipsFollower(t *testing.T) {
	dir := t.TempDir()
	token := filepath.Join(dir, "admin.token")
	if err := os.WriteFile(token, []byte("0123456789abcdef0123456789abcdef\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var followerCalls, leaderCalls atomic.Int32
	follower := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		followerCalls.Add(1)
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(`{"error":{"code":"NOT_LEADER"}}`))
	}))
	defer follower.Close()
	leader := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		leaderCalls.Add(1)
		if r.Header.Get("Authorization") != "Bearer 0123456789abcdef0123456789abcdef" {
			t.Fatal("missing administrator token")
		}
		_, _ = w.Write([]byte(`{"data":{"ok":true}}`))
	}))
	defer leader.Close()

	body, err := ControlRequestAny(context.Background(), Config{AdminTokenFile: token}, []string{follower.URL, leader.URL}, http.MethodPost, "/api/v1/test", []byte(`{}`), nil)
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != `{"data":{"ok":true}}` || followerCalls.Load() != 1 || leaderCalls.Load() != 1 {
		t.Fatalf("unexpected failover result body=%s follower=%d leader=%d", body, followerCalls.Load(), leaderCalls.Load())
	}
}

func TestControlRequestAnyDoesNotHideApplicationError(t *testing.T) {
	dir := t.TempDir()
	token := filepath.Join(dir, "admin.token")
	if err := os.WriteFile(token, []byte("0123456789abcdef0123456789abcdef\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var secondCalls atomic.Int32
	first := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusConflict)
		_, _ = w.Write([]byte(`{"error":{"code":"CAS_CONFLICT"}}`))
	}))
	defer first.Close()
	second := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		secondCalls.Add(1)
	}))
	defer second.Close()

	_, err := ControlRequestAny(context.Background(), Config{AdminTokenFile: token}, []string{first.URL, second.URL}, http.MethodPut, "/api/v1/test", nil, nil)
	var responseErr *ControlHTTPError
	if err == nil || !errors.As(err, &responseErr) || responseErr.Code != "CAS_CONFLICT" || secondCalls.Load() != 0 {
		t.Fatalf("unexpected error=%v second_calls=%d", err, secondCalls.Load())
	}
}
