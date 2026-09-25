package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
)

const blockedRoute = "127.0.0.1:1"

// switchCluster is entered only after repeated independent failure checks. Every
// external action has a durable started marker; an uncertain result stops here.
func (s *Server) switchCluster(ctx context.Context, _ *State, proposed Cluster) error {
	if err := s.Store.Ready(ctx); err != nil {
		return err
	}
	dbSize, err := s.Store.MaxDBSize(ctx)
	if err != nil {
		return apiError(http.StatusServiceUnavailable, "ETCD_UNAVAILABLE", "no etcd member status is available")
	}
	if s.Config.EtcdQuotaBytes > 0 && dbSize >= s.Config.EtcdQuotaBytes*9/10 {
		return apiError(http.StatusServiceUnavailable, "ETCD_CAPACITY", "etcd is above the switch capacity threshold")
	}
	opID := uuid.NewString()
	var prepared Operation
	err = s.Store.Update(ctx, func(st *State) error {
		c, ok := st.Clusters[clusterKey(proposed.ID)]
		if !ok || c.TopologyEpoch != proposed.TopologyEpoch || !s.switchReady(st, c) {
			return apiError(http.StatusPreconditionFailed, "SWITCH_NOT_READY", "switch evidence or epoch changed")
		}
		if st.SwitchLocks[clusterKey(c.ID)] != "" {
			return apiError(http.StatusConflict, "SWITCH_LOCKED", "switch lock exists")
		}
		participants := make([]string, 0, 2)
		for _, id := range c.ProxyIDs {
			if s.freshInstance(st, id) && sameBackend(observation(st, id), st.Instances[c.PrimaryID].Endpoint) {
				participants = append(participants, id)
			}
		}
		if len(participants) == 0 {
			return apiError(http.StatusPreconditionFailed, "NO_PROXY", "no verified proxy available")
		}
		sort.Strings(participants)
		policies, err := snapshotSwitchPolicies(st, c.DeploymentID)
		if err != nil {
			return err
		}
		created := time.Now().UTC()
		prepared = Operation{ID: opID, ClusterID: c.ID, Phase: "PREPARED", InputEpoch: c.TopologyEpoch, OldPrimaryID: c.PrimaryID, CandidateID: c.StandbyID, ProxyIDs: participants, PolicySnapshot: policies, EvidenceRev: st.Revision, Steps: make(map[string]OperationStep), CreatedAt: created, PhaseEnteredAt: created}
		st.Operations[opID] = prepared
		c.OperationState = "SWITCHING"
		c.ActiveOperationID = opID
		st.Clusters[clusterKey(c.ID)] = c
		st.SwitchLocks[clusterKey(c.ID)] = opID
		return nil
	})
	if err != nil {
		return err
	}
	profile, err := s.profileFromOperation(ctx, prepared)
	if err != nil {
		return s.failOperation(ctx, prepared, err)
	}
	for _, id := range prepared.ProxyIDs {
		proxy, err := s.operationInstance(ctx, prepared, id)
		if err != nil {
			return s.failOperation(ctx, prepared, err)
		}
		if err = s.runStep(ctx, prepared, "block:"+id, func() error { return s.refreshProxy(ctx, proxy, profile, blockedRoute) }); err != nil {
			return s.failOperation(ctx, prepared, err)
		}
	}
	if err = s.advancePhase(ctx, prepared, "ROUTES_BLOCKED"); err != nil {
		return s.failOperation(ctx, prepared, err)
	}
	candidate, err := s.operationInstance(ctx, prepared, prepared.CandidateID)
	if err != nil {
		return s.failOperation(ctx, prepared, err)
	}
	if err = s.runStep(ctx, prepared, "promote", func() error {
		return s.promote(ctx, candidate, profile, strings.TrimPrefix(prepared.OldPrimaryID, "mysql:"))
	}); err != nil {
		return s.failOperation(ctx, prepared, err)
	}
	if err = s.advancePhase(ctx, prepared, "PROMOTED"); err != nil {
		return s.failOperation(ctx, prepared, err)
	}
	for _, id := range prepared.ProxyIDs {
		proxy, err := s.operationInstance(ctx, prepared, id)
		if err != nil {
			return s.failOperation(ctx, prepared, err)
		}
		if err = s.runStep(ctx, prepared, "route:"+id, func() error { return s.refreshProxy(ctx, proxy, profile, candidate.Endpoint) }); err != nil {
			return s.failOperation(ctx, prepared, err)
		}
	}
	if err = s.advancePhase(ctx, prepared, "ROUTED"); err != nil {
		return s.failOperation(ctx, prepared, err)
	}
	if err = s.commitOperation(ctx, prepared); err != nil {
		if current, readErr := s.Store.Read(ctx); readErr == nil && current.Operations[opID].Phase == "COMMITTED" {
			return s.saveWatermark(ctx)
		}
		return s.failOperation(ctx, prepared, err)
	}
	return s.saveWatermark(ctx)
}

