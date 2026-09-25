package server

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	clientv3 "go.etcd.io/etcd/client/v3"
)

func TestCampaignFollowerTakesOverAfterOwnerRelease(t *testing.T) {
	leader, baseCtx := integrationStore(t)
	if _, err := leader.CreateDeployment(baseCtx, DeploymentRequest{IdempotencyKey: "ha-campaign"}); err != nil {
		t.Fatal(err)
	}
	environmentID := strings.TrimSuffix(strings.TrimPrefix(leader.prefix, "/dbha/v1/"), "/")
	dir := t.TempDir()
	h := &haRuntime{
		client: leader.client,
		config: Config{EnvironmentID: environmentID, StateDir: dir, AdminTokenFile: dir + "/admin-token", NodeID: "standby"},
		log:    slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	ctx, cancel := context.WithCancel(baseCtx)
	done := make(chan error, 1)
	go func() { done <- h.campaign(ctx) }()
	deadline := time.Now().Add(8 * time.Second)
	for !h.candidateSafe(baseCtx) && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if !h.candidateSafe(baseCtx) {
		cancel()
		t.Fatal("standby did not synchronize its watermark")
	}
	if err := leader.Close(); err != nil {
		cancel()
		t.Fatal(err)
	}
	deadline = time.Now().Add(3 * time.Second)
	for h.active.Load() == nil && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	active := h.active.Load()
	if active == nil || active.Store.Ready(baseCtx) != nil {
		cancel()
		t.Fatal("standby did not become active")
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestFollowerSyncMakesCandidateElectable(t *testing.T) {
	leader, ctx := integrationStore(t)
	if _, err := leader.CreateDeployment(ctx, DeploymentRequest{IdempotencyKey: "ha-watermark"}); err != nil {
		t.Fatal(err)
	}
	environmentID := strings.TrimSuffix(strings.TrimPrefix(leader.prefix, "/dbha/v1/"), "/")
	h := &haRuntime{client: leader.client, config: Config{EnvironmentID: environmentID, StateDir: t.TempDir()}}
	if h.candidateSafe(ctx) {
		t.Fatal("candidate without watermark unexpectedly electable")
	}
	h.syncFollowerWatermark(ctx)
	if !h.candidateSafe(ctx) {
		t.Fatal("follower did not persist a safe watermark")
	}
	if err := leader.Close(); err != nil {
		t.Fatal(err)
	}
	record := ownerRecord{NodeID: "node-2", OwnerID: uuid.NewString(), AdvertiseHTTP: "http://node-2:8080", AdvertiseGRPC: "node-2:50052"}
	next, err := newStore(ctx, h.client, environmentID, record)
	if err != nil {
		t.Fatal(err)
	}
	defer next.Close()
	owner, err := h.owner(ctx)
	if err != nil || owner.NodeID != record.NodeID || owner.AdvertiseHTTP != record.AdvertiseHTTP || owner.AdvertiseGRPC != record.AdvertiseGRPC || owner.EtcdClusterID == 0 {
		t.Fatalf("owner record=%+v err=%v", owner, err)
	}
}

func TestExistingStateWithoutOwnerOrWatermarkIsNotElectable(t *testing.T) {
	leader, ctx := integrationStore(t)
	if _, err := leader.CreateDeployment(ctx, DeploymentRequest{IdempotencyKey: "ha-unsafe"}); err != nil {
		t.Fatal(err)
	}
	environmentID := strings.TrimSuffix(strings.TrimPrefix(leader.prefix, "/dbha/v1/"), "/")
	if err := leader.Close(); err != nil {
		t.Fatal(err)
	}
	h := &haRuntime{client: leader.client, config: Config{EnvironmentID: environmentID, StateDir: t.TempDir()}}
	h.syncFollowerWatermark(ctx)
	if h.candidateSafe(ctx) {
		t.Fatal("unowned existing state without independent watermark became electable")
	}
}

func TestFollowerDoesNotTrustOwnerWithoutLease(t *testing.T) {
	leader, ctx := integrationStore(t)
	if _, err := leader.CreateDeployment(ctx, DeploymentRequest{IdempotencyKey: "ha-owner-without-lease"}); err != nil {
		t.Fatal(err)
	}
	environmentID := strings.TrimSuffix(strings.TrimPrefix(leader.prefix, "/dbha/v1/"), "/")
	client := leader.client
	if err := leader.Close(); err != nil {
		t.Fatal(err)
	}
	owner, _ := json.Marshal(ownerRecord{OwnerID: uuid.NewString(), NodeID: "stale-owner"})
	if _, err := client.Put(ctx, leader.key("coordination/server-owner"), string(owner)); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	h := &haRuntime{client: client, config: Config{EnvironmentID: environmentID, StateDir: dir}}
	h.syncFollowerWatermark(ctx)
	if _, err := os.Stat(filepath.Join(dir, "control-watermark.json")); !os.IsNotExist(err) {
		t.Fatalf("lease-less owner produced a trusted watermark: %v", err)
	}
}

func TestFollowerDoesNotTrustRestoredLeaseWithoutHeartbeat(t *testing.T) {
	leader, ctx := integrationStore(t)
	if _, err := leader.CreateDeployment(ctx, DeploymentRequest{IdempotencyKey: "ha-restored-owner"}); err != nil {
		t.Fatal(err)
	}
	environmentID := strings.TrimSuffix(strings.TrimPrefix(leader.prefix, "/dbha/v1/"), "/")
	client := leader.client
	if err := leader.Close(); err != nil {
		t.Fatal(err)
	}
	lease, err := client.Grant(ctx, 60)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = client.Revoke(context.Background(), lease.ID) })
	owner, _ := json.Marshal(ownerRecord{OwnerID: uuid.NewString(), NodeID: "restored-owner", EtcdClusterID: lease.ClusterId})
	if _, err := client.Put(ctx, leader.key("coordination/server-owner"), string(owner), clientv3.WithLease(lease.ID)); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	h := &haRuntime{client: client, config: Config{EnvironmentID: environmentID, StateDir: dir}}
	waitCtx, cancel := context.WithTimeout(ctx, 250*time.Millisecond)
	defer cancel()
	h.syncFollowerWatermark(waitCtx)
	if _, err := os.Stat(filepath.Join(dir, "control-watermark.json")); !os.IsNotExist(err) {
		t.Fatalf("restored lease without live heartbeat produced a trusted watermark: %v", err)
	}
}

func TestFollowerDoesNotTrustOwnerFromDifferentEtcdCluster(t *testing.T) {
	leader, ctx := integrationStore(t)
	if _, err := leader.CreateDeployment(ctx, DeploymentRequest{IdempotencyKey: "ha-other-cluster-owner"}); err != nil {
		t.Fatal(err)
	}
	environmentID := strings.TrimSuffix(strings.TrimPrefix(leader.prefix, "/dbha/v1/"), "/")
	client := leader.client
	clusterID := leader.clusterID
	if err := leader.Close(); err != nil {
		t.Fatal(err)
	}
	lease, err := client.Grant(ctx, 60)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = client.Revoke(context.Background(), lease.ID) })
	owner, _ := json.Marshal(ownerRecord{OwnerID: uuid.NewString(), NodeID: "old-cluster-owner", EtcdClusterID: clusterID ^ 1})
	if _, err := client.Put(ctx, leader.key("coordination/server-owner"), string(owner), clientv3.WithLease(lease.ID)); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	h := &haRuntime{client: client, config: Config{EnvironmentID: environmentID, StateDir: dir}}
	h.syncFollowerWatermark(ctx)
	if _, err := os.Stat(filepath.Join(dir, "control-watermark.json")); !os.IsNotExist(err) {
		t.Fatalf("owner from a different etcd cluster produced a trusted watermark: %v", err)
	}
}

