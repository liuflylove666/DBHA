package server

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"
)

type PolicyValue struct {
	Name                   string `json:"name"`
	Status                 string `json:"status"`
	TriggerEventName       string `json:"trigger_event_name"`
	TriggerEventNameReason string `json:"trigger_event_name_reason"`
	TriggerCount           int    `json:"trigger_count"`
	Priority               int    `json:"priority"`
	Scope                  string `json:"scope"`
	Action                 string `json:"action"`
	Description            string `json:"description,omitempty"`
}

func defaultPolicy() PolicyValue {
	return PolicyValue{Name: "default-mysql-failure", Status: "enabled", TriggerEventName: "dbha_detect_db_failure", TriggerEventNameReason: "connection exception", TriggerCount: 3, Priority: 0, Scope: "cluster", Action: "switch"}
}

func validatePolicy(p PolicyValue) error {
	if p.Name == "" || len(p.Name) > 128 || len(p.Description) > 512 || p.TriggerCount < 1 || p.TriggerCount > 10 || p.Priority < 0 || p.Priority > 100 {
		return apiError(400, "INVALID_POLICY", "invalid name, count, priority or description")
	}
	if p.Status != "enabled" && p.Status != "disabled" {
		return apiError(400, "INVALID_POLICY", "status must be enabled or disabled")
	}
	if p.TriggerEventName != "dbha_detect_db_failure" && p.TriggerEventName != "dbha_doublecheck_ssh_fail" {
		return apiError(400, "INVALID_POLICY", "unsupported event")
	}
	if p.TriggerEventNameReason != "connection exception" && p.TriggerEventNameReason != "heartbeat write failure" && p.TriggerEventNameReason != "missed probe" && p.TriggerEventNameReason != "no target" {
		return apiError(400, "INVALID_POLICY", "unsupported event reason")
	}
	if p.Scope != "cluster" && p.Scope != "host" && p.Scope != "db_instance" {
		return apiError(400, "INVALID_POLICY", "unsupported action scope")
	}
	if p.Action != "switch" {
		return apiError(400, "INVALID_POLICY", "only implemented switch action is supported")
	}
	return nil
}

func (s *Store) ensureDefaultPolicy(ctx context.Context) error {
	return s.Update(ctx, func(st *State) error {
		if _, ok := st.Policies["default"]; ok {
			return nil
		}
		v, _ := json.Marshal(defaultPolicy())
		st.Policies["default"] = Policy{ID: "default", Scope: "global", Version: 1, Value: v}
		return nil
	})
}