// Capture the bounded set of policies that could have selected this switch.
// The copy belongs to the operation and is unaffected by later policy edits.
func snapshotSwitchPolicies(st *State, deploymentID string) ([]Policy, error) {
	ids := make([]string, 0, len(st.Policies))
	for id := range st.Policies {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	var snapshot []Policy
	size := 0
	for _, id := range ids {
		p := st.Policies[id]
		if p.Scope != "global" && p.Scope != deploymentID {
			continue
		}
		var v PolicyValue
		if json.Unmarshal(p.Value, &v) != nil || v.Status != "enabled" || v.Action != "switch" || (v.TriggerEventName != "dbha_detect_db_failure" && v.TriggerEventName != "dbha_doublecheck_ssh_fail") {
			continue
		}
		size += len(p.Value)
		if len(snapshot) >= 32 || size > 32<<10 {
			return nil, apiError(http.StatusRequestEntityTooLarge, "POLICY_SNAPSHOT_TOO_LARGE", "too many effective policies for one operation")
		}
		p.Value = append(json.RawMessage(nil), p.Value...)
		snapshot = append(snapshot, p)
	}
	return snapshot, nil
}

// Durations measure time spent in a durable phase, including waits and retries.
// They do not represent SQL command or proxy refresh execution time.
func transitionPhase(op *Operation, next string, at time.Time) {
	entered := op.PhaseEnteredAt
	if entered.IsZero() && op.Phase == "PREPARED" {
		entered = op.CreatedAt
	}
	if !entered.IsZero() && at.After(entered) {
		if op.PhaseDurationsMS == nil {
			op.PhaseDurationsMS = make(map[string]int64)
		}
		op.PhaseDurationsMS[op.Phase] += at.Sub(entered).Milliseconds()
	}
	op.Phase = next
	op.PhaseEnteredAt = at
}

func (s *Server) profileFromOperation(ctx context.Context, op Operation) (CredentialProfile, error) {
	st, err := s.Store.Read(ctx)
	if err != nil {
		return CredentialProfile{}, err
	}
	c, ok := st.Clusters[clusterKey(op.ClusterID)]
	if !ok {
		return CredentialProfile{}, apiError(http.StatusPreconditionFailed, "CLUSTER_MISSING", "cluster missing after prepare")
	}
	return s.profile(st, c.DeploymentID), nil
}

func (s *Server) operationInstance(ctx context.Context, op Operation, id string) (Instance, error) {
	st, err := s.Store.Read(ctx)
	if err != nil {
		return Instance{}, err
	}
	i, ok := st.Instances[id]
	if !ok || i.DeploymentID != st.Clusters[clusterKey(op.ClusterID)].DeploymentID {
		return Instance{}, apiError(http.StatusPreconditionFailed, "INSTANCE_MISSING", "prepared instance is missing")
	}
	return i, nil
}

func (s *Server) beforeExternal(ctx context.Context, op Operation) error {
	if err := s.Store.Ready(ctx); err != nil {
		return err
	}
	st, err := s.Store.Read(ctx)
	if err != nil {
		return err
	}
	c := st.Clusters[clusterKey(op.ClusterID)]
	if c.ActiveOperationID != op.ID || c.OperationState != "SWITCHING" || c.TopologyEpoch != op.InputEpoch || st.SwitchLocks[clusterKey(op.ClusterID)] != op.ID {
		return apiError(http.StatusConflict, "OPERATION_SUPERSEDED", "operation ownership or epoch changed")
	}
	return nil
}

func (s *Server) runStep(ctx context.Context, op Operation, name string, action func() error) error {
	if err := s.beforeExternal(ctx, op); err != nil {
		return err
	}
	for _, started := range []bool{false, true} {
		err := s.Store.Update(ctx, func(st *State) error {
			v := st.Operations[op.ID]
			c := st.Clusters[clusterKey(op.ClusterID)]
			if c.ActiveOperationID != op.ID || c.OperationState != "SWITCHING" || st.SwitchLocks[clusterKey(op.ClusterID)] != op.ID {
				return apiError(http.StatusConflict, "OPERATION_SUPERSEDED", "operation changed")
			}
			step := v.Steps[name]
			if step.ActionStarted || step.Verified {
				return apiError(http.StatusConflict, "STEP_UNCERTAIN", "step already started; manual recovery required")
			}
			step.IntentWritten = true
			step.ActionStarted = started
			if v.Steps == nil {
				v.Steps = make(map[string]OperationStep)
			}
			v.Steps[name] = step
			st.Operations[op.ID] = v
			return nil
		})
		if err != nil {
			return err
		}
		if err = s.beforeExternal(ctx, op); err != nil {
			return err
		}
	}
	if err := action(); err != nil {
		return err
	}
	if err := s.Store.Ready(ctx); err != nil {
		return err
	}
	return s.Store.Update(ctx, func(st *State) error {
		v := st.Operations[op.ID]
		c := st.Clusters[clusterKey(op.ClusterID)]
		if c.ActiveOperationID != op.ID || c.OperationState != "SWITCHING" || st.SwitchLocks[clusterKey(op.ClusterID)] != op.ID {
			return apiError(http.StatusConflict, "OPERATION_SUPERSEDED", "operation changed after external action")
		}
		step := v.Steps[name]
		if !step.ActionStarted {
			return apiError(http.StatusConflict, "STEP_UNCERTAIN", "missing started marker")
		}
		step.Verified = true
		v.Steps[name] = step
		st.Operations[op.ID] = v
		return nil
	})
}

func (s *Server) advancePhase(ctx context.Context, op Operation, phase string) error {
	return s.Store.Update(ctx, func(st *State) error {
		v := st.Operations[op.ID]
		c := st.Clusters[clusterKey(op.ClusterID)]
		if c.ActiveOperationID != op.ID || c.OperationState != "SWITCHING" || st.SwitchLocks[clusterKey(op.ClusterID)] != op.ID {
			return apiError(http.StatusConflict, "OPERATION_SUPERSEDED", "operation changed")
		}
		expected := map[string]string{"ROUTES_BLOCKED": "PREPARED", "PROMOTED": "ROUTES_BLOCKED", "ROUTED": "PROMOTED"}
		if v.Phase != expected[phase] {
			return apiError(http.StatusConflict, "OPERATION_PHASE", "operation phase changed")
		}
		if phase == "ROUTES_BLOCKED" {
			for _, id := range op.ProxyIDs {
				if !v.Steps["block:"+id].Verified {
					return apiError(http.StatusPreconditionFailed, "STEP_UNVERIFIED", "proxy block not verified")
				}
			}
		}
		if phase == "PROMOTED" && !v.Steps["promote"].Verified {
			return apiError(http.StatusPreconditionFailed, "STEP_UNVERIFIED", "promotion not verified")
		}
		if phase == "ROUTED" {
			for _, id := range op.ProxyIDs {
				if !v.Steps["route:"+id].Verified {
					return apiError(http.StatusPreconditionFailed, "STEP_UNVERIFIED", "proxy route not verified")
				}
			}
		}
		transitionPhase(&v, phase, time.Now().UTC())
		st.Operations[op.ID] = v
		return nil
	})
}

func (s *Server) commitOperation(ctx context.Context, op Operation) error {
	if err := s.beforeExternal(ctx, op); err != nil {
		return err
	}
	return s.Store.Update(ctx, func(st *State) error {
		v, ok := st.Operations[op.ID]
		c := st.Clusters[clusterKey(op.ClusterID)]
		if !ok || v.Phase != "ROUTED" || c.ActiveOperationID != op.ID || c.TopologyEpoch != op.InputEpoch || st.SwitchLocks[clusterKey(op.ClusterID)] != op.ID {
			return apiError(http.StatusConflict, "OPERATION_SUPERSEDED", "role commit precondition failed")
		}
		for _, id := range op.ProxyIDs {
			if !v.Steps["route:"+id].Verified {
				return apiError(http.StatusPreconditionFailed, "STEP_UNVERIFIED", "proxy not routed")
			}
		}
		c.PrimaryID = op.CandidateID
		c.StandbyID = ""
		c.TopologyEpoch++
		c.OperationState = "IDLE"
		c.ActiveOperationID = ""
		c.SwitchingEnabled = false
		c.HealthState = "DEGRADED"
		c.LastHealthyAt = time.Time{}
		c.ReasonCodes = []string{"NO_VERIFIED_STANDBY"}
		st.Clusters[clusterKey(c.ID)] = c
		old := st.Instances[op.OldPrimaryID]
		old.Admission = "RETURNED_UNVERIFIED"
		st.Instances[old.ID] = old
		transitionPhase(&v, "COMMITTED", time.Now().UTC())
		v.Result = "COMMITTED"
		v.CompletedAt = v.PhaseEnteredAt
		st.Operations[op.ID] = v
		delete(st.SwitchLocks, clusterKey(c.ID))
		return nil
	})
}

func (s *Server) failOperation(ctx context.Context, op Operation, cause error) error {
	markCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	markErr := s.Store.Update(markCtx, func(st *State) error {
		c := st.Clusters[clusterKey(op.ClusterID)]
		if c.ActiveOperationID != op.ID {
			return nil
		}
		c.OperationState = "RECOVERY_REQUIRED"
		c.SwitchingEnabled = false
		c.ReasonCodes = []string{"OPERATION_UNCERTAIN"}
		st.Clusters[clusterKey(c.ID)] = c
		v := st.Operations[op.ID]
		v.Result = "RECOVERY_REQUIRED"
		st.Operations[op.ID] = v
		return nil
	})
	if markErr != nil {
		return errors.Join(cause, markErr)
	}
	return cause
}

func (s *Server) cleanOperations(ctx context.Context) error {
	return s.Store.Update(ctx, func(st *State) error {
		var terminal []Operation
		for _, op := range st.Operations {
			if !op.CompletedAt.IsZero() {
				terminal = append(terminal, op)
			}
		}
		sort.Slice(terminal, func(i, j int) bool { return terminal[i].CompletedAt.Before(terminal[j].CompletedAt) })
		cutoff := time.Now().Add(-7 * 24 * time.Hour)
		for i, op := range terminal {
			if op.CompletedAt.Before(cutoff) || i < len(terminal)-1000 {
				delete(st.Operations, op.ID)
			}
		}
		return nil
	})
}

// ResolveOperation uses direct read-only evidence. It never sends a second
// promotion or proxy refresh after an uncertain external result.
func (s *Server) ResolveOperation(ctx context.Context, id, resolution string, expectedEpoch uint64) error {
	if resolution != "complete" && resolution != "abort" {
		return apiError(http.StatusBadRequest, "INVALID_RESOLUTION", "resolution must be complete or abort")
	}
	if err := s.Store.Ready(ctx); err != nil {
		return err
	}
	st, err := s.Store.Read(ctx)
	if err != nil {
		return err
	}
	op, ok := st.Operations[id]
	if !ok {
		return apiError(http.StatusNotFound, "OPERATION_NOT_FOUND", "operation was not found")
	}
	if op.CompletedAt.IsZero() == false {
		if (resolution == "complete" && op.Phase == "COMMITTED") || (resolution == "abort" && op.Phase == "ABORTED") {
			return nil
		}
		return apiError(http.StatusConflict, "OPERATION_TERMINAL", "operation already resolved")
	}
	c := st.Clusters[clusterKey(op.ClusterID)]
	if !c.Maintenance {
		return apiError(http.StatusPreconditionFailed, "MAINTENANCE_REQUIRED", "enable maintenance before resolving an uncertain operation")
	}
	if c.ActiveOperationID != id || c.OperationState != "RECOVERY_REQUIRED" || c.TopologyEpoch != expectedEpoch || expectedEpoch != op.InputEpoch {
		return apiError(http.StatusConflict, "OPERATION_SUPERSEDED", "recovery precondition changed")
	}
	p := s.profile(st, c.DeploymentID)
	primary := st.Instances[op.OldPrimaryID]
	candidate := st.Instances[op.CandidateID]
	if resolution == "complete" {
		observed, e := s.verify(ctx, candidate, p)
		if e != nil || observed.ServerUUID != strings.TrimPrefix(candidate.ID, "mysql:") || len(observed.Channels) != 0 || observed.ReadOnly || observed.SuperReadOnly {
			return apiError(http.StatusPreconditionFailed, "RECOVERY_EVIDENCE", "candidate promotion is not verified")
		}
		old, e := s.verify(ctx, primary, p)
		if e == nil {
			if !old.ReadOnly && !old.SuperReadOnly {
				return apiError(http.StatusPreconditionFailed, "RECOVERY_EVIDENCE", "old primary remains writable")
			}
		} else if networkFailure(e) {
			down, sshErr := s.sshHealth(ctx, primary, p)
			if !down || sshErr != nil {
				return apiError(http.StatusPreconditionFailed, "RECOVERY_EVIDENCE", "old primary is not independently confirmed down")
			}
		} else {
			return apiError(http.StatusPreconditionFailed, "RECOVERY_EVIDENCE", "old primary state is unknown")
		}
		for _, proxyID := range op.ProxyIDs {
			actual, e := s.verify(ctx, st.Instances[proxyID], p)
			if e != nil || !sameBackend(actual, candidate.Endpoint) {
				return apiError(http.StatusPreconditionFailed, "RECOVERY_EVIDENCE", "proxy route is not verified")
			}
		}
		return s.resolveCommit(ctx, op)
	}
	old, e := s.verify(ctx, primary, p)
	if e != nil || old.ServerUUID != strings.TrimPrefix(primary.ID, "mysql:") || old.ReadOnly || old.SuperReadOnly || len(old.Channels) != 0 {
		return apiError(http.StatusPreconditionFailed, "RECOVERY_EVIDENCE", "old primary is not verified writable")
	}
	replica, e := s.verify(ctx, candidate, p)
	if e != nil || len(replica.Channels) != 1 || replica.Channels[0].SourceUUID != old.ServerUUID || !replica.ReadOnly {
		return apiError(http.StatusPreconditionFailed, "RECOVERY_EVIDENCE", "candidate is not verified as replica")
	}
	for _, proxyID := range op.ProxyIDs {
		actual, e := s.verify(ctx, st.Instances[proxyID], p)
		if e != nil || !sameBackend(actual, primary.Endpoint) {
			return apiError(http.StatusPreconditionFailed, "RECOVERY_EVIDENCE", "proxy route is not verified old primary")
		}
	}
	return s.Store.Update(ctx, func(current *State) error {
		v := current.Operations[id]
		cluster := current.Clusters[clusterKey(op.ClusterID)]
		if !cluster.Maintenance || cluster.ActiveOperationID != id || cluster.OperationState != "RECOVERY_REQUIRED" || cluster.TopologyEpoch != expectedEpoch {
			return apiError(http.StatusConflict, "OPERATION_SUPERSEDED", "recovery changed")
		}
		cluster.ActiveOperationID = ""
		cluster.OperationState = "IDLE"
		cluster.SwitchingEnabled = false
		cluster.RecoveryGate = "STARTUP_VALIDATION"
		current.Clusters[clusterKey(cluster.ID)] = cluster
		transitionPhase(&v, "ABORTED", time.Now().UTC())
		v.Result = "ABORTED"
		v.CompletedAt = v.PhaseEnteredAt
		current.Operations[id] = v
		delete(current.SwitchLocks, clusterKey(cluster.ID))
		return nil
	})
}

func (s *Server) resolveCommit(ctx context.Context, op Operation) error {
	if err := s.Store.Update(ctx, func(st *State) error {
		c := st.Clusters[clusterKey(op.ClusterID)]
		v := st.Operations[op.ID]
		if !c.Maintenance || c.ActiveOperationID != op.ID || c.OperationState != "RECOVERY_REQUIRED" || c.TopologyEpoch != op.InputEpoch {
			return apiError(http.StatusConflict, "OPERATION_SUPERSEDED", "recovery changed")
		}
		c.PrimaryID = op.CandidateID
		c.StandbyID = ""
		c.TopologyEpoch++
		c.ActiveOperationID = ""
		c.OperationState = "IDLE"
		c.SwitchingEnabled = false
		c.HealthState = "DEGRADED"
		c.LastHealthyAt = time.Time{}
		c.RecoveryGate = "STARTUP_VALIDATION"
		c.ReasonCodes = []string{"NO_VERIFIED_STANDBY"}
		st.Clusters[clusterKey(c.ID)] = c
		old := st.Instances[op.OldPrimaryID]
		old.Admission = "RETURNED_UNVERIFIED"
		st.Instances[old.ID] = old
		transitionPhase(&v, "COMMITTED", time.Now().UTC())
		v.Result = "COMMITTED"
		v.CompletedAt = v.PhaseEnteredAt
		st.Operations[op.ID] = v
		delete(st.SwitchLocks, clusterKey(c.ID))
		return nil
	}); err != nil {
		return err
	}
	return s.saveWatermark(ctx)
}
