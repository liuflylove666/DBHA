package server

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"testing"
	"time"

	"dbm-services/common/dbha-v2/pkg/discovery"
	"github.com/google/uuid"
	clientv3 "go.etcd.io/etcd/client/v3"
)

func integrationStore(t *testing.T) (*Store, context.Context) {
	t.Helper()
	endpoint := os.Getenv("DBHA_TEST_ETCD")
	if endpoint == "" {
		t.Skip("DBHA_TEST_ETCD is not set")
	}
	client, err := clientv3.New(clientv3.Config{Endpoints: []string{endpoint}, DialTimeout: 3 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	store, err := NewStore(ctx, client, uuid.NewString())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = store.Close()
		cleanup, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_, _ = client.Delete(cleanup, store.prefix, clientv3.WithPrefix())
		_ = client.Close()
	})
	return store, ctx
}

func expectCode(t *testing.T, err error, code string) {
	t.Helper()
	var api *Error
	if !errors.As(err, &api) || api.Code != code {
		t.Fatalf("wanted %s, got %v", code, err)
	}
}

func TestEtcdStatusSkipsUnavailableEndpoint(t *testing.T) {
	s, ctx := integrationStore(t)
	original := s.client.Endpoints()
	s.client.SetEndpoints("http://127.0.0.1:1", original[0])
	t.Cleanup(func() { s.client.SetEndpoints(original...) })
	if size, err := s.MaxDBSize(ctx); err != nil || size <= 0 {
		t.Fatalf("reachable etcd member was not used: size=%d err=%v", size, err)
	}
}

