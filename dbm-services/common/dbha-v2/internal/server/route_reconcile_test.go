package server

import (
	"encoding/json"
	"testing"
	"time"

	"dbm-services/common/dbha-v2/pkg/discovery"
	"github.com/google/uuid"
)

func TestProxyReconcileDoesNotInterruptControlledSwitch(t *testing.T) {
	s, ctx := integrationStore(t)
	d, err := s.CreateDeployment(ctx, DeploymentRequest{IdempotencyKey: "route-reconcile-test"})
	if err != nil {
		t.Fatal(err)
	}
	a := uuid.NewString()
	_, token, err := s.IssueAgent(ctx, d.ID, a, "proxy")
	if err != nil {
		t.Fatal(err)
	}
	reg, _, err := s.Register(ctx, token, discovery.RegisterRequest{SchemaVersion: 1, AgentID: a, BootID: uuid.NewString(), BootGeneration: 1})
	if err != nil {
		t.Fatal(err)
	}
	p := uuid.NewString()
	seq := uint64(0)
	for _, tc := range []struct {
		operation, gate, route string
		reconcile              bool
	}{
		{"SWITCHING", "NONE", blockedRoute, false},
		{"SWITCHING", "NONE", "127.0.0.2:3306", false},
		{"RECOVERY_REQUIRED", "NONE", blockedRoute, false},
		{"IDLE", "RESTORE_UNVERIFIED", "127.0.0.2:3306", false},
		{"IDLE", "NONE", "127.0.0.2:3306", true},
		{"IDLE", "NONE", "127.0.0.1:3306", false},
	} {
		if err := s.Update(ctx, func(st *State) error {
			c := st.Clusters[clusterKey(d.ClusterID)]
			c.PrimaryID, c.TopologyEpoch, c.OperationState, c.RecoveryGate = "mysql:primary", 2, tc.operation, tc.gate
			st.Clusters[clusterKey(d.ClusterID)] = c
			st.Instances[c.PrimaryID] = Instance{ID: c.PrimaryID, Endpoint: "127.0.0.1:3306"}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		seq++
		payload, _ := json.Marshal(discovery.Observation{Kind: "proxy", ProxyUUID: p, AdvertiseHost: "127.0.0.3", DataPort: 10000, AdminPort: 11000, ProcessState: "RUNNING", CollectionState: "OK", BackendQueryState: "OK", Backends: []discovery.Backend{{Address: tc.route}}})
		ack, err := s.Ingest(ctx, token, discovery.Envelope{SchemaVersion: 1, AgentID: a, SessionID: reg.SessionID, SessionEpoch: reg.SessionEpoch, Stream: "topology", Sequence: seq, SampledAt: time.Now().UTC(), Payload: payload})
		if err != nil {
			t.Fatal(err)
		}
		if ack.RouteReconcileRequired != tc.reconcile {
			t.Fatalf("%+v: reconcile=%v", tc, ack.RouteReconcileRequired)
		}
	}
}
