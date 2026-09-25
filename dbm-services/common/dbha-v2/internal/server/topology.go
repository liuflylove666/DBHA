package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"time"

	"dbm-services/common/dbha-v2/pkg/discovery"
)

func (s *Server) fresh(r Report) bool {
	now := time.Now()
	return r.Valid && !r.ReceivedAt.Before(s.started) && now.Sub(r.ReceivedAt) <= 15*time.Second && now.Sub(r.Envelope.SampledAt) <= 15*time.Second && r.Envelope.SampledAt.Sub(now) <= 5*time.Second
}

func (s *Server) freshInstance(st *State, id string) bool {
	i, ok := st.Instances[id]
	if !ok || i.Admission == "RETIRED" || i.Admission == "QUARANTINED" {
		return false
	}
	r := st.Observations[id]
	a, ok := st.Agents[i.AgentID]
	if !ok || a.SessionClosed || a.SessionID != r.Envelope.SessionID || a.SessionEpoch != r.Envelope.SessionEpoch {
		return false
	}
	cred, ok := st.Credentials[a.CredentialID]
	return ok && !cred.Revoked && s.fresh(r)
}

func evidenceUnchanged(before, after *State, ids []string) bool {
	for _, id := range ids {
		if !reflect.DeepEqual(before.Instances[id], after.Instances[id]) || before.Observations[id].Digest != after.Observations[id].Digest || before.Observations[id].Envelope.Sequence != after.Observations[id].Envelope.Sequence || before.Observations[id].Envelope.SessionID != after.Observations[id].Envelope.SessionID {
			return false
		}
		agentID := before.Instances[id].AgentID
		a, b := before.Agents[agentID], after.Agents[agentID]
		if a.SessionID != b.SessionID || a.SessionEpoch != b.SessionEpoch || a.SessionClosed != b.SessionClosed || a.CredentialID != b.CredentialID || !reflect.DeepEqual(before.Credentials[a.CredentialID], after.Credentials[b.CredentialID]) {
			return false
		}
	}
	return true
}

func observation(st *State, id string) discovery.Observation {
	var o discovery.Observation
	_ = json.Unmarshal(st.Observations[id].Payload, &o)
	return o
}

func supportedMySQL(o discovery.Observation) bool {
	parts := strings.Split(o.Version, ".")
	if len(parts) < 3 {
		return false
	}
	patch, _ := strconv.Atoi(strings.Split(parts[2], "-")[0])
	return parts[0] == "8" && parts[1] == "0" && patch >= 22 && !strings.Contains(strings.ToLower(o.Version), "mariadb") && o.GTIDMode == "ON"
}

func pairReason(a, b discovery.Observation, maxDelay int) (bool, string) {
	if !supportedMySQL(a) || !supportedMySQL(b) {
		return false, "UNSUPPORTED_VERSION_OR_GTID"
	}
	if a.ReplicationQueryState != "OK" || b.ReplicationQueryState != "OK" {
		return false, "REPLICATION_QUERY_FAILED"
	}
	if len(a.Channels) > 1 || len(b.Channels) > 1 {
		return false, "MULTI_CHANNEL_UNSUPPORTED"
	}
	if len(a.Channels) != 0 || len(b.Channels) != 1 {
		return false, "WAITING_MYSQL"
	}
	c := b.Channels[0]
	if c.SourceUUID == "" {
		return false, "WAITING_SOURCE_UUID"
	}
	if c.SourceUUID != a.ServerUUID {
		return false, "CROSS_DEPLOYMENT_SOURCE"
	}
	if c.SourceHost != a.AdvertiseHost || c.SourcePort != a.Port {
		return false, "ROLE_CONFLICT"
	}
	if a.ReadOnly || a.SuperReadOnly || !b.ReadOnly || !c.IORunning || !c.SQLRunning || c.SecondsBehindSource == nil || *c.SecondsBehindSource < 0 || *c.SecondsBehindSource > int64(maxDelay) {
		return false, "REPLICATION_NOT_READY"
	}
	return true, ""
}

func newRound(c *Cluster, st *State, ids []string) bool {
	var previous map[string]string
	_ = json.Unmarshal([]byte(c.LastEvidence), &previous)
	current := map[string]string{}
	for _, id := range ids {
		r := st.Observations[id]
		v := fmt.Sprintf("%s/%d", r.Envelope.SessionID, r.Envelope.Sequence)
		if previous[id] == v {
			return false
		}
		current[id] = v
	}
	b, _ := json.Marshal(current)
	c.LastEvidence = string(b)
	if c.ConfirmationCount == 0 {
		c.ConfirmationSince = time.Now().UTC()
	}
	c.ConfirmationCount++
	return c.ConfirmationCount >= 3 && time.Since(c.ConfirmationSince) >= 10*time.Second
}

