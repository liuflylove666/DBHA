package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

type fakeStore struct{ updates, swaps int }

func (f *fakeStore) PingContext(context.Context) error { return nil }
func (f *fakeStore) queryMetadata(context.Context, metadataRequest) ([]*instanceMetadata, error) {
	return []*instanceMetadata{{IP: "10.203.80.21", Port: 3306}}, nil
}
func (f *fakeStore) updateStatus(context.Context, updateStatusRequest) error { f.updates++; return nil }
func (f *fakeStore) swapRole(context.Context, swapRoleRequest) error         { f.swaps++; return nil }

func TestEmptyMutationsHaveNoSideEffects(t *testing.T) {
	f := &fakeStore{}
	h := newAPIWithStore(f, "secret")
	for _, path := range []string{"/api/v1/update-status", "/api/v1/swap-mysql-role"} {
		r := httptest.NewRequest(http.MethodPost, path, strings.NewReader(`{"db_cloud_token":"secret","payloads":[]}`))
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != http.StatusOK {
			t.Fatalf("%s returned %d: %s", path, w.Code, w.Body.String())
		}
	}
	if f.updates != 0 || f.swaps != 0 {
		t.Fatalf("empty requests reached store: updates=%d swaps=%d", f.updates, f.swaps)
	}
}

func TestMetadataContractAndAuth(t *testing.T) {
	h := newAPIWithStore(&fakeStore{}, "secret")
	r := httptest.NewRequest(http.MethodPost, "/api/v1/metadata", strings.NewReader(`{"bk_cloud_id":0,"db_cloud_token":"secret","addresses":["10.203.80.21"]}`))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("got %d: %s", w.Code, w.Body.String())
	}
	var body struct {
		Result bool               `json:"result"`
		Data   []instanceMetadata `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if !body.Result || len(body.Data) != 1 || body.Data[0].IP != "10.203.80.21" {
		t.Fatalf("unexpected response: %+v", body)
	}
	r = httptest.NewRequest(http.MethodPost, "/api/v1/metadata", strings.NewReader(`{"bk_cloud_id":0,"db_cloud_token":"wrong"}`))
	w = httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("unauthorized request returned %d", w.Code)
	}
}

func TestMySQLSwapRoleTransactionIsIdempotent(t *testing.T) {
	dsn := os.Getenv("MYSQL_TEST_DSN")
	if dsn == "" {
		t.Skip("MYSQL_TEST_DSN is not set")
	}
	database, err := sql.Open("mysql", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	const cloudID, clusterID = 999, 900001
	_, _ = database.Exec(`DELETE FROM standalone_instances WHERE bk_cloud_id=? AND cluster_id=?`, cloudID, clusterID)
	defer database.Exec(`DELETE FROM standalone_instances WHERE bk_cloud_id=? AND cluster_id=?`, cloudID, clusterID)
	_, err = database.Exec(`INSERT INTO standalone_instances (host_id,bk_cloud_id,bk_biz_id,cluster_id,cluster,cluster_type,machine_type,access_layer,instance_role,status,ip,port,admin_port) VALUES (121,?,?,?,'integration.test','tendbha','backend','storage','backend_master','running','10.203.80.121',3306,0),(122,?,?,?,'integration.test','tendbha','backend','storage','backend_slave','running','10.203.80.122',3306,0),(131,?,?,?,'integration.test','tendbha','proxy','proxy','','running','10.203.80.131',10000,11000),(132,?,?,?,'integration.test','tendbha','proxy','proxy','','running','10.203.80.132',10000,11000)`, cloudID, 1, clusterID, cloudID, 1, clusterID, cloudID, 1, clusterID, cloudID, 1, clusterID)
	if err != nil {
		t.Fatal(err)
	}
	s := &sqlStore{db: database}
	before, err := s.queryMetadata(context.Background(), metadataRequest{BkCloudID: cloudID, Addresses: []string{"10.203.80.121"}})
	if err != nil || len(before) != 1 || len(before[0].Receiver) != 1 || before[0].Receiver[0].IP != "10.203.80.122" || len(before[0].ProxyInstances) != 2 {
		t.Fatalf("initial topology is incomplete: data=%+v err=%v", before, err)
	}
	req := swapRoleRequest{BkCloudID: cloudID}
	req.Payloads = append(req.Payloads, swapPayload{endpoint{"10.203.80.121", 3306}, endpoint{"10.203.80.122", 3306}})
	if err := s.swapRole(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	if err := s.swapRole(context.Background(), req); err != nil {
		t.Fatalf("retry failed: %v", err)
	}
	var oldRole, newRole string
	if err := database.QueryRow(`SELECT instance_role FROM standalone_instances WHERE bk_cloud_id=? AND ip='10.203.80.121' AND port=3306`, cloudID).Scan(&oldRole); err != nil {
		t.Fatal(err)
	}
	if err := database.QueryRow(`SELECT instance_role FROM standalone_instances WHERE bk_cloud_id=? AND ip='10.203.80.122' AND port=3306`, cloudID).Scan(&newRole); err != nil {
		t.Fatal(err)
	}
	if oldRole != "backend_slave" || newRole != "backend_master" {
		t.Fatalf("retry reversed roles: old=%s new=%s", oldRole, newRole)
	}
	after, err := s.queryMetadata(context.Background(), metadataRequest{BkCloudID: cloudID, Addresses: []string{"10.203.80.122"}})
	if err != nil || len(after) != 1 || len(after[0].Receiver) != 1 || after[0].Receiver[0].IP != "10.203.80.121" || len(after[0].ProxyInstances) != 2 {
		t.Fatalf("swapped topology is incomplete: data=%+v err=%v", after, err)
	}
	statusReq := updateStatusRequest{BkCloudID: cloudID}
	statusReq.Payloads = append(statusReq.Payloads, struct {
		IP     string `json:"ip"`
		Port   int    `json:"port"`
		Status string `json:"status"`
	}{"10.203.80.121", 3306, "unavailable"})
	if err := s.updateStatus(context.Background(), statusReq); err != nil {
		t.Fatal(err)
	}
	if err := s.updateStatus(context.Background(), statusReq); err != nil {
		t.Fatalf("idempotent status retry failed: %v", err)
	}
}

func TestPlanSwapIsDirectionalAndIdempotent(t *testing.T) {
	master := roleState{clusterID: 1, machineType: "backend", role: "backend_master"}
	slave := roleState{clusterID: 1, machineType: "backend", role: "backend_slave"}
	do, err := planSwap(master, slave)
	if err != nil || !do {
		t.Fatalf("initial transition: do=%v err=%v", do, err)
	}
	do, err = planSwap(slave, master)
	if err != nil || do {
		t.Fatalf("retry transition: do=%v err=%v", do, err)
	}
	_, err = planSwap(master, roleState{clusterID: 2, machineType: "backend", role: "backend_slave"})
	if err == nil {
		t.Fatal("cross-cluster swap accepted")
	}
}

func TestUnsupportedIntegrationFailsClosed(t *testing.T) {
	h := newAPIWithStore(&fakeStore{}, "secret")
	r := httptest.NewRequest(http.MethodPost, "/api/v1/dumper/switch", strings.NewReader(`{}`))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated request returned %d", w.Code)
	}
	r = httptest.NewRequest(http.MethodPost, "/api/v1/dumper/switch", strings.NewReader(`{"db_cloud_token":"secret"}`))
	w = httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusNotImplemented {
		t.Fatalf("got %d", w.Code)
	}
}