func TestRestoreMarkerMakesRollbackCandidateElectable(t *testing.T) {
	leader, ctx := integrationStore(t)
	if _, err := leader.CreateDeployment(ctx, DeploymentRequest{IdempotencyKey: "ha-controlled-restore"}); err != nil {
		t.Fatal(err)
	}
	environmentID := strings.TrimSuffix(strings.TrimPrefix(leader.prefix, "/dbha/v1/"), "/")
	dir := t.TempDir()
	old := watermark{ClusterID: leader.clusterID ^ 1, Revision: 1 << 60, Epochs: map[string]uint64{"1": 99}}
	b, _ := json.Marshal(old)
	if err := writePrivate(filepath.Join(dir, "control-watermark.json"), b); err != nil {
		t.Fatal(err)
	}
	if err := writePrivate(filepath.Join(dir, "restore-required"), []byte("controlled restore\n")); err != nil {
		t.Fatal(err)
	}
	if err := leader.Close(); err != nil {
		t.Fatal(err)
	}
	h := &haRuntime{
		client: leader.client,
		config: Config{EnvironmentID: environmentID, StateDir: dir, AdminTokenFile: filepath.Join(dir, "admin.token"), NodeID: "restore-node"},
		log:    slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	if !h.candidateSafe(ctx) {
		t.Fatal("explicit restore marker did not permit a recovery-gated owner election")
	}
	runCtx, cancel := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() { done <- h.campaign(runCtx) }()
	deadline := time.Now().Add(3 * time.Second)
	for h.active.Load() == nil && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	active := h.active.Load()
	if active == nil {
		cancel()
		t.Fatal("restore-marked controller did not acquire ownership")
	}
	state, err := active.Store.Read(ctx)
	if err != nil || state.Clusters["1"].RecoveryGate != "RESTORE_UNVERIFIED" || state.Clusters["1"].SwitchingEnabled {
		cancel()
		t.Fatalf("restored state was not fail-closed: state=%+v err=%v", state.Clusters["1"], err)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestFollowerAdoptsWatermarkFromRestoreGatedLeader(t *testing.T) {
	leader, ctx := integrationStore(t)
	if _, err := leader.CreateDeployment(ctx, DeploymentRequest{IdempotencyKey: "ha-restored-watermark"}); err != nil {
		t.Fatal(err)
	}
	if err := leader.Update(ctx, func(st *State) error {
		cluster := st.Clusters["1"]
		cluster.RecoveryGate = "RESTORE_UNVERIFIED"
		cluster.SwitchingEnabled = false
		st.Clusters["1"] = cluster
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	environmentID := strings.TrimSuffix(strings.TrimPrefix(leader.prefix, "/dbha/v1/"), "/")
	dir := t.TempDir()
	old := watermark{ClusterID: leader.clusterID ^ 1, Revision: 1 << 60, Epochs: map[string]uint64{"1": 99}}
	b, _ := json.Marshal(old)
	if err := writePrivate(filepath.Join(dir, "control-watermark.json"), b); err != nil {
		t.Fatal(err)
	}
	if err := writePrivate(filepath.Join(dir, "restore-required"), []byte("controlled restore\n")); err != nil {
		t.Fatal(err)
	}
	h := &haRuntime{client: leader.client, config: Config{EnvironmentID: environmentID, StateDir: dir}}
	h.syncFollowerWatermark(ctx)
	if !h.candidateSafe(ctx) {
		t.Fatal("standby did not adopt the recovery-gated leader watermark")
	}
	if _, err := os.Stat(filepath.Join(dir, "restore-required")); !os.IsNotExist(err) {
		t.Fatalf("standby restore marker was not retired after watermark adoption: %v", err)
	}
	storedBytes, err := os.ReadFile(filepath.Join(dir, "control-watermark.json"))
	var stored watermark
	if err != nil || json.Unmarshal(storedBytes, &stored) != nil || stored.RestoreMarker != "controlled restore" {
		t.Fatalf("consumed restore generation was not retained in watermark: %+v err=%v", stored, err)
	}
}

func TestConsumedRestoreMarkerDoesNotReopenGate(t *testing.T) {
	leader, ctx := integrationStore(t)
	if _, err := leader.CreateDeployment(ctx, DeploymentRequest{IdempotencyKey: "ha-consumed-restore-marker"}); err != nil {
		t.Fatal(err)
	}
	state, err := leader.Read(ctx)
	if err != nil {
		t.Fatal(err)
	}
	environmentID := strings.TrimSuffix(strings.TrimPrefix(leader.prefix, "/dbha/v1/"), "/")
	dir := t.TempDir()
	marker := "restore-generation-already-consumed"
	w := watermark{ClusterID: state.EtcdClusterID, Revision: state.Revision, Epochs: map[string]uint64{"1": state.Clusters["1"].TopologyEpoch}, RestoreMarker: marker}
	b, _ := json.Marshal(w)
	if err := writePrivate(filepath.Join(dir, "control-watermark.json"), b); err != nil {
		t.Fatal(err)
	}
	if err := writePrivate(filepath.Join(dir, "restore-required"), []byte(marker+"\n")); err != nil {
		t.Fatal(err)
	}
	if err := leader.Close(); err != nil {
		t.Fatal(err)
	}
	h := &haRuntime{
		client: leader.client,
		config: Config{EnvironmentID: environmentID, StateDir: dir, AdminTokenFile: filepath.Join(dir, "admin.token"), NodeID: "post-restore-node"},
		log:    slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	runCtx, cancel := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() { done <- h.campaign(runCtx) }()
	deadline := time.Now().Add(3 * time.Second)
	for h.active.Load() == nil && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	active := h.active.Load()
	if active == nil {
		cancel()
		t.Fatal("controller did not acquire ownership with a consumed marker")
	}
	state, err = active.Store.Read(ctx)
	if err != nil || state.Clusters["1"].RecoveryGate == "RESTORE_UNVERIFIED" {
		cancel()
		t.Fatalf("consumed restore marker reopened the restore gate: state=%+v err=%v", state.Clusters["1"], err)
	}
	if _, err := os.Stat(filepath.Join(dir, "restore-required")); !os.IsNotExist(err) {
		cancel()
		t.Fatalf("consumed restore marker was not cleaned up: %v", err)
	}
	storedBytes, err := os.ReadFile(filepath.Join(dir, "control-watermark.json"))
	var stored watermark
	if err != nil || json.Unmarshal(storedBytes, &stored) != nil || stored.RestoreMarker != marker {
		cancel()
		t.Fatalf("consumed restore generation was not preserved: %+v err=%v", stored, err)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestFollowerReturnsNotLeaderWithHint(t *testing.T) {
	leader, ctx := integrationStore(t)
	environmentID := strings.TrimSuffix(strings.TrimPrefix(leader.prefix, "/dbha/v1/"), "/")
	var stored ownerRecord
	if err := json.Unmarshal([]byte(leader.owner), &stored); err != nil {
		t.Fatal(err)
	}
	stored.AdvertiseHTTP = "http://leader:8080"
	if err := leader.Close(); err != nil {
		t.Fatal(err)
	}
	leader, err := newStore(ctx, leader.client, environmentID, stored)
	if err != nil {
		t.Fatal(err)
	}
	defer leader.Close()
	h := &haRuntime{client: leader.client, config: Config{EnvironmentID: environmentID}}
	w := httptest.NewRecorder()
	h.Handler().ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/v1/clusters", nil))
	if w.Code != http.StatusServiceUnavailable || w.Header().Get("Retry-After") != "1" || !strings.Contains(w.Body.String(), `"code":"NOT_LEADER"`) || !strings.Contains(w.Body.String(), stored.AdvertiseHTTP) {
		t.Fatalf("status=%d headers=%v body=%s", w.Code, w.Header(), w.Body.String())
	}
}

func TestActiveRequestIsCanceledWhenLeadershipEnds(t *testing.T) {
	leaderCtx, cancelLeader := context.WithCancel(context.Background())
	s := &Server{leaderCtx: leaderCtx}
	request, done := s.requestWithLeadership(httptest.NewRequest(http.MethodGet, "/api/v1/clusters", nil))
	defer done()
	cancelLeader()
	select {
	case <-request.Context().Done():
	case <-time.After(time.Second):
		t.Fatal("in-flight request survived leadership loss")
	}
}

func TestFollowerMetricsReportNotLeader(t *testing.T) {
	h := &haRuntime{}
	w := httptest.NewRecorder()
	h.Handler().ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "dbha_server_leader 0") {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
}

func TestFollowerStartupDoesNotBootstrapOverExistingState(t *testing.T) {
	leader, ctx := integrationStore(t)
	if err := leader.Update(ctx, func(st *State) error {
		st.ClusterIDCounter = 99
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := leader.client.Delete(ctx, leader.key("schema")); err != nil {
		t.Fatal(err)
	}
	environmentID := strings.TrimSuffix(strings.TrimPrefix(leader.prefix, "/dbha/v1/"), "/")
	h := &haRuntime{client: leader.client, config: Config{EnvironmentID: environmentID}}
	if err := h.init(ctx); err == nil {
		t.Fatal("follower bootstrapped over state with a missing schema")
	}
	value, err := leader.client.Get(ctx, leader.key("counters/cluster-id"))
	if err != nil || len(value.Kvs) != 1 || !strings.Contains(string(value.Kvs[0].Value), `"value":99`) {
		t.Fatalf("cluster counter changed: response=%v err=%v", value, err)
	}
}