func (s *Server) reconcile(ctx context.Context) error {
	if !s.decisionMu.TryLock() {
		return nil
	}
	defer s.decisionMu.Unlock()
	st, err := s.Store.Read(ctx)
	if err != nil {
		return err
	}
	keys := make([]string, 0, len(st.Clusters))
	for id := range st.Clusters {
		keys = append(keys, id)
	}
	sort.Strings(keys)
	for _, id := range keys {
		c := st.Clusters[id]
		if c.TopologyState == "RETIRED" || c.OperationState != "IDLE" {
			continue
		}
		if c.PrimaryID == "" {
			err = s.discoverCluster(ctx, st, c)
		} else {
			err = s.checkCluster(ctx, st, c)
		}
		if err != nil {
			s.log.Warn("cluster verification deferred", "cluster_id", c.ID, "error", err.Error())
		}
	}
	return s.cleanOperations(ctx)
}

func (s *Server) discoverCluster(ctx context.Context, st *State, c Cluster) error {
	if c.Maintenance || c.RecoveryGate == "MIGRATION_UNVERIFIED" || c.RecoveryGate == "RESTORE_UNVERIFIED" {
		return nil
	}
	var mysqlIDs, proxyIDs []string
	for id, i := range st.Instances {
		if i.DeploymentID != c.DeploymentID || i.Admission == "RETIRED" || i.Admission == "QUARANTINED" {
			continue
		}
		if i.Kind == "mysql" {
			mysqlIDs = append(mysqlIDs, id)
		} else {
			proxyIDs = append(proxyIDs, id)
		}
	}
	sort.Strings(mysqlIDs)
	sort.Strings(proxyIDs)
	reason := "WAITING_MYSQL"
	state := "DISCOVERING"
	var primary, standby string
	if len(mysqlIDs) == 2 {
		a, b := observation(st, mysqlIDs[0]), observation(st, mysqlIDs[1])
		primary, standby = mysqlIDs[0], mysqlIDs[1]
		if len(a.Channels) == 1 && len(b.Channels) == 0 {
			a, b = b, a
			primary, standby = standby, primary
		}
		ok, why := pairReason(a, b, s.Config.MaxReplicationDelaySeconds)
		reason = why
		if ok {
			directFacts := map[string]discovery.Observation{}
			for _, id := range mysqlIDs {
				if !s.freshInstance(st, id) {
					reason = "PROBE_STALE"
					break
				}
				direct, err := s.verify(ctx, st.Instances[id], s.profile(st, c.DeploymentID))
				if err != nil {
					return err
				}
				observed := observation(st, id)
				directFacts[id] = direct
				if direct.ServerUUID != observed.ServerUUID || direct.ReadOnly != observed.ReadOnly || direct.SuperReadOnly != observed.SuperReadOnly || len(direct.Channels) != len(observed.Channels) {
					reason = "ROLE_CONFLICT"
					break
				}
				if len(direct.Channels) == 1 && direct.Channels[0].SourceUUID != observed.Channels[0].SourceUUID {
					reason = "ROLE_CONFLICT"
					break
				}
			}
			if reason == "" {
				_, reason = pairReason(directFacts[primary], directFacts[standby], s.Config.MaxReplicationDelaySeconds)
			}
		}
	}
	if reason == "MULTI_CHANNEL_UNSUPPORTED" || reason == "UNSUPPORTED_VERSION_OR_GTID" {
		state = "UNSUPPORTED"
	}
	if reason == "ROLE_CONFLICT" || reason == "CROSS_DEPLOYMENT_SOURCE" {
		state = "CONFLICT"
	}
	err := s.Store.Update(ctx, func(current *State) error {
		v := current.Clusters[clusterKey(c.ID)]
		if v.PrimaryID != "" || v.TopologyEpoch != c.TopologyEpoch || v.Maintenance || v.RecoveryGate != c.RecoveryGate {
			return nil
		}
		if !evidenceUnchanged(st, current, mysqlIDs) || v.TopologyState != c.TopologyState {
			return nil
		}
		v.TopologyState = state
		v.ProxyIDs = proxyIDs
		v.ReasonCodes = nil
		if reason != "" {
			v.ReasonCodes = []string{reason}
			v.ConfirmationCount = 0
			v.ConfirmationSince = time.Time{}
			v.LastEvidence = ""
		} else if newRound(&v, st, mysqlIDs) {
			v.PrimaryID = primary
			v.StandbyID = standby
			v.TopologyEpoch = 1
			v.TopologyState = "DATABASES_CONFIRMED"
			v.RecoveryGate = "NONE"
			v.ConfirmationCount = 0
			v.LastEvidence = ""
			v.LastHealthyAt = time.Now().UTC()
			for _, id := range mysqlIDs {
				i := current.Instances[id]
				i.Admission = "ACTIVE"
				current.Instances[id] = i
			}
		}
		current.Clusters[clusterKey(c.ID)] = v
		return nil
	})
	if err != nil {
		return err
	}
	return s.saveWatermark(ctx)
}

