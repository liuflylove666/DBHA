package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"dbm-services/common/dbha-v2/pkg/discovery"
	"github.com/google/uuid"
	clientv3 "go.etcd.io/etcd/client/v3"
)

func TestEtcdReportRevisionWithUnrelatedConcurrentWrites(t *testing.T) {
	s, ctx := integrationStore(t)
	agentID, token, _, env := registeredProxy(t, s, ctx)
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			select {
			case <-stop:
				return
			default:
				_, _ = s.client.Put(ctx, "/unrelated/"+uuid.NewString(), "x")
				time.Sleep(time.Millisecond)
			}
		}
	}()
	defer func() { close(stop); <-done }()
	for seq := uint64(1); seq <= 12; seq++ {
		env.Sequence = seq
		env.SampledAt = time.Now().UTC()
		ack, err := s.Ingest(ctx, token, env)
		if err != nil {
			t.Fatal(err)
		}
		key := s.key("observations/" + url.PathEscape(ack.InstanceID))
		kv, err := s.client.Get(ctx, key, clientv3.WithSerializable())
		if err != nil || len(kv.Kvs) != 1 {
			t.Fatalf("report key: %v %+v", err, kv)
		}
		if ack.StoredRevision != kv.Kvs[0].ModRevision {
			t.Fatalf("ACK revision %d != report ModRevision %d", ack.StoredRevision, kv.Kvs[0].ModRevision)
		}
		st, err := s.Read(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if st.Observations[ack.InstanceID].StoredRevision != ack.StoredRevision || st.Agents[agentID].Streams[env.Stream].StoredRevision != ack.StoredRevision {
			t.Fatalf("read did not hydrate report and stream at revision %d", ack.StoredRevision)
		}
		dupe, err := s.Ingest(ctx, token, env)
		if err != nil || !dupe.Duplicate || dupe.StoredRevision != ack.StoredRevision {
			t.Fatalf("duplicate revision: %+v %v", dupe, err)
		}
	}
}

