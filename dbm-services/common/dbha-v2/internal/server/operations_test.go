package server

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestOperationPhaseTimingAndPolicySnapshot(t *testing.T) {
	base := time.Now().Add(-4 * time.Second).UTC()
	op := Operation{Phase: "PREPARED", CreatedAt: base, PhaseEnteredAt: base}
	transitionPhase(&op, "ROUTES_BLOCKED", base.Add(time.Second))
	transitionPhase(&op, "PROMOTED", base.Add(2500*time.Millisecond))
	transitionPhase(&op, "ROUTED", base.Add(3*time.Second))
	transitionPhase(&op, "COMMITTED", base.Add(4*time.Second))
	for phase, want := range map[string]int64{"PREPARED": 1000, "ROUTES_BLOCKED": 1500, "PROMOTED": 500, "ROUTED": 1000} {
		if got := op.PhaseDurationsMS[phase]; got != want {
			t.Errorf("%s residence = %dms, want %dms", phase, got, want)
		}
	}
	st := emptyState()
	enabled, _ := json.Marshal(defaultPolicy())
	disabled := defaultPolicy()
	disabled.Status = "disabled"
	disabledBytes, _ := json.Marshal(disabled)
	st.Policies["global"] = Policy{ID: "global", Scope: "global", Version: 2, Value: enabled}
	st.Policies["deployment"] = Policy{ID: "deployment", Scope: "mine", Version: 3, Value: enabled}
	st.Policies["disabled"] = Policy{ID: "disabled", Scope: "mine", Version: 4, Value: disabledBytes}
	st.Policies["other"] = Policy{ID: "other", Scope: "other", Version: 5, Value: enabled}
	snapshot, err := snapshotSwitchPolicies(st, "mine")
	if err != nil || len(snapshot) != 2 || snapshot[0].ID != "deployment" || snapshot[1].ID != "global" {
		t.Fatalf("effective snapshot: %+v %v", snapshot, err)
	}
	enabled[0] = 'x'
	if snapshot[1].Value[0] != '{' || snapshot[1].Version != 2 {
		t.Fatal("prepared policy snapshot changed with policy edit")
	}
}