func (s *Server) checkCluster(ctx context.Context, st *State, c Cluster) error {
	if c.RecoveryGate == "RESTORE_UNVERIFIED" || c.RecoveryGate == "MIGRATION_UNVERIFIED" {
		return nil
	}
	primary := st.Instances[c.PrimaryID]
	p := s.profile(st, c.DeploymentID)
	actual, err := s.verify(ctx, primary, p)
	if err != nil {
		_ = s.setHealth(ctx, c, "DEGRADED", "PRIMARY_CHECK_FAILED")
		if !networkFailure(err) || !s.switchReady(st, c) {
			s.failures[clusterKey(c.ID)] = 0
			return nil
		}
		down, sshErr := s.sshHealth(ctx, primary, p)
		if !down {
			s.failures[clusterKey(c.ID)] = 0
			return sshErr
		}
		s.failures[clusterKey(c.ID)]++
		threshold := s.failurePolicy(st, c)
		if threshold > 0 && s.failures[clusterKey(c.ID)] >= threshold {
			return s.switchCluster(ctx, st, c)
		}
		return nil
	}
	s.failures[clusterKey(c.ID)] = 0
	if actual.ServerUUID != strings.TrimPrefix(primary.ID, "mysql:") || actual.ReadOnly || actual.SuperReadOnly || len(actual.Channels) != 0 {
		return s.setHealth(ctx, c, "DEGRADED", "ROLE_CONFLICT")
	}
	if !s.freshInstance(st, primary.ID) {
		return s.setHealth(ctx, c, "UNKNOWN", "PROBE_STALE")
	}
	var ids []string
	ids = append(ids, c.PrimaryID)
	standbyHealthy := false
	if standby, ok := st.Instances[c.StandbyID]; ok && standby.Admission == "ACTIVE" && s.freshInstance(st, standby.ID) {
		b, e := s.verify(ctx, standby, p)
		if e == nil {
			standbyHealthy, _ = pairReason(actual, b, s.Config.MaxReplicationDelaySeconds)
		}
		if standbyHealthy {
			ids = append(ids, standby.ID)
		}
	}
	proxyIDs := []string{}
	for id, i := range st.Instances {
		if i.DeploymentID == c.DeploymentID && i.Kind == "proxy" && i.Admission != "RETIRED" && i.Admission != "QUARANTINED" {
			proxyIDs = append(proxyIDs, id)
		}
	}
	if c.TopologyState == "READY" {
		proxyIDs = append([]string(nil), c.ProxyIDs...)
	}
	sort.Strings(proxyIDs)
	proxiesHealthy := len(proxyIDs) == 2
	for _, id := range proxyIDs {
		if !s.freshInstance(st, id) {
			proxiesHealthy = false
			continue
		}
		o, e := s.verify(ctx, st.Instances[id], p)
		if e != nil || !sameBackend(o, primary.Endpoint) {
			proxiesHealthy = false
			continue
		}
		ids = append(ids, id)
	}
	return s.Store.Update(ctx, func(current *State) error {
		v := current.Clusters[clusterKey(c.ID)]
		if v.TopologyEpoch != c.TopologyEpoch || v.OperationState != "IDLE" || v.RecoveryGate != c.RecoveryGate || v.TopologyState != c.TopologyState || !evidenceUnchanged(st, current, ids) {
			return nil
		}
		v.ProxyIDs = proxyIDs
		v.HealthState = "DEGRADED"
		v.ReasonCodes = []string{"WAITING_PROXY_OR_REPLICA"}
		if proxiesHealthy {
			if v.TopologyState == "DATABASES_CONFIRMED" && standbyHealthy && len(v.ActiveStartPermits) == 0 && newRound(&v, st, ids) {
				v.TopologyState = "READY"
				v.TopologyEpoch++
			}
			if v.RecoveryGate == "STARTUP_VALIDATION" {
				v.RecoveryGate = "NONE"
			}
			for _, id := range proxyIDs {
				i := current.Instances[id]
				i.Admission = "ACTIVE"
				current.Instances[id] = i
			}
			if standbyHealthy {
				v.HealthState = "HEALTHY"
				v.LastHealthyAt = time.Now().UTC()
				v.ReasonCodes = nil
			}
		} else {
			v.ConfirmationCount = 0
			v.ConfirmationSince = time.Time{}
			v.LastEvidence = ""
		}
		current.Clusters[clusterKey(c.ID)] = v
		return nil
	})
}