func TestEtcdIdentityAndReportCAS(t *testing.T) {
	s, ctx := integrationStore(t)
	d, err := s.CreateDeployment(ctx, DeploymentRequest{IdempotencyKey: "install-1"})
	if err != nil {
		t.Fatal(err)
	}
	again, err := s.CreateDeployment(ctx, DeploymentRequest{IdempotencyKey: "install-1"})
	if err != nil || again.ID != d.ID || again.ClusterID != d.ClusterID {
		t.Fatalf("idempotent deployment: %+v %v", again, err)
	}
	agentID := uuid.NewString()
	_, token, err := s.IssueAgent(ctx, d.ID, agentID, "proxy")
	if err != nil {
		t.Fatal(err)
	}
	state, err := s.Read(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if state.Credentials["agent:"+agentID].TokenHash == token {
		t.Fatal("plaintext token persisted")
	}
	boot := uuid.NewString()
	reg, created, err := s.Register(ctx, token, discovery.RegisterRequest{SchemaVersion: 1, AgentID: agentID, BootID: boot, BootGeneration: 1})
	if err != nil || !created {
		t.Fatalf("register: %+v %t %v", reg, created, err)
	}
	regAgain, created, err := s.Register(ctx, token, discovery.RegisterRequest{SchemaVersion: 1, AgentID: agentID, BootID: boot, BootGeneration: 1})
	if err != nil || created || regAgain != reg {
		t.Fatalf("retry register: %+v %t %v", regAgain, created, err)
	}
	_, _, err = s.Register(ctx, token, discovery.RegisterRequest{SchemaVersion: 1, AgentID: agentID, BootID: uuid.NewString(), BootGeneration: 2})
	expectCode(t, err, "SESSION_ACTIVE")
	proxyID := uuid.NewString()
	payload, _ := json.Marshal(discovery.Observation{Kind: "proxy", ProxyUUID: proxyID, AdvertiseHost: "127.0.0.1", DataPort: 3306, AdminPort: 3307, ProcessState: "STARTING", BackendQueryState: "NOT_READY", CollectionState: "ERROR"})
	env := discovery.Envelope{SchemaVersion: 1, AgentID: agentID, SessionID: reg.SessionID, SessionEpoch: reg.SessionEpoch, Stream: "topology", Sequence: 1, SampledAt: time.Now().UTC(), Payload: payload}
	ack, err := s.Ingest(ctx, token, env)
	if err != nil || ack.InstanceID != "proxy:"+proxyID {
		t.Fatalf("STARTING proxy report: %+v %v", ack, err)
	}
	state, err = s.Read(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if state.Instances[ack.InstanceID].Admission != "CANDIDATE" {
		t.Fatal("proxy STARTING not registered")
	}
	dupe, err := s.Ingest(ctx, token, env)
	if err != nil || !dupe.Duplicate || dupe.StoredRevision != ack.StoredRevision {
		t.Fatalf("duplicate: %+v %v", dupe, err)
	}
	state2, err := s.Read(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if state2.Revision != state.Revision {
		t.Fatalf("duplicate changed etcd revision %d -> %d", state.Revision, state2.Revision)
	}
	env.Payload = json.RawMessage(`{"kind":"proxy","proxy_uuid":"different"}`)
	_, err = s.Ingest(ctx, token, env)
	expectCode(t, err, "SEQUENCE_CONFLICT")
	if err := s.CloseSession(ctx, token, agentID, reg.SessionID); err != nil {
		t.Fatal(err)
	}
	newReg, created, err := s.Register(ctx, token, discovery.RegisterRequest{SchemaVersion: 1, AgentID: agentID, BootID: uuid.NewString(), BootGeneration: 2})
	if err != nil || !created || newReg.SessionEpoch <= reg.SessionEpoch {
		t.Fatalf("takeover: %+v %t %v", newReg, created, err)
	}
	_, err = s.Ingest(ctx, token, env)
	expectCode(t, err, "SESSION_EXPIRED")
}

func TestEtcdOwnerExclusion(t *testing.T) {
	s, ctx := integrationStore(t)
	_, err := NewStore(ctx, s.client, s.prefix[len("/dbha/v1/"):len(s.prefix)-1])
	expectCode(t, err, "OWNER_EXISTS")
	if err := s.Ready(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestEtcdOwnerAcquisitionWaitsForPreviousLeaseRelease(t *testing.T) {
	first, ctx := integrationStore(t)
	released := make(chan struct{})
	go func() {
		time.Sleep(100 * time.Millisecond)
		_ = first.Close()
		close(released)
	}()
	second, err := acquireStore(ctx, first.client, first.prefix[len("/dbha/v1/"):len(first.prefix)-1], 3*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	<-released
	if err := second.Ready(ctx); err != nil {
		t.Fatal(err)
	}
	if err := second.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestEtcdReportIdentityDigestAndFailedSample(t *testing.T) {
	s, ctx := integrationStore(t)
	d, err := s.CreateDeployment(ctx, DeploymentRequest{IdempotencyKey: "sample-test"})
	if err != nil {
		t.Fatal(err)
	}
	aID := uuid.NewString()
	_, token, err := s.IssueAgent(ctx, d.ID, aID, "proxy")
	if err != nil {
		t.Fatal(err)
	}
	reg, _, err := s.Register(ctx, token, discovery.RegisterRequest{SchemaVersion: 1, AgentID: aID, BootID: uuid.NewString(), BootGeneration: 1})
	if err != nil {
		t.Fatal(err)
	}
	pID := uuid.NewString()
	payload, _ := json.Marshal(discovery.Observation{Kind: "proxy", ProxyUUID: pID, AdvertiseHost: "127.0.0.1", DataPort: 3306, AdminPort: 3307, ProcessState: "RUNNING", CollectionState: "OK", BackendQueryState: "OK"})
	env := discovery.Envelope{SchemaVersion: 1, AgentID: aID, SessionID: reg.SessionID, SessionEpoch: reg.SessionEpoch, Stream: "topology", Sequence: 1, SampledAt: time.Now().UTC(), Payload: payload}
	ack, err := s.Ingest(ctx, token, env)
	if err != nil {
		t.Fatal(err)
	}
	changed := env
	changed.SampledAt = changed.SampledAt.Add(time.Second)
	_, err = s.Ingest(ctx, token, changed)
	expectCode(t, err, "SEQUENCE_CONFLICT")
	st, err := s.Read(ctx)
	if err != nil {
		t.Fatal(err)
	}
	lastValid := st.Agents[aID].LastValidReportAt
	sample := discovery.Envelope{SchemaVersion: 1, AgentID: aID, SessionID: reg.SessionID, SessionEpoch: reg.SessionEpoch, InstanceID: ack.InstanceID, Stream: "default", Sequence: 1, SampledAt: time.Now().UTC(), Payload: json.RawMessage(`{"collection_state":"ERROR","error_code":"QUERY_TIMEOUT"}`)}
	if _, err = s.Ingest(ctx, token, sample); err != nil {
		t.Fatal(err)
	}
	st, err = s.Read(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if st.Samples[ack.InstanceID+"/default"].Valid || !st.Agents[aID].LastValidReportAt.Equal(lastValid) || st.Agents[aID].LastErrorCode != "QUERY_TIMEOUT" {
		t.Fatal("failed collection refreshed valid report freshness")
	}
	changedPort := env
	changedPort.Sequence = 2
	changedPort.SampledAt = time.Now().UTC()
	changedPort.Payload, _ = json.Marshal(discovery.Observation{Kind: "proxy", ProxyUUID: pID, AdvertiseHost: "127.0.0.1", DataPort: 3306, AdminPort: 3310, ProcessState: "RUNNING", CollectionState: "OK", BackendQueryState: "OK"})
	if _, err = s.Ingest(ctx, token, changedPort); err != nil {
		t.Fatal(err)
	}
	st, err = s.Read(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if st.Instances[ack.InstanceID].AdminPort != 3307 || st.Instances[ack.InstanceID].Admission != "QUARANTINED" {
		t.Fatal("unverified proxy admin port replaced actionable address")
	}
	replacementUUID := uuid.NewString()
	replacement := env
	replacement.Sequence = 3
	replacement.SampledAt = time.Now().UTC()
	replacement.Payload, _ = json.Marshal(discovery.Observation{Kind: "proxy", ProxyUUID: replacementUUID, AdvertiseHost: "127.0.0.1", DataPort: 3308, AdminPort: 3309, ProcessState: "RUNNING", CollectionState: "OK", BackendQueryState: "OK"})
	if _, err = s.Ingest(ctx, token, replacement); err != nil {
		t.Fatal(err)
	}
	st, err = s.Read(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if st.Instances["proxy:"+replacementUUID].Admission != "QUARANTINED" || st.Clusters[clusterKey(d.ClusterID)].TopologyState != "CONFLICT" {
		t.Fatal("same agent new UUID became an automatic member")
	}
}

func TestEtcdCrossDeploymentClaimCannotMutateOwner(t *testing.T) {
	s, ctx := integrationStore(t)
	first, err := s.CreateDeployment(ctx, DeploymentRequest{IdempotencyKey: "first"})
	if err != nil {
		t.Fatal(err)
	}
	second, err := s.CreateDeployment(ctx, DeploymentRequest{IdempotencyKey: "second"})
	if err != nil {
		t.Fatal(err)
	}
	proxyUUID := uuid.NewString()
	report := func(d Deployment, host string) (string, error) {
		aID := uuid.NewString()
		_, token, err := s.IssueAgent(ctx, d.ID, aID, "proxy")
		if err != nil {
			return "", err
		}
		reg, _, err := s.Register(ctx, token, discovery.RegisterRequest{SchemaVersion: 1, AgentID: aID, BootID: uuid.NewString(), BootGeneration: 1})
		if err != nil {
			return "", err
		}
		payload, _ := json.Marshal(discovery.Observation{Kind: "proxy", ProxyUUID: proxyUUID, AdvertiseHost: host, DataPort: 3306, AdminPort: 3307, ProcessState: "STARTING", CollectionState: "OK"})
		_, err = s.Ingest(ctx, token, discovery.Envelope{SchemaVersion: 1, AgentID: aID, SessionID: reg.SessionID, SessionEpoch: reg.SessionEpoch, Stream: "topology", Sequence: 1, SampledAt: time.Now().UTC(), Payload: payload})
		return aID, err
	}
	owner, err := report(first, "127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	_, err = report(second, "127.0.0.2")
	expectCode(t, err, "CROSS_DEPLOYMENT_IDENTITY")
	st, err := s.Read(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if st.Instances["proxy:"+proxyUUID].AgentID != owner || st.Instances["proxy:"+proxyUUID].Admission != "CANDIDATE" || st.Clusters[clusterKey(first.ClusterID)].TopologyState != "DISCOVERING" || st.Clusters[clusterKey(second.ClusterID)].TopologyState != "CONFLICT" {
		t.Fatal("cross-deployment claim affected authoritative owner")
	}
}

func TestEtcdImportedClusterIDNeverReused(t *testing.T) {
	s, ctx := integrationStore(t)
	err := s.Update(ctx, func(st *State) error {
		st.ClusterIDCounter = 100
		st.Clusters["101"] = Cluster{ID: 101, TopologyState: "READY"}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	d, err := s.CreateDeployment(ctx, DeploymentRequest{IdempotencyKey: "after-import"})
	if err != nil {
		t.Fatal(err)
	}
	if d.ClusterID != 102 {
		t.Fatalf("imported cluster id reused: %d", d.ClusterID)
	}
}
