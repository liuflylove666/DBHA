package server

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

func TestMetricsRegistryIsPerServer(t *testing.T) {
	first := (&Server{}).ensureMetrics()
	second := (&Server{}).ensureMetrics()
	if first.registry == second.registry {
		t.Fatal("servers share a metrics registry")
	}
	if _, err := first.registry.Gather(); err != nil {
		t.Fatal(err)
	}
	if _, err := second.registry.Gather(); err != nil {
		t.Fatal(err)
	}
}

func TestEtcdMetricsSnapshotAndBoundedHTTPLabels(t *testing.T) {
	store, ctx := integrationStore(t)
	agentID, token, _, env := registeredProxy(t, store, ctx)
	ack, err := store.Ingest(ctx, token, env)
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.Read(ctx)
	if err != nil {
		t.Fatal(err)
	}
	cID := st.Deployments[st.Agents[agentID].DeploymentID].ClusterID
	opID := uuid.NewString()
	err = store.Update(ctx, func(state *State) error {
		c := state.Clusters[clusterKey(cID)]
		c.OperationState = "RECOVERY_REQUIRED"
		c.ActiveOperationID = opID
		state.Clusters[clusterKey(cID)] = c
		state.Operations[opID] = Operation{ID: opID, ClusterID: cID, Phase: "PROMOTED", CreatedAt: time.Now().UTC(), PhaseDurationsMS: map[string]int64{"PREPARED": 2000}}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	s := &Server{Store: store, Config: Config{EtcdQuotaBytes: 2 << 30}}
	s.observeHTTP("/api/v1/agents/register", nil)
	s.observeHTTP("/api/v1/agents/register", apiError(409, "BOOT_SUPERSEDED", "stale"))
	s.observeHTTP("/api/v1/clusters/sensitive-123/route", apiError(503, "ROUTE_NOT_READY", "not ready"))
	rpc := &discoveryRPC{server: s}
	if _, err = rpc.PushObservation(context.Background(), nil); status.Code(err) != codes.Unauthenticated {
		t.Fatalf("unexpected grpc error: %v", err)
	}
	newer := env
	newer.Sequence = 2
	newer.SampledAt = time.Now().UTC()
	if _, err = store.Ingest(ctx, token, newer); err != nil {
		t.Fatal(err)
	}
	authed := metadata.NewIncomingContext(ctx, metadata.Pairs("authorization", "Bearer "+token))
	if _, err = rpc.PushObservation(authed, env.ToProto()); status.Code(err) != codes.Aborted {
		t.Fatalf("out-of-order grpc report accepted: %v", err)
	}
	w := httptest.NewRecorder()
	s.serveMetrics(w, httptest.NewRequest("GET", "/metrics", nil))
	if w.Code != 200 {
		t.Fatalf("metrics scrape: %d %s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	for _, want := range []string{
		`dbha_registration_requests_total{code="OK"} 1`,
		`dbha_registration_requests_total{code="BOOT_SUPERSEDED"} 1`,
		`dbha_route_not_ready_total 1`,
		`dbha_report_rejections_total{code="UNAUTHORIZED",stream="topology"} 1`,
		`dbha_report_rejections_total{code="OUT_OF_ORDER",stream="topology"} 1`,
		`dbha_agents{kind="proxy",state="active"} 1`,
		`dbha_instances{admission="CANDIDATE",kind="proxy"} 1`,
		`dbha_recovery_pending_operations 1`,
		`dbha_operation_retained_phase_duration_seconds{phase="PREPARED"} 2`,
		`dbha_metrics_etcd_read_up 1`,
		`dbha_metrics_etcd_status_up 1`,
		`dbha_server_leader 1`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("missing metric %s", want)
		}
	}
	if strings.Contains(body, "sensitive-123") {
		t.Fatal("dynamic request path leaked into metric label")
	}
	w2 := httptest.NewRecorder()
	s.serveMetrics(w2, httptest.NewRequest("GET", "/metrics", nil))
	if !strings.Contains(w2.Body.String(), `dbha_operation_retained_phase_duration_seconds{phase="PREPARED"} 2`) {
		t.Fatal("phase duration counted again on scrape")
	}
	if st.Observations[ack.InstanceID].StoredRevision == 0 {
		t.Fatal("test setup lacked durable report")
	}
}
