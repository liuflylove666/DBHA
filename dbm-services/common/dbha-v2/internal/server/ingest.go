package server

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net"
	"net/http"
	"net/netip"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"dbm-services/common/dbha-v2/pkg/discovery"
)

const maxObservationBytes = 64 << 10
const maxSampleBytes = 128 << 10

func (s *Store) Ingest(ctx context.Context, token string, env discovery.Envelope) (discovery.ReportResponse, error) {
	if env.SchemaVersion != discovery.SchemaVersion || env.AgentID == "" || env.SessionID == "" || env.SessionEpoch == 0 || env.Sequence == 0 {
		return discovery.ReportResponse{}, apiError(http.StatusBadRequest, "INVALID_ENVELOPE", "missing or invalid report envelope")
	}
	observation := env.Stream == "topology"
	if !observation && env.Stream != "default" && env.Stream != "heartbeat" && env.Stream != "repldelay" {
		return discovery.ReportResponse{}, apiError(http.StatusBadRequest, "INVALID_STREAM", "unsupported report stream")
	}
	limit := maxSampleBytes
	if observation {
		limit = maxObservationBytes
	}
	if len(env.Payload) > limit {
		return discovery.ReportResponse{}, apiError(http.StatusRequestEntityTooLarge, "REPORT_TOO_LARGE", "report exceeds stream limit")
	}
	if !json.Valid(env.Payload) {
		return discovery.ReportResponse{}, apiError(http.StatusBadRequest, "INVALID_PAYLOAD", "payload must be JSON")
	}
	if err := s.reserveIngest(len(env.Payload)); err != nil {
		return discovery.ReportResponse{}, err
	}
	defer s.releaseIngest(len(env.Payload))
	now := time.Now().UTC()
	if env.SampledAt.Before(now.Add(-15 * time.Second)) {
		return discovery.ReportResponse{}, apiError(http.StatusUnprocessableEntity, "STALE_SAMPLE", "sample is too old")
	}
	if env.SampledAt.After(now.Add(5 * time.Second)) {
		return discovery.ReportResponse{}, apiError(http.StatusUnprocessableEntity, "CLOCK_SKEW", "sample is in the future")
	}
	if env.CollectionDurationMS < 0 {
		return discovery.ReportResponse{}, apiError(http.StatusBadRequest, "INVALID_DURATION", "negative collection duration")
	}
	var canonical bytes.Buffer
	if err := json.Compact(&canonical, env.Payload); err != nil {
		return discovery.ReportResponse{}, apiError(http.StatusBadRequest, "INVALID_PAYLOAD", "payload must be JSON")
	}
	var decoded any
	dec := json.NewDecoder(bytes.NewReader(canonical.Bytes()))
	dec.UseNumber()
	if err := dec.Decode(&decoded); err != nil {
		return discovery.ReportResponse{}, err
	}
	canonicalBytes, err := json.Marshal(decoded)
	if err != nil {
		return discovery.ReportResponse{}, err
	}
	digestInput, _ := json.Marshal(struct {
		SchemaVersion        int             `json:"schema_version"`
		AgentID              string          `json:"agent_id"`
		SessionID            string          `json:"session_id"`
		SessionEpoch         uint64          `json:"session_epoch"`
		InstanceID           string          `json:"instance_id"`
		Stream               string          `json:"stream"`
		Sequence             uint64          `json:"sequence"`
		SampledAt            time.Time       `json:"sampled_at"`
		CollectionDurationMS int64           `json:"collection_duration_ms"`
		Payload              json.RawMessage `json:"payload"`
	}{env.SchemaVersion, env.AgentID, env.SessionID, env.SessionEpoch, env.InstanceID, env.Stream, env.Sequence, env.SampledAt, env.CollectionDurationMS, canonicalBytes})
	digestBytes := sha256.Sum256(digestInput)
	digest := hex.EncodeToString(digestBytes[:])
	var obs discovery.Observation
	if observation {
		observationDecoder := json.NewDecoder(bytes.NewReader(env.Payload))
		observationDecoder.DisallowUnknownFields()
		if err := observationDecoder.Decode(&obs); err != nil {
			return discovery.ReportResponse{}, apiError(http.StatusBadRequest, "INVALID_OBSERVATION", err.Error())
		}
	}
	var response discovery.ReportResponse
	var reportedConflict error
	commitRevision, err := s.updateIngestRevision(ctx, env.AgentID, observation, func(st *State) error {
		reportedConflict = nil
		a, ok := st.Agents[env.AgentID]
		if !ok {
			return apiError(http.StatusUnauthorized, "UNAUTHORIZED", "unknown agent")
		}
		c := st.Credentials[a.CredentialID]
		if c.Revoked || !equalToken(c.TokenHash, token) {
			return apiError(http.StatusUnauthorized, "UNAUTHORIZED", "invalid agent credential")
		}
		if a.SessionClosed || a.SessionID != env.SessionID || a.SessionEpoch != env.SessionEpoch {
			return apiError(http.StatusConflict, "SESSION_EXPIRED", "session is no longer active")
		}
		d := st.Deployments[a.DeploymentID]
		cluster := st.Clusters[clusterKey(d.ClusterID)]
		instanceID := env.InstanceID
		valid := true
		if !observation {
			var sample struct {
				CollectionState string `json:"collection_state"`
				ErrorCode       string `json:"error_code"`
			}
			_ = json.Unmarshal(env.Payload, &sample)
			if sample.CollectionState != "" && sample.CollectionState != "OK" {
				valid = false
				a.LastErrorCode = sample.ErrorCode
			}
		}
		mark := a.Streams[env.Stream]
		if mark.SessionID == env.SessionID {
			if env.Sequence < mark.Sequence {
				return apiError(http.StatusConflict, "OUT_OF_ORDER", "report sequence is stale")
			}
			if env.Sequence == mark.Sequence {
				if mark.Digest != digest {
					return apiError(http.StatusConflict, "SEQUENCE_CONFLICT", "sequence payload differs")
				}
				response = discovery.ReportResponse{InstanceID: mark.InstanceID, AcceptedSequence: mark.Sequence, StoredRevision: mark.StoredRevision, Duplicate: true, TopologyState: cluster.TopologyState, ReasonCodes: cluster.ReasonCodes, AuthoritativeTopologyEpoch: cluster.TopologyEpoch}
				return nil
			}
		}
		acceptMark := func(id string) {
			if a.Streams == nil {
				a.Streams = make(map[string]SequenceMark)
			}
			a.Streams[env.Stream] = SequenceMark{SessionID: env.SessionID, Sequence: env.Sequence, Digest: digest, InstanceID: id}
		}
		if observation {
			if obs.Kind != a.AllowedKind {
				return apiError(http.StatusForbidden, "KIND_FORBIDDEN", "observation kind not allowed by credential")
			}
			if obs.CollectionState != "OK" && !(obs.Kind == "proxy" && obs.ProxyUUID != "" && obs.ProcessState == "STARTING") {
				a.LastContactAt = now
				a.LastErrorCode = obs.ErrorCode
				acceptMark(instanceID)
				st.Agents[a.ID] = a
				response = discovery.ReportResponse{InstanceID: instanceID, AcceptedSequence: env.Sequence, TopologyState: cluster.TopologyState, ReasonCodes: cluster.ReasonCodes}
				return nil
			}
			if obs.Kind == "mysql" {
				if _, err := uuid.Parse(obs.ServerUUID); err != nil {
					return apiError(http.StatusUnprocessableEntity, "IDENTITY_MISSING", "mysql server_uuid is required")
				}
				instanceID = "mysql:" + obs.ServerUUID
			} else {
				if _, err := uuid.Parse(obs.ProxyUUID); err != nil {
					return apiError(http.StatusUnprocessableEntity, "IDENTITY_MISSING", "proxy uuid is required")
				}
				instanceID = "proxy:" + obs.ProxyUUID
			}
			if env.InstanceID != "" && env.InstanceID != instanceID {
				return apiError(http.StatusConflict, "IDENTITY_CONFLICT", "instance identity changed")
			}
		} else if instanceID == "" {
			return apiError(http.StatusFailedDependency, "INSTANCE_UNKNOWN", "sample requires registered instance")
		}
		inst, exists := st.Instances[instanceID]
		if !observation && !exists {
			return apiError(http.StatusFailedDependency, "INSTANCE_UNKNOWN", "sample requires registered instance")
		}
		if exists {
			if inst.Admission == "RETIRED" {
				return apiError(http.StatusConflict, "INSTANCE_RETIRED", "retired instance cannot report")
			}
			if inst.AgentID != a.ID || inst.DeploymentID != a.DeploymentID || inst.Kind != a.AllowedKind {
				if inst.DeploymentID != a.DeploymentID {
					cluster.TopologyState = "CONFLICT"
					cluster.ReasonCodes = []string{"CROSS_DEPLOYMENT_IDENTITY"}
					st.Clusters[clusterKey(d.ClusterID)] = cluster
					reportedConflict = apiError(http.StatusConflict, "CROSS_DEPLOYMENT_IDENTITY", "identity belongs to another deployment")
					return nil
				}
				inst.Admission = "QUARANTINED"
				st.Instances[instanceID] = inst
				cluster.TopologyState = "CONFLICT"
				cluster.ReasonCodes = []string{"IDENTITY_CONFLICT"}
				st.Clusters[clusterKey(d.ClusterID)] = cluster
				a.LastContactAt = now
				a.LastErrorCode = "IDENTITY_CONFLICT"
				acceptMark(instanceID)
				st.Agents[a.ID] = a
				response = discovery.ReportResponse{InstanceID: instanceID, AcceptedSequence: env.Sequence, TopologyState: "CONFLICT", ReasonCodes: []string{"IDENTITY_CONFLICT"}}
				return nil
			}
		}
		endpoint := ""
		if observation {
			port := obs.Port
			if obs.Kind == "proxy" {
				port = obs.DataPort
			}
			if obs.AdvertiseHost == "" || port == 0 {
				return apiError(http.StatusUnprocessableEntity, "ADDRESS_UNRESOLVED", "advertised host and port required")
			}
			if !allowedAddress(d.AllowedNetworks, obs.AdvertiseHost) {
				return apiError(http.StatusForbidden, "ADDRESS_FORBIDDEN", "advertised host is outside deployment networks")
			}
			endpoint = net.JoinHostPort(obs.AdvertiseHost, strconv.FormatUint(uint64(port), 10))
			indexKey := obs.Kind + ":" + endpoint
			if otherID, ok := st.EndpointIndex[indexKey]; ok && otherID != instanceID {
				other := st.Instances[otherID]
				if other.DeploymentID != a.DeploymentID {
					cluster.TopologyState = "CONFLICT"
					cluster.ReasonCodes = []string{"CROSS_DEPLOYMENT_ENDPOINT"}
					st.Clusters[clusterKey(d.ClusterID)] = cluster
					reportedConflict = apiError(http.StatusConflict, "CROSS_DEPLOYMENT_ENDPOINT", "endpoint belongs to another deployment")
					return nil
				}
				other.Admission = "QUARANTINED"
				st.Instances[otherID] = other
				inst = Instance{ID: instanceID, AgentID: a.ID, DeploymentID: a.DeploymentID, Kind: obs.Kind, Endpoint: endpoint, Admission: "QUARANTINED"}
				st.Instances[instanceID] = inst
				cluster.TopologyState = "CONFLICT"
				cluster.ReasonCodes = []string{"ENDPOINT_CONFLICT"}
				st.Clusters[clusterKey(d.ClusterID)] = cluster
				a.LastContactAt = now
				a.LastErrorCode = "ENDPOINT_CONFLICT"
				acceptMark(instanceID)
				st.Agents[a.ID] = a
				response = discovery.ReportResponse{InstanceID: instanceID, AcceptedSequence: env.Sequence, TopologyState: "CONFLICT", ReasonCodes: cluster.ReasonCodes}
				return nil
			}
			if !exists {
				inst = Instance{ID: instanceID, AgentID: a.ID, DeploymentID: a.DeploymentID, Kind: obs.Kind, Endpoint: endpoint, AdminPort: obs.AdminPort, Admission: "CANDIDATE"}
				st.EndpointIndex[indexKey] = instanceID
				for _, previous := range st.Instances {
					if previous.AgentID == a.ID && previous.Kind == obs.Kind && previous.ID != instanceID && previous.Admission != "RETIRED" {
						inst.Admission = "QUARANTINED"
						cluster.TopologyState = "CONFLICT"
						cluster.ReasonCodes = []string{"IDENTITY_REPLACEMENT_REQUIRED"}
						st.Clusters[clusterKey(d.ClusterID)] = cluster
						break
					}
				}
			} else if inst.Endpoint != endpoint {
				inst.Admission = "QUARANTINED"
				cluster.TopologyState = "CONFLICT"
				cluster.ReasonCodes = []string{"ADDRESS_CHANGED"}
				st.Clusters[clusterKey(d.ClusterID)] = cluster
			}
			if obs.Kind == "proxy" && obs.AdminPort != 0 {
				if inst.AdminPort != 0 && inst.AdminPort != obs.AdminPort {
					inst.Admission = "QUARANTINED"
					cluster.TopologyState = "CONFLICT"
					cluster.ReasonCodes = []string{"ADMIN_PORT_CHANGED"}
					st.Clusters[clusterKey(d.ClusterID)] = cluster
				} else if inst.AdminPort == 0 {
					inst.AdminPort = obs.AdminPort
				}
			}
			st.Instances[instanceID] = inst
			valid = obs.CollectionState == "OK" && (obs.Kind != "mysql" || obs.ReplicationQueryState == "OK")
		}
		key := instanceID + "/" + env.Stream
		a.LastContactAt = now
		if valid {
			a.LastErrorCode = ""
		}
		if valid {
			a.LastValidReportAt = now
		}
		acceptMark(instanceID)
		st.Agents[a.ID] = a
		stored := Report{Envelope: env, Digest: digest, ReceivedAt: now, Payload: canonicalBytes, Valid: valid}
		stored.Envelope.InstanceID = instanceID
		if observation {
			st.Observations[instanceID] = stored
		} else {
			st.Samples[key] = stored
		}
		response = discovery.ReportResponse{InstanceID: instanceID, AcceptedSequence: env.Sequence, StoredRevision: stored.StoredRevision, TopologyState: cluster.TopologyState, ReasonCodes: cluster.ReasonCodes, AuthoritativeTopologyEpoch: cluster.TopologyEpoch}
		// During a controlled switch, blocking/rerouting intentionally differs
		// from the committed primary. The operation owns those external actions;
		// a supervisor must not stop the Proxy in the middle of that sequence.
		if obs.Kind == "proxy" && cluster.PrimaryID != "" && cluster.TopologyEpoch > 0 && cluster.OperationState == "IDLE" && cluster.ActiveOperationID == "" && (cluster.RecoveryGate == "NONE" || cluster.RecoveryGate == "STARTUP_VALIDATION") {
			primary := st.Instances[cluster.PrimaryID]
			matched := len(obs.Backends) > 0
			for _, backend := range obs.Backends {
				if !strings.EqualFold(backend.Address, primary.Endpoint) {
					matched = false
					break
				}
			}
			response.RouteReconcileRequired = !matched && obs.ProcessState == "RUNNING"
		}
		return nil
	})
	if err != nil {
		return discovery.ReportResponse{}, err
	}
	if reportedConflict != nil {
		return discovery.ReportResponse{}, reportedConflict
	}
	if !response.Duplicate {
		response.StoredRevision = commitRevision
	}
	return response, nil
}

func allowedAddress(networks []string, host string) bool {
	if len(networks) == 0 {
		return true
	}
	addr, err := netip.ParseAddr(host)
	if err != nil {
		return false
	}
	for _, raw := range networks {
		p, err := netip.ParsePrefix(raw)
		if err == nil && p.Contains(addr) {
			return true
		}
	}
	return false
}

func (s *Store) reserveIngest(n int) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ingestCount >= 1024 || s.ingestBytes+n > 8<<20 {
		return apiError(http.StatusTooManyRequests, "INGEST_BACKPRESSURE", "ingest capacity exhausted")
	}
	s.ingestCount++
	s.ingestBytes += n
	return nil
}

func (s *Store) releaseIngest(n int) { s.mu.Lock(); s.ingestCount--; s.ingestBytes -= n; s.mu.Unlock() }