// Policies tune an already independently verified database/host failure. They
// cannot bypass candidate freshness, SSH, maintenance or operation gates.
func (s *Server) failurePolicy(st *State, c Cluster) int {
	ids := make([]string, 0, len(st.Policies))
	for id := range st.Policies {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	priority := -1
	count := 0
	for _, id := range ids {
		p := st.Policies[id]
		if p.Scope != "global" && p.Scope != c.DeploymentID {
			continue
		}
		var value PolicyValue
		if json.Unmarshal(p.Value, &value) != nil || value.Status != "enabled" {
			continue
		}
		if value.TriggerEventName != "dbha_detect_db_failure" && value.TriggerEventName != "dbha_doublecheck_ssh_fail" {
			continue
		}
		if value.Priority >= priority {
			priority = value.Priority
			count = 0
			if value.Action == "switch" {
				count = value.TriggerCount
			}
		}
	}
	return count
}

func (s *Server) setHealth(ctx context.Context, c Cluster, health, reason string) error {
	return s.Store.Update(ctx, func(st *State) error {
		v := st.Clusters[clusterKey(c.ID)]
		if v.TopologyEpoch == c.TopologyEpoch {
			v.HealthState = health
			v.ReasonCodes = []string{reason}
			st.Clusters[clusterKey(c.ID)] = v
		}
		return nil
	})
}

func (s *Server) switchReady(st *State, c Cluster) bool {
	if c.TopologyState != "READY" || c.OperationState != "IDLE" || !c.SwitchingEnabled || c.Maintenance || c.RecoveryGate != "NONE" || len(c.ActiveStartPermits) > 0 || time.Since(c.LastHealthyAt) > 30*time.Second {
		return false
	}
	for _, id := range []string{c.PrimaryID, c.StandbyID} {
		if _, ok := st.Exclusions[id]; ok {
			return false
		}
	}
	b, ok := st.Instances[c.StandbyID]
	if !ok || b.Admission != "ACTIVE" || !s.freshInstance(st, b.ID) {
		return false
	}
	obs := observation(st, b.ID)
	if !obs.ReadOnly || obs.GTIDMode != "ON" || len(obs.Channels) != 1 || !obs.Channels[0].SQLRunning || obs.Channels[0].SourceUUID != strings.TrimPrefix(c.PrimaryID, "mysql:") {
		return false
	}
	for _, id := range c.ProxyIDs {
		if s.freshInstance(st, id) && sameBackend(observation(st, id), st.Instances[c.PrimaryID].Endpoint) {
			return true
		}
	}
	return false
}

func (s *Server) routeReady(ctx context.Context, st *State, c Cluster) error {
	if (c.TopologyState != "DATABASES_CONFIRMED" && c.TopologyState != "READY") || c.OperationState != "IDLE" || (c.RecoveryGate != "NONE" && c.RecoveryGate != "STARTUP_VALIDATION") || !s.freshInstance(st, c.PrimaryID) {
		return apiError(503, "ROUTE_NOT_READY", "database route has not been verified")
	}
	if err := s.Store.Ready(ctx); err != nil {
		return err
	}
	o, err := s.verify(ctx, st.Instances[c.PrimaryID], s.profile(st, c.DeploymentID))
	if err != nil {
		return apiError(503, "ROUTE_NOT_READY", "primary verification failed")
	}
	if o.ServerUUID != strings.TrimPrefix(c.PrimaryID, "mysql:") || o.ReadOnly || o.SuperReadOnly || len(o.Channels) != 0 {
		return apiError(503, "ROUTE_NOT_READY", "primary does not match committed role")
	}
	return nil
}

func routeData(st *State, c Cluster) map[string]any {
	i := st.Instances[c.PrimaryID]
	host, portText, _ := net.SplitHostPort(i.Endpoint)
	port, _ := strconv.Atoi(portText)
	return map[string]any{"cluster_id": c.ID, "topology_epoch": c.TopologyEpoch, "current_primary": map[string]any{"instance_id": i.ID, "host": host, "port": port}}
}
