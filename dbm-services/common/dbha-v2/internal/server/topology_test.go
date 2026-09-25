package server

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"dbm-services/common/dbha-v2/pkg/discovery"
)

func topologyFixture() (*Server, *State, Cluster) {
	s := &Server{started: time.Now().Add(-time.Minute), Config: Config{MaxReplicationDelaySeconds: 30, FailureThreshold: 3}, failures: map[string]int{}, log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	st := emptyState()
	d := Deployment{ID: "deployment", ClusterID: 1}
	st.Deployments[d.ID] = d
	c := Cluster{ID: 1, DeploymentID: d.ID, PrimaryID: "mysql:primary", StandbyID: "mysql:standby", ProxyIDs: []string{"proxy:one", "proxy:two"}, TopologyState: "READY", HealthState: "HEALTHY", OperationState: "IDLE", RecoveryGate: "NONE", SwitchingEnabled: true, TopologyEpoch: 2, LastHealthyAt: time.Now(), ActiveStartPermits: map[string]StartPermit{}}
	zero := int64(0)
	obs := map[string]discovery.Observation{
		c.PrimaryID: {Kind: "mysql", ServerUUID: "primary", AdvertiseHost: "127.0.0.1", Port: 3306, Version: "8.0.41", GTIDMode: "ON", CollectionState: "OK", ReplicationQueryState: "OK"},
		c.StandbyID: {Kind: "mysql", ServerUUID: "standby", AdvertiseHost: "127.0.0.2", Port: 3306, Version: "8.0.41", GTIDMode: "ON", ReadOnly: true, CollectionState: "OK", ReplicationQueryState: "OK", Channels: []discovery.Channel{{SourceUUID: "primary", SourceHost: "127.0.0.1", SourcePort: 3306, IORunning: true, SQLRunning: true, SecondsBehindSource: &zero}}},
		"proxy:one": {Kind: "proxy", ProxyUUID: "one", CollectionState: "OK", BackendQueryState: "OK", Backends: []discovery.Backend{{Address: "127.0.0.1:3306"}}},
		"proxy:two": {Kind: "proxy", ProxyUUID: "two", CollectionState: "OK", BackendQueryState: "OK", Backends: []discovery.Backend{{Address: "127.0.0.1:3306"}}},
	}
	for id, o := range obs {
		b, _ := json.Marshal(o)
		agent := "agent-" + id
		st.Agents[agent] = Agent{ID: agent, CredentialID: agent, SessionID: "session", SessionEpoch: 1}
		st.Credentials[agent] = Credential{ID: agent}
		endpoint := "127.0.0.1:3306"
		if id == c.StandbyID {
			endpoint = "127.0.0.2:3306"
		}
		st.Instances[id] = Instance{ID: id, Kind: o.Kind, AgentID: agent, DeploymentID: d.ID, Endpoint: endpoint, Admission: "ACTIVE"}
		st.Observations[id] = Report{Valid: true, ReceivedAt: time.Now(), Payload: b, Digest: string(b), Envelope: discovery.Envelope{SessionID: "session", SessionEpoch: 1, Sequence: 1, SampledAt: time.Now()}}
	}
	st.Clusters["1"] = c
	return s, st, c
}

func TestCandidateMustBeReadOnlyAndCurrentSession(t *testing.T) {
	s, st, c := topologyFixture()
	if !s.switchReady(st, c) {
		t.Fatal("healthy candidate rejected")
	}
	o := observation(st, c.StandbyID)
	o.ReadOnly = false
	b, _ := json.Marshal(o)
	r := st.Observations[c.StandbyID]
	r.Payload = b
	st.Observations[c.StandbyID] = r
	if s.switchReady(st, c) {
		t.Fatal("writable replica must not be promoted")
	}
	s, st, c = topologyFixture()
	a := st.Agents[st.Instances[c.StandbyID].AgentID]
	a.SessionEpoch++
	st.Agents[a.ID] = a
	if s.switchReady(st, c) {
		t.Fatal("previous session evidence remains eligible")
	}
}

func TestEvidenceCASRejectsSessionTakeoverAndCredentialRevocation(t *testing.T) {
	_, before, c := topologyFixture()
	encoded, err := json.Marshal(before)
	if err != nil {
		t.Fatal(err)
	}
	var after State
	if err := json.Unmarshal(encoded, &after); err != nil {
		t.Fatal(err)
	}
	ids := []string{c.PrimaryID, c.StandbyID}
	if !evidenceUnchanged(before, &after, ids) {
		t.Fatal("identical evidence rejected")
	}
	agentID := before.Instances[c.StandbyID].AgentID
	a := after.Agents[agentID]
	a.SessionEpoch++
	after.Agents[agentID] = a
	if evidenceUnchanged(before, &after, ids) {
		t.Fatal("session takeover must invalidate prepared observation")
	}
	after.Agents[agentID] = before.Agents[agentID]
	cred := after.Credentials[a.CredentialID]
	cred.Revoked = true
	after.Credentials[cred.ID] = cred
	if evidenceUnchanged(before, &after, ids) {
		t.Fatal("revoked credential must invalidate prepared observation")
	}
}

func TestPrimaryMayBeOfflineButPermitsAndStaleCandidatesBlock(t *testing.T) {
	s, st, c := topologyFixture()
	delete(st.Observations, c.PrimaryID)
	if !s.switchReady(st, c) {
		t.Fatal("offline primary must not make real failures unswitchable")
	}
	c.ActiveStartPermits["permit"] = StartPermit{ID: "permit", ExpiresAt: time.Now().Add(-time.Hour)}
	if s.switchReady(st, c) {
		t.Fatal("expired but unverified permit must still block switching")
	}
	delete(c.ActiveStartPermits, "permit")
	r := st.Observations[c.StandbyID]
	r.Envelope.SampledAt = time.Now().Add(-time.Minute)
	st.Observations[c.StandbyID] = r
	if s.switchReady(st, c) {
		t.Fatal("fresh receipt cannot freshen stale sample")
	}
}

func TestPairDistinguishesUnknownSourceAndQueryFailure(t *testing.T) {
	_, st, c := topologyFixture()
	a, b := observation(st, c.PrimaryID), observation(st, c.StandbyID)
	if ok, why := pairReason(a, b, 30); !ok {
		t.Fatal(why)
	}
	b.Channels[0].SourceUUID = ""
	if _, why := pairReason(a, b, 30); why != "WAITING_SOURCE_UUID" {
		t.Fatal(why)
	}
	a.ReplicationQueryState = "ERROR"
	a.Channels = nil
	if _, why := pairReason(a, b, 30); why != "REPLICATION_QUERY_FAILED" {
		t.Fatal(why)
	}
}

func TestThreeRoundsNeedEveryMemberAndElapsedWindow(t *testing.T) {
	_, st, c := topologyFixture()
	c.ConfirmationCount = 0
	c.LastEvidence = ""
	ids := []string{c.PrimaryID, c.StandbyID}
	if newRound(&c, st, ids) {
		t.Fatal("single report confirmed topology")
	}
	r := st.Observations[c.PrimaryID]
	r.Envelope.Sequence++
	st.Observations[c.PrimaryID] = r
	if newRound(&c, st, ids) || c.ConfirmationCount != 1 {
		t.Fatal("one member repeated while peer absent")
	}
	r = st.Observations[c.StandbyID]
	r.Envelope.Sequence++
	st.Observations[c.StandbyID] = r
	newRound(&c, st, ids)
	for _, id := range ids {
		r = st.Observations[id]
		r.Envelope.Sequence++
		st.Observations[id] = r
	}
	if newRound(&c, st, ids) {
		t.Fatal("three rapid reports bypassed elapsed window")
	}
	c.ConfirmationSince = time.Now().Add(-11 * time.Second)
	for _, id := range ids {
		r = st.Observations[id]
		r.Envelope.Sequence++
		st.Observations[id] = r
	}
	if !newRound(&c, st, ids) {
		t.Fatal("stable fresh rounds did not confirm")
	}
}

func TestMigrationWatermarkPreservesGateAtFirstStart(t *testing.T) {
	testPersistedRecoveryGate(t, "MIGRATION_UNVERIFIED")
}

func TestRestartPreservesRestoreGateWithValidWatermarkAndNoMarker(t *testing.T) {
	testPersistedRecoveryGate(t, "RESTORE_UNVERIFIED")
}

func testPersistedRecoveryGate(t *testing.T, gate string) {
	t.Helper()
	store, ctx := integrationStore(t)
	if err := store.Update(ctx, func(st *State) error {
		st.Clusters["1"] = Cluster{ID: 1, TopologyEpoch: 5, RecoveryGate: gate, OperationState: "IDLE"}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if err := FinalizeMigration(ctx, store, dir); err != nil {
		t.Fatal(err)
	}
	s := &Server{Store: store, Config: Config{StateDir: dir}, started: time.Now()}
	before, err := store.Read(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.prepareRecovery(ctx, before); err != nil {
		t.Fatal(err)
	}
	after, err := store.Read(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if after.Clusters["1"].RecoveryGate != gate || after.Clusters["1"].SwitchingEnabled {
		t.Fatalf("gate unexpectedly changed: %+v", after.Clusters["1"])
	}
}

func TestRouteNeverReadsUnverifiedRestoredRole(t *testing.T) {
	s, st, c := topologyFixture()
	c.RecoveryGate = "RESTORE_UNVERIFIED"
	s.verify = func(context.Context, Instance, CredentialProfile) (observationResult, error) {
		t.Fatal("restored role must fail before external route verification")
		return observationResult{}, nil
	}
	expectCode(t, s.routeReady(context.Background(), st, c), "ROUTE_NOT_READY")
}

func persistTopologyFixture(t *testing.T, store *Store, ctx context.Context, fixture *State) {
	t.Helper()
	if err := store.Update(ctx, func(st *State) error {
		st.Deployments, st.Clusters, st.Instances = fixture.Deployments, fixture.Clusters, fixture.Instances
		st.Agents, st.Credentials, st.Observations = fixture.Agents, fixture.Credentials, fixture.Observations
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestExplicitRecoveryAllowsIndependentlyStoppedProxies(t *testing.T) {
	for _, gate := range []string{"RESTORE_UNVERIFIED", "MIGRATION_UNVERIFIED"} {
		t.Run(gate, func(t *testing.T) {
			store, ctx := integrationStore(t)
			s, fixture, c := topologyFixture()
			c.RecoveryGate = gate
			fixture.Clusters["1"] = c
			persistTopologyFixture(t, store, ctx, fixture)
			s.Store = store
			s.Config.StateDir = t.TempDir()
			s.verify = func(_ context.Context, i Instance, _ CredentialProfile) (observationResult, error) {
				if i.Kind == "proxy" {
					return observationResult{}, &net.OpError{Op: "dial", Net: "tcp", Err: errors.New("refused")}
				}
				return observation(fixture, i.ID), nil
			}
			checks := 0
			s.verifyStop = func(context.Context, Instance, CredentialProfile) error { checks++; return nil }
			r := httptest.NewRequest("POST", "/api/v1/recovery/reconcile", strings.NewReader(`{"gate":"`+gate+`","clusters":[{"cluster_id":1,"expected_epoch":2}]}`)).WithContext(ctx)
			if _, err := s.recoverClusters(httptest.NewRecorder(), r); err != nil {
				t.Fatal(err)
			}
			after, err := store.Read(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if checks != 2 || after.Clusters["1"].RecoveryGate != "NONE" || after.Clusters["1"].SwitchingEnabled {
				t.Fatal("recovery did not independently verify stopped proxies and keep switching disabled")
			}
			for _, id := range c.ProxyIDs {
				if after.Instances[id].Admission != "CANDIDATE" {
					t.Fatal("stopped proxy must await normal route validation")
				}
			}
		})
	}
}

func TestPermitCASRejectsConcurrentRestoreGate(t *testing.T) {
	store, ctx := integrationStore(t)
	s, fixture, c := topologyFixture()
	s.Store = store
	agentID := fixture.Instances[c.ProxyIDs[0]].AgentID
	a := fixture.Agents[agentID]
	a.AllowedKind, a.DeploymentID = "proxy", c.DeploymentID
	fixture.Agents[agentID] = a
	cred := fixture.Credentials[a.CredentialID]
	token, err := newToken()
	if err != nil {
		t.Fatal(err)
	}
	cred.TokenHash, cred.AgentID = tokenHash(token), agentID
	cred.DeploymentID, cred.AllowedKind = c.DeploymentID, "proxy"
	fixture.Credentials[cred.ID] = cred
	persistTopologyFixture(t, store, ctx, fixture)
	s.verify = func(_ context.Context, i Instance, _ CredentialProfile) (observationResult, error) {
		err := store.Update(ctx, func(st *State) error {
			v := st.Clusters["1"]
			v.RecoveryGate = "RESTORE_UNVERIFIED"
			st.Clusters["1"] = v
			return nil
		})
		return observation(fixture, i.ID), err
	}
	r := httptest.NewRequest("POST", "/api/v1/proxies/"+c.ProxyIDs[0]+"/start-permits", strings.NewReader(`{"request_id":"fc46fae1-9d6b-4a78-8d40-c26c26d54e1e"}`)).WithContext(ctx)
	r.Header.Set("Authorization", "Bearer "+token)
	r.SetPathValue("id", c.ProxyIDs[0])
	_, err = s.createPermit(httptest.NewRecorder(), r)
	expectCode(t, err, "ROUTE_NOT_READY")
	after, err := store.Read(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(after.Clusters["1"].ActiveStartPermits) != 0 {
		t.Fatal("permit escaped recovery gate")
	}
}

func TestInitialReadyDoesNotSupersedeUnfinishedProxyPermit(t *testing.T) {
	store, ctx := integrationStore(t)
	s, fixture, c := topologyFixture()
	s.Store = store
	c.TopologyState = "DATABASES_CONFIRMED"
	c.ConfirmationCount = 2
	c.ConfirmationSince = time.Now().Add(-20 * time.Second)
	c.ActiveStartPermits["pending"] = StartPermit{ID: "pending", ProxyID: c.ProxyIDs[0], Epoch: c.TopologyEpoch}
	fixture.Clusters["1"] = c
	persistTopologyFixture(t, store, ctx, fixture)
	s.verify = func(_ context.Context, i Instance, _ CredentialProfile) (observationResult, error) {
		return observation(fixture, i.ID), nil
	}
	if err := s.checkCluster(ctx, fixture, c); err != nil {
		t.Fatal(err)
	}
	st, err := store.Read(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if st.Clusters["1"].TopologyState != "DATABASES_CONFIRMED" || st.Clusters["1"].TopologyEpoch != c.TopologyEpoch {
		t.Fatal("unfinished start permit became impossible to complete")
	}
	if err := store.Update(ctx, func(st *State) error {
		v := st.Clusters["1"]
		delete(v.ActiveStartPermits, "pending")
		st.Clusters["1"] = v
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	st, err = store.Read(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.checkCluster(ctx, st, st.Clusters["1"]); err != nil {
		t.Fatal(err)
	}
	st, err = store.Read(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if st.Clusters["1"].TopologyState != "READY" {
		t.Fatal("confirmed proxies should become READY after permit completion")
	}
}