func (s *Server) retireInstance(w http.ResponseWriter, r *http.Request) (any, error) {
	var req struct {
		ExpectedEpoch *uint64 `json:"expected_epoch"`
		Reason        string  `json:"reason"`
	}
	if err := decode(r, &req); err != nil {
		return nil, err
	}
	if req.ExpectedEpoch == nil {
		return nil, apiError(428, "EPOCH_REQUIRED", "expected_epoch required")
	}
	if req.Reason == "" {
		return nil, apiError(400, "REASON_REQUIRED", "retirement reason required")
	}
	id := r.PathValue("id")
	var result Instance
	err := s.Store.Update(r.Context(), func(st *State) error {
		i, ok := st.Instances[id]
		if !ok {
			return apiError(404, "INSTANCE_NOT_FOUND", "instance not found")
		}
		c := st.Clusters[clusterKey(st.Deployments[i.DeploymentID].ClusterID)]
		if c.TopologyEpoch != *req.ExpectedEpoch {
			return apiError(409, "VERSION_CONFLICT", "topology epoch changed")
		}
		if c.SwitchingEnabled || c.OperationState != "IDLE" || c.ActiveOperationID != "" || len(c.ActiveStartPermits) > 0 {
			return apiError(412, "RETIRE_BLOCKED", "disable switching and finish operations or permits")
		}
		if i.ID == c.PrimaryID {
			return apiError(412, "PRIMARY_RETIRE_BLOCKED", "confirmed primary requires verified recovery")
		}
		if i.Admission == "RETIRED" {
			result = i
			return nil
		}
		i.Admission = "RETIRED"
		i.RetireReason = req.Reason
		st.Instances[id] = i
		result = i
		formal := false
		if c.StandbyID == id {
			c.StandbyID = ""
			formal = true
		}
		for n, p := range c.ProxyIDs {
			if p == id {
				c.ProxyIDs = append(c.ProxyIDs[:n], c.ProxyIDs[n+1:]...)
				formal = true
				break
			}
		}
		if formal {
			c.TopologyEpoch++
			c.HealthState = "DEGRADED"
			c.ReasonCodes = []string{"MEMBER_RETIRED"}
			st.Clusters[clusterKey(c.ID)] = c
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if err = s.saveWatermark(r.Context()); err != nil {
		return nil, err
	}
	return result, nil
}

func (s *Server) reconcileInstance(w http.ResponseWriter, r *http.Request) (any, error) {
	var req struct {
		ExpectedEpoch         *uint64 `json:"expected_epoch"`
		Endpoint              string  `json:"endpoint"`
		ReplacementInstanceID string  `json:"replacement_instance_id"`
		AdminPort             *uint32 `json:"admin_port"`
	}
	if err := decode(r, &req); err != nil {
		return nil, err
	}
	if req.ExpectedEpoch == nil {
		return nil, apiError(428, "EPOCH_REQUIRED", "expected_epoch required")
	}
	if req.Endpoint != "" && req.ReplacementInstanceID != "" {
		return nil, apiError(400, "INVALID_RECONCILE", "choose endpoint or replacement")
	}
	if req.AdminPort != nil && (*req.AdminPort == 0 || req.ReplacementInstanceID != "") {
		return nil, apiError(400, "INVALID_ADMIN_PORT", "admin_port must be nonzero and is not combined with replacement")
	}
	st, err := s.Store.Read(r.Context())
	if err != nil {
		return nil, err
	}
	id := r.PathValue("id")
	old, ok := st.Instances[id]
	if !ok {
		return nil, apiError(404, "INSTANCE_NOT_FOUND", "instance not found")
	}
	c := st.Clusters[clusterKey(st.Deployments[old.DeploymentID].ClusterID)]
	if c.TopologyEpoch != *req.ExpectedEpoch {
		return nil, apiError(409, "VERSION_CONFLICT", "topology epoch changed")
	}
	if c.SwitchingEnabled || c.OperationState != "IDLE" || c.ActiveOperationID != "" || len(c.ActiveStartPermits) > 0 {
		return nil, apiError(412, "RECONCILE_BLOCKED", "disable switching and finish operations or permits")
	}
	if old.Admission == "RETIRED" {
		return nil, apiError(412, "INSTANCE_RETIRED", "retired identity cannot be revived")
	}
	verified := old
	if req.Endpoint != "" {
		verified.Endpoint = req.Endpoint
	}
	if req.AdminPort != nil {
		if old.Kind != "proxy" {
			return nil, apiError(400, "INVALID_ADMIN_PORT", "only proxy has admin_port")
		}
		verified.AdminPort = *req.AdminPort
	}
	if req.ReplacementInstanceID != "" {
		if id == c.PrimaryID {
			return nil, apiError(412, "PRIMARY_REPLACEMENT_BLOCKED", "primary replacement needs recovery")
		}
		var found bool
		verified, found = st.Instances[req.ReplacementInstanceID]
		if !found || verified.Admission == "RETIRED" || verified.DeploymentID != old.DeploymentID || verified.Kind != old.Kind {
			return nil, apiError(412, "REPLACEMENT_INVALID", "replacement identity, kind or deployment invalid")
		}
	}
	if !allowedEndpoint(verified.Endpoint, s.Config.AllowedNetworks) {
		return nil, apiError(412, "ADDRESS_UNRESOLVED", "endpoint outside server networks")
	}
	host, _, _ := net.SplitHostPort(verified.Endpoint)
	if !allowedAddress(st.Deployments[old.DeploymentID].AllowedNetworks, host) {
		return nil, apiError(412, "ADDRESS_UNRESOLVED", "endpoint outside deployment networks")
	}
	if err = s.verifyMember(r.Context(), st, c, verified); err != nil {
		return nil, err
	}
	var result Instance
	err = s.Store.Update(r.Context(), func(current *State) error {
		v := current.Clusters[clusterKey(c.ID)]
		i, exists := current.Instances[id]
		if !exists || v.TopologyEpoch != *req.ExpectedEpoch || v.SwitchingEnabled || v.OperationState != "IDLE" || len(v.ActiveStartPermits) > 0 || i.Admission == "RETIRED" {
			return apiError(409, "VERSION_CONFLICT", "topology or instance changed")
		}
		if req.ReplacementInstanceID != "" {
			next, ok := current.Instances[req.ReplacementInstanceID]
			if !ok || next.Admission == "RETIRED" || next.Endpoint != verified.Endpoint {
				return apiError(409, "VERSION_CONFLICT", "replacement changed")
			}
			i.Admission = "RETIRED"
			current.Instances[id] = i
			next.Admission = "ACTIVE"
			current.Instances[next.ID] = next
			result = next
			if v.StandbyID == id {
				v.StandbyID = next.ID
			} else {
				for n, p := range v.ProxyIDs {
					if p == id {
						v.ProxyIDs[n] = next.ID
						break
					}
				}
			}
		} else {
			if verified.Endpoint != i.Endpoint {
				newKey := i.Kind + ":" + verified.Endpoint
				if owner := current.EndpointIndex[newKey]; owner != "" && owner != id {
					return apiError(409, "ENDPOINT_CONFLICT", "endpoint belongs to another instance")
				}
				delete(current.EndpointIndex, i.Kind+":"+i.Endpoint)
				current.EndpointIndex[newKey] = id
				i.Endpoint = verified.Endpoint
			}
			if req.AdminPort != nil {
				i.AdminPort = *req.AdminPort
			}
			i.Admission = "ACTIVE"
			current.Instances[id] = i
			result = i
			if i.Kind == "mysql" && id != v.PrimaryID && v.StandbyID == "" {
				v.StandbyID = id
			}
		}
		v.TopologyEpoch++
		if v.TopologyState == "CONFLICT" {
			v.TopologyState = "DATABASES_CONFIRMED"
			v.ConfirmationCount = 0
			v.ConfirmationSince = time.Time{}
			v.LastEvidence = ""
		}
		v.HealthState = "DEGRADED"
		v.SwitchingEnabled = false
		v.ReasonCodes = []string{"POST_MAINTENANCE_VALIDATION"}
		current.Clusters[clusterKey(v.ID)] = v
		return nil
	})
	if err != nil {
		return nil, err
	}
	if err = s.saveWatermark(r.Context()); err != nil {
		return nil, err
	}
	return result, nil
}

func (s *Server) verifyMember(ctx context.Context, st *State, c Cluster, i Instance) error {
	o, err := s.verify(ctx, i, s.profile(st, c.DeploymentID))
	if err != nil {
		return apiError(412, "VERIFY_FAILED", "direct member verification failed")
	}
	if i.Kind == "proxy" {
		if !sameBackend(o, st.Instances[c.PrimaryID].Endpoint) {
			return apiError(412, "ROUTE_MISMATCH", "proxy does not route to confirmed primary")
		}
		return nil
	}
	if "mysql:"+o.ServerUUID != i.ID || o.GTIDMode != "ON" {
		return apiError(412, "IDENTITY_CONFLICT", "database identity or GTID differs")
	}
	if i.ID == c.PrimaryID {
		if o.ReadOnly || o.SuperReadOnly || len(o.Channels) != 0 {
			return apiError(412, "ROLE_CONFLICT", "confirmed primary facts differ")
		}
		return nil
	}
	if len(o.Channels) != 1 || o.Channels[0].SourceUUID != strings.TrimPrefix(c.PrimaryID, "mysql:") || !o.ReadOnly || !o.Channels[0].SQLRunning {
		return apiError(412, "REPLICATION_NOT_READY", "replacement is not verified replica of confirmed primary")
	}
	return nil
}

func (s *Server) recoverClusters(w http.ResponseWriter, r *http.Request) (any, error) {
	s.decisionMu.Lock()
	defer s.decisionMu.Unlock()
	var req struct {
		Gate     string `json:"gate"`
		Clusters []struct {
			ClusterID     uint64  `json:"cluster_id"`
			ExpectedEpoch *uint64 `json:"expected_epoch"`
		} `json:"clusters"`
	}
	if err := decode(r, &req); err != nil {
		return nil, err
	}
	if req.Gate != "RESTORE_UNVERIFIED" && req.Gate != "MIGRATION_UNVERIFIED" {
		return nil, apiError(400, "INVALID_GATE", "restore or migration gate required")
	}
	if len(req.Clusters) == 0 {
		return nil, apiError(400, "CLUSTERS_REQUIRED", "list clusters to verify")
	}
	st, err := s.Store.Read(r.Context())
	if err != nil {
		return nil, err
	}
	requested := map[string]uint64{}
	newPrimary := map[string]string{}
	verifiedMembers := map[string][]string{}
	stoppedMembers := map[string][]string{}
	for _, entry := range req.Clusters {
		if entry.ExpectedEpoch == nil {
			return nil, apiError(428, "EPOCH_REQUIRED", "expected_epoch required")
		}
		key := clusterKey(entry.ClusterID)
		c, ok := st.Clusters[key]
		if !ok || c.RecoveryGate != req.Gate || c.TopologyEpoch != *entry.ExpectedEpoch || c.ActiveOperationID != "" || c.OperationState != "IDLE" || len(c.ActiveStartPermits) > 0 {
			return nil, apiError(412, "RECOVERY_BLOCKED", "cluster gate, operation or epoch differs")
		}
		if _, dup := requested[key]; dup {
			return nil, apiError(400, "DUPLICATE_CLUSTER", "cluster listed twice")
		}
		requested[key] = *entry.ExpectedEpoch
		primary := st.Instances[c.PrimaryID]
		actual, e := s.verify(r.Context(), primary, s.profile(st, c.DeploymentID))
		chosen := c.PrimaryID
		if e != nil || actual.ServerUUID != strings.TrimPrefix(c.PrimaryID, "mysql:") || actual.ReadOnly || actual.SuperReadOnly || len(actual.Channels) != 0 {
			if c.StandbyID == "" || e == nil || !networkFailure(e) {
				return nil, apiError(412, "RECOVERY_EVIDENCE", "confirmed primary does not match live topology")
			}
			down, _ := s.sshHealth(r.Context(), primary, s.profile(st, c.DeploymentID))
			if !down {
				return nil, apiError(412, "RECOVERY_EVIDENCE", "old primary not independently confirmed down")
			}
			candidate := st.Instances[c.StandbyID]
			next, nextErr := s.verify(r.Context(), candidate, s.profile(st, c.DeploymentID))
			if nextErr != nil || next.ServerUUID != strings.TrimPrefix(candidate.ID, "mysql:") || next.ReadOnly || next.SuperReadOnly || len(next.Channels) != 0 {
				return nil, apiError(412, "RECOVERY_EVIDENCE", "new primary not verified")
			}
			chosen = candidate.ID
		} else if c.StandbyID != "" {
			standby := st.Instances[c.StandbyID]
			if standby.Admission == "ACTIVE" || req.Gate == "MIGRATION_UNVERIFIED" && standby.Admission == "RETURNED_UNVERIFIED" {
				if err = s.verifyMember(r.Context(), st, c, standby); err != nil {
					return nil, err
				}
				verifiedMembers[key] = append(verifiedMembers[key], standby.ID)
			}
		}
		if chosen == "" {
			return nil, apiError(412, "RECOVERY_EVIDENCE", "confirmed primary missing")
		}
		verifiedMembers[key] = append(verifiedMembers[key], chosen)
		for _, proxyID := range c.ProxyIDs {
			p := st.Instances[proxyID]
			if p.Admission == "RETIRED" {
				continue
			}
			o, e := s.verify(r.Context(), p, s.profile(st, c.DeploymentID))
			if networkFailure(e) && s.verifyProxyStopped(r.Context(), p, s.profile(st, c.DeploymentID)) == nil {
				// A fully stopped Proxy can safely obtain a new permit after
				// this explicit recovery. Requiring its route here deadlocks
				// a cold start while the recovery gate blocks all permits.
				stoppedMembers[key] = append(stoppedMembers[key], proxyID)
				continue
			}
			if e != nil || !sameBackend(o, st.Instances[chosen].Endpoint) {
				return nil, apiError(412, "RECOVERY_EVIDENCE", "proxy route does not match verified primary")
			}
			verifiedMembers[key] = append(verifiedMembers[key], proxyID)
		}
		newPrimary[key] = chosen
	}
	for key, c := range st.Clusters {
		if c.RecoveryGate == req.Gate {
			if _, ok := requested[key]; !ok {
				return nil, apiError(412, "RECOVERY_INCOMPLETE", "all gated clusters must be verified together")
			}
		}
	}
	err = s.Store.Update(r.Context(), func(current *State) error {
		for key, epoch := range requested {
			c := current.Clusters[key]
			if c.RecoveryGate != req.Gate || c.TopologyEpoch != epoch || c.OperationState != "IDLE" || c.ActiveOperationID != "" || len(c.ActiveStartPermits) > 0 {
				return apiError(409, "VERSION_CONFLICT", "recovery state changed")
			}
			members := append(append([]string{}, verifiedMembers[key]...), stoppedMembers[key]...)
			if !evidenceUnchanged(st, current, members) {
				return apiError(409, "VERSION_CONFLICT", "recovery evidence changed")
			}
			if chosen := newPrimary[key]; chosen != c.PrimaryID {
				old := current.Instances[c.PrimaryID]
				old.Admission = "RETURNED_UNVERIFIED"
				current.Instances[old.ID] = old
				c.PrimaryID = chosen
				c.StandbyID = ""
				c.TopologyEpoch++
			}
			for _, memberID := range verifiedMembers[key] {
				member := current.Instances[memberID]
				if member.Admission == "RETURNED_UNVERIFIED" || member.Admission == "CANDIDATE" {
					member.Admission = "ACTIVE"
					current.Instances[memberID] = member
				}
			}
			for _, memberID := range stoppedMembers[key] {
				member := current.Instances[memberID]
				member.Admission = "CANDIDATE"
				current.Instances[memberID] = member
			}
			c.SwitchingEnabled = false
			c.HealthState = "DEGRADED"
			c.LastHealthyAt = time.Time{}
			c.ReasonCodes = []string{"POST_RECOVERY_VALIDATION"}
			current.Clusters[key] = c
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if err = s.saveWatermark(r.Context()); err != nil {
		return nil, err
	}
	if req.Gate == "RESTORE_UNVERIFIED" {
		if err = removeRestoreMarker(s.Config.StateDir); err != nil {
			return nil, err
		}
	}
	err = s.Store.Update(r.Context(), func(current *State) error {
		for key := range requested {
			c := current.Clusters[key]
			if c.RecoveryGate != req.Gate {
				return apiError(409, "VERSION_CONFLICT", "recovery gate changed")
			}
			c.RecoveryGate = "NONE"
			current.Clusters[key] = c
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if err = s.saveWatermark(r.Context()); err != nil {
		return nil, err
	}
	return map[string]any{"verified_clusters": len(requested), "switching_enabled": false}, nil
}

func (s *Server) policyResource(w http.ResponseWriter, r *http.Request) (any, error) {
	path := r.URL.Path
	id := r.PathValue("id")
	exclusion := strings.Contains(path, "/exclusions")
	st, err := s.Store.Read(r.Context())
	if err != nil {
		return nil, err
	}
	if r.Method == http.MethodGet {
		if exclusion {
			if id != "" {
				v, ok := st.Exclusions[id]
				if !ok {
					return nil, apiError(404, "EXCLUSION_NOT_FOUND", "exclusion not found")
				}
				w.Header().Set("ETag", strconv.FormatUint(v.Version, 10))
				return v, nil
			}
			items := []Exclusion{}
			for _, v := range st.Exclusions {
				items = append(items, v)
			}
			sort.Slice(items, func(i, j int) bool { return items[i].InstanceID < items[j].InstanceID })
			return map[string]any{"items": items}, nil
		}
		if id != "" {
			v, ok := st.Policies[id]
			if !ok {
				return nil, apiError(404, "POLICY_NOT_FOUND", "policy not found")
			}
			w.Header().Set("ETag", strconv.FormatUint(v.Version, 10))
			return v, nil
		}
		items := []Policy{}
		for _, v := range st.Policies {
			items = append(items, v)
		}
		sort.Slice(items, func(i, j int) bool { return items[i].ID < items[j].ID })
		return map[string]any{"items": items}, nil
	}
	if id == "" {
		return nil, apiError(405, "METHOD_NOT_ALLOWED", "resource id required")
	}
	if r.Method != http.MethodPut && r.Method != http.MethodDelete {
		return nil, apiError(405, "METHOD_NOT_ALLOWED", "unsupported resource method")
	}
	match := r.Header.Get("If-Match")
	create := r.Header.Get("If-None-Match") == "*"
	if match == "" && !create {
		return nil, apiError(428, "VERSION_REQUIRED", "If-Match or If-None-Match: * required")
	}
	if match != "" && create {
		return nil, apiError(400, "INVALID_PRECONDITION", "choose one version precondition")
	}
	var expected uint64
	if match != "" {
		expected, err = strconv.ParseUint(strings.Trim(match, "\""), 10, 64)
		if err != nil {
			return nil, apiError(400, "INVALID_VERSION", "If-Match must be resource version")
		}
	}
	if r.Method == http.MethodDelete && create {
		return nil, apiError(428, "VERSION_REQUIRED", "If-Match required for deletion")
	}
	var newPolicy Policy
	var newExclusion Exclusion
	if r.Method == http.MethodPut {
		if exclusion {
			var body struct {
				Reason    string    `json:"reason"`
				ExpiresAt time.Time `json:"expires_at"`
			}
			if err = decode(r, &body); err != nil {
				return nil, err
			}
			if strings.TrimSpace(body.Reason) == "" || len(body.Reason) > 256 {
				return nil, apiError(400, "REASON_REQUIRED", "reason required and limited to 256 characters")
			}
			newExclusion = Exclusion{InstanceID: id, Reason: body.Reason, ExpiresAt: body.ExpiresAt}
		} else {
			var body struct {
				PolicyScope string `json:"policy_scope"`
				PolicyValue
			}
			if err = decode(r, &body); err != nil {
				return nil, err
			}
			if err = validatePolicy(body.PolicyValue); err != nil {
				return nil, err
			}
			if body.PolicyScope == "" {
				body.PolicyScope = "global"
			}
			if body.PolicyScope != "global" {
				if _, ok := st.Deployments[body.PolicyScope]; !ok {
					return nil, apiError(400, "INVALID_SCOPE", "unknown deployment scope")
				}
			}
			v, _ := json.Marshal(body.PolicyValue)
			newPolicy = Policy{ID: id, Scope: body.PolicyScope, Value: v}
		}
	}
	var result any
	err = s.Store.Update(r.Context(), func(current *State) error {
		if exclusion {
			old, exists := current.Exclusions[id]
			if create && exists || match != "" && (!exists || old.Version != expected) {
				return apiError(409, "VERSION_CONFLICT", "exclusion version changed")
			}
			if r.Method == http.MethodDelete {
				delete(current.Exclusions, id)
				result = map[string]bool{"deleted": true}
				return nil
			}
			if _, ok := current.Instances[id]; !ok {
				return apiError(412, "INSTANCE_UNKNOWN", "exclusion requires registered instance")
			}
			newExclusion.Version = old.Version + 1
			current.Exclusions[id] = newExclusion
			result = newExclusion
			return nil
		}
		old, exists := current.Policies[id]
		if create && exists || match != "" && (!exists || old.Version != expected) {
			return apiError(409, "VERSION_CONFLICT", "policy version changed")
		}
		if r.Method == http.MethodDelete {
			delete(current.Policies, id)
			result = map[string]bool{"deleted": true}
			return nil
		}
		newPolicy.Version = old.Version + 1
		current.Policies[id] = newPolicy
		result = newPolicy
		return nil
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}