func TestEtcdSwitchStepUncertainAndAtomicCommit(t *testing.T) {
	store, ctx := integrationStore(t)
	d, err := store.CreateDeployment(ctx, DeploymentRequest{IdempotencyKey: "switch-test"})
	if err != nil {
		t.Fatal(err)
	}
	op := Operation{ID: uuid.NewString(), ClusterID: d.ClusterID, Phase: "PREPARED", InputEpoch: 1, OldPrimaryID: "mysql:old", CandidateID: "mysql:new", ProxyIDs: []string{"proxy:p1"}, Steps: map[string]OperationStep{}, CreatedAt: time.Now().UTC()}
	err = store.Update(ctx, func(st *State) error {
		c := st.Clusters[clusterKey(d.ClusterID)]
		c.PrimaryID = op.OldPrimaryID
		c.StandbyID = op.CandidateID
		c.TopologyEpoch = 1
		c.TopologyState = "READY"
		c.OperationState = "SWITCHING"
		c.ActiveOperationID = op.ID
		c.SwitchingEnabled = true
		st.Clusters[clusterKey(d.ClusterID)] = c
		st.Instances[op.OldPrimaryID] = Instance{ID: op.OldPrimaryID, Admission: "ACTIVE"}
		st.Instances[op.CandidateID] = Instance{ID: op.CandidateID, Admission: "ACTIVE"}
		st.Operations[op.ID] = op
		st.SwitchLocks[clusterKey(d.ClusterID)] = op.ID
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	s := &Server{Store: store}
	failed := errors.New("unknown external result")
	if err = s.runStep(ctx, op, "block:proxy:p1", func() error { return failed }); !errors.Is(err, failed) {
		t.Fatalf("step: %v", err)
	}
	st, err := store.Read(ctx)
	if err != nil {
		t.Fatal(err)
	}
	step := st.Operations[op.ID].Steps["block:proxy:p1"]
	if !step.IntentWritten || !step.ActionStarted || step.Verified {
		t.Fatalf("uncertain action marker: %+v", step)
	}
	called := false
	err = s.runStep(ctx, op, "block:proxy:p1", func() error { called = true; return nil })
	expectCode(t, err, "STEP_UNCERTAIN")
	if called {
		t.Fatal("uncertain external action replayed")
	}
	err = store.Update(ctx, func(st *State) error {
		v := st.Operations[op.ID]
		v.Phase = "ROUTED"
		v.PhaseEnteredAt = time.Now().Add(-time.Second).UTC()
		v.Steps["route:proxy:p1"] = OperationStep{IntentWritten: true, ActionStarted: true, Verified: true}
		st.Operations[op.ID] = v
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if err = s.commitOperation(ctx, op); err != nil {
		t.Fatal(err)
	}
	st, err = store.Read(ctx)
	if err != nil {
		t.Fatal(err)
	}
	c := st.Clusters[clusterKey(d.ClusterID)]
	if c.PrimaryID != op.CandidateID || c.StandbyID != "" || c.TopologyEpoch != 2 || c.SwitchingEnabled || c.ActiveOperationID != "" || st.Operations[op.ID].Phase != "COMMITTED" || st.Instances[op.OldPrimaryID].Admission != "RETURNED_UNVERIFIED" || st.SwitchLocks[clusterKey(d.ClusterID)] != "" {
		t.Fatalf("role commit not atomic: cluster=%+v operation=%+v", c, st.Operations[op.ID])
	}
	if st.Operations[op.ID].PhaseDurationsMS["ROUTED"] < 900 {
		t.Fatalf("committed operation did not persist routed residence time: %+v", st.Operations[op.ID])
	}
}

func TestEtcdCleanupOnlyTerminalOperations(t *testing.T) {
	store, ctx := integrationStore(t)
	s := &Server{Store: store}
	old := time.Now().Add(-8 * 24 * time.Hour)
	err := store.Update(ctx, func(st *State) error {
		st.Operations["unfinished"] = Operation{ID: "unfinished", Phase: "PROMOTED", CreatedAt: old}
		st.Operations["finished"] = Operation{ID: "finished", Phase: "COMMITTED", CreatedAt: old, CompletedAt: old}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if err = s.cleanOperations(context.Background()); err != nil {
		t.Fatal(err)
	}
	st, err := store.Read(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := st.Operations["unfinished"]; !ok {
		t.Fatal("nonterminal operation deleted")
	}
	if _, ok := st.Operations["finished"]; ok {
		t.Fatal("expired terminal operation retained")
	}
}

func TestEtcdResolveRequiresMaintenance(t *testing.T) {
	store, ctx := integrationStore(t)
	d, err := store.CreateDeployment(ctx, DeploymentRequest{IdempotencyKey: "resolve-maintenance"})
	if err != nil {
		t.Fatal(err)
	}
	op := Operation{ID: uuid.NewString(), ClusterID: d.ClusterID, Phase: "PROMOTED", InputEpoch: 1, OldPrimaryID: "mysql:old", CandidateID: "mysql:new", CreatedAt: time.Now().UTC()}
	err = store.Update(ctx, func(st *State) error {
		c := st.Clusters[clusterKey(d.ClusterID)]
		c.TopologyEpoch = 1
		c.OperationState = "RECOVERY_REQUIRED"
		c.ActiveOperationID = op.ID
		st.Clusters[clusterKey(c.ID)] = c
		st.Operations[op.ID] = op
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	s := &Server{Store: store}
	expectCode(t, s.ResolveOperation(ctx, op.ID, "complete", 1), "MAINTENANCE_REQUIRED")
	st, err := store.Read(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if st.Operations[op.ID].Phase != "PROMOTED" || st.Clusters[clusterKey(d.ClusterID)].OperationState != "RECOVERY_REQUIRED" {
		t.Fatal("resolve altered operation outside maintenance")
	}
}