func TestEtcdFailedObservationRevisionSurvivesOtherStream(t *testing.T) {
	s, ctx := integrationStore(t)
	agentID, token, _, env := registeredProxy(t, s, ctx)
	first, err := s.Ingest(ctx, token, env)
	if err != nil {
		t.Fatal(err)
	}
	failed := env
	failed.Sequence = 2
	failed.SampledAt = time.Now().UTC()
	failed.Payload = json.RawMessage(`{"kind":"proxy","collection_state":"ERROR","error_code":"QUERY_FAILED"}`)
	ack, err := s.Ingest(ctx, token, failed)
	if err != nil || ack.StoredRevision <= first.StoredRevision {
		t.Fatalf("failed observation ACK: %+v %v", ack, err)
	}
	sample := env
	sample.Stream = "heartbeat"
	sample.InstanceID = first.InstanceID
	sample.Payload = json.RawMessage(`{"collection_state":"OK"}`)
	sample.SampledAt = time.Now().UTC()
	if _, err := s.Ingest(ctx, token, sample); err != nil {
		t.Fatal(err)
	}
	st, err := s.Read(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if st.Agents[agentID].Streams["topology"].StoredRevision != ack.StoredRevision {
		t.Fatal("another stream changed failed observation's stored revision")
	}
	duplicate, err := s.Ingest(ctx, token, failed)
	if err != nil || !duplicate.Duplicate || duplicate.StoredRevision != ack.StoredRevision {
		t.Fatalf("failed observation duplicate: %+v %v", duplicate, err)
	}
}

func registeredProxy(t *testing.T, s *Store, ctx context.Context) (string, string, discovery.RegisterResponse, discovery.Envelope) {
	t.Helper()
	d, err := s.CreateDeployment(ctx, DeploymentRequest{IdempotencyKey: uuid.NewString()})
	if err != nil {
		t.Fatal(err)
	}
	agentID := uuid.NewString()
	_, token, err := s.IssueAgent(ctx, d.ID, agentID, "proxy")
	if err != nil {
		t.Fatal(err)
	}
	reg, _, err := s.Register(ctx, token, discovery.RegisterRequest{SchemaVersion: 1, AgentID: agentID, BootID: uuid.NewString(), BootGeneration: 1})
	if err != nil {
		t.Fatal(err)
	}
	payload, _ := json.Marshal(discovery.Observation{Kind: "proxy", ProxyUUID: uuid.NewString(), AdvertiseHost: "127.0.0.1", DataPort: 3306, AdminPort: 3307, ProcessState: "STARTING", CollectionState: "OK"})
	env := discovery.Envelope{SchemaVersion: 1, AgentID: agentID, SessionID: reg.SessionID, SessionEpoch: reg.SessionEpoch, Stream: "topology", Sequence: 1, SampledAt: time.Now().UTC(), Payload: payload}
	return agentID, token, reg, env
}

func TestEtcdConcurrentDuplicateReportSingleRevision(t *testing.T) {
	s, ctx := integrationStore(t)
	_, token, _, env := registeredProxy(t, s, ctx)
	const requests = 8
	start := make(chan struct{})
	var wg sync.WaitGroup
	results := make([]discovery.ReportResponse, requests)
	errs := make([]error, requests)
	for i := 0; i < requests; i++ {
		wg.Add(1)
		go func(n int) { defer wg.Done(); <-start; results[n], errs[n] = s.Ingest(ctx, token, env) }(i)
	}
	close(start)
	wg.Wait()
	revision := results[0].StoredRevision
	ordinary := 0
	for i := 0; i < requests; i++ {
		if errs[i] != nil {
			t.Fatalf("report %d: %v", i, errs[i])
		}
		if results[i].StoredRevision != revision {
			t.Fatalf("different durable revision: %+v", results)
		}
		if !results[i].Duplicate {
			ordinary++
		}
	}
	if ordinary != 1 {
		t.Fatalf("expected one stored report, got %d", ordinary)
	}
}

func TestEtcdManagementDecisionRetriesAfterConcurrentReport(t *testing.T) {
	s, ctx := integrationStore(t)
	_, token, _, env := registeredProxy(t, s, ctx)
	first, err := s.Ingest(ctx, token, env)
	if err != nil {
		t.Fatal(err)
	}
	entered := make(chan struct{})
	release := make(chan struct{})
	done := make(chan error, 1)
	var calls atomic.Int32
	go func() {
		done <- s.Update(ctx, func(st *State) error {
			call := calls.Add(1)
			report := st.Observations[first.InstanceID]
			if call == 1 {
				close(entered)
				<-release
			}
			deployment := st.Deployments[st.Agents[env.AgentID].DeploymentID]
			cluster := st.Clusters[clusterKey(deployment.ClusterID)]
			cluster.ReasonCodes = []string{fmt.Sprintf("sequence-%d", report.Envelope.Sequence)}
			st.Clusters[clusterKey(cluster.ID)] = cluster
			return nil
		})
	}()
	<-entered
	env.Sequence = 2
	env.SampledAt = time.Now().UTC()
	if _, err := s.Ingest(ctx, token, env); err != nil {
		t.Fatal(err)
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	st, err := s.Read(ctx)
	if err != nil {
		t.Fatal(err)
	}
	deployment := st.Deployments[st.Agents[env.AgentID].DeploymentID]
	if calls.Load() < 2 || len(st.Clusters[clusterKey(deployment.ClusterID)].ReasonCodes) != 1 || st.Clusters[clusterKey(deployment.ClusterID)].ReasonCodes[0] != "sequence-2" {
		t.Fatal("management transaction committed from observation superseded by concurrent ingest")
	}
}

func TestEtcdRegisterTakeoverAndReportCannotBothWin(t *testing.T) {
	s, ctx := integrationStore(t)
	agentID, token, reg, env := registeredProxy(t, s, ctx)
	if err := s.Update(ctx, func(st *State) error {
		a := st.Agents[agentID]
		a.LastContactAt = time.Now().Add(-31 * time.Second)
		st.Agents[agentID] = a
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	start := make(chan struct{})
	var wg sync.WaitGroup
	var reportErr, registerErr error
	var newReg discovery.RegisterResponse
	wg.Add(2)
	go func() { defer wg.Done(); <-start; _, reportErr = s.Ingest(ctx, token, env) }()
	go func() {
		defer wg.Done()
		<-start
		newReg, _, registerErr = s.Register(ctx, token, discovery.RegisterRequest{SchemaVersion: 1, AgentID: agentID, BootID: uuid.NewString(), BootGeneration: 2})
	}()
	close(start)
	wg.Wait()
	st, err := s.Read(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if reportErr == nil && registerErr == nil {
		t.Fatal("old session report and new boot takeover both committed")
	}
	if reportErr != nil && registerErr != nil {
		t.Fatalf("neither operation committed: report=%v register=%v", reportErr, registerErr)
	}
	if registerErr == nil {
		expectCode(t, reportErr, "SESSION_EXPIRED")
		if st.Agents[agentID].SessionID != newReg.SessionID || st.Agents[agentID].BootGeneration != 2 {
			t.Fatal("takeover state not durable")
		}
	}
	if reportErr == nil {
		expectCode(t, registerErr, "SESSION_ACTIVE")
		if st.Agents[agentID].SessionID != reg.SessionID || st.Agents[agentID].BootGeneration != 1 {
			t.Fatal("successful contact did not protect active session")
		}
	}
}

func TestEtcdOwnerLossRejectsWrites(t *testing.T) {
	s, ctx := integrationStore(t)
	if _, err := s.client.Revoke(ctx, s.lease); err != nil {
		t.Fatal(err)
	}
	expectCode(t, s.Ready(ctx), "OWNER_LOST")
	err := s.Update(ctx, func(st *State) error { st.ClusterIDCounter = 999; return nil })
	expectCode(t, err, "OWNER_LOST")
	st, err := s.Read(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if st.ClusterIDCounter == 999 {
		t.Fatal("write committed after owner loss")
	}
}

func TestEtcdOwnerDeletionSignalsLeadershipLoss(t *testing.T) {
	s, ctx := integrationStore(t)
	if _, err := s.client.Delete(ctx, s.key("coordination/server-owner")); err != nil {
		t.Fatal(err)
	}
	select {
	case <-s.Done():
	case <-time.After(3 * time.Second):
		t.Fatal("owner deletion did not revoke active leadership")
	}
}

func TestEtcdOwnerReplacementSignalsLeadershipLoss(t *testing.T) {
	s, ctx := integrationStore(t)
	if _, err := s.client.Put(ctx, s.key("coordination/server-owner"), `{"owner_id":"other"}`); err != nil {
		t.Fatal(err)
	}
	select {
	case <-s.Done():
	case <-time.After(3 * time.Second):
		t.Fatal("owner replacement did not revoke active leadership")
	}
}
