// Package discovery contains the wire contract shared by the probe and control service.
package discovery

import (
	"encoding/json"
	"time"
)

const SchemaVersion = 1

type Envelope struct {
	SchemaVersion        int             `json:"schema_version"`
	AgentID              string          `json:"agent_id"`
	SessionID            string          `json:"session_id"`
	SessionEpoch         uint64          `json:"session_epoch"`
	InstanceID           string          `json:"instance_id,omitempty"`
	Stream               string          `json:"stream"`
	Sequence             uint64          `json:"sequence"`
	SampledAt            time.Time       `json:"sampled_at"`
	CollectionDurationMS int64           `json:"collection_duration_ms"`
	Payload              json.RawMessage `json:"payload"`
}

type Channel struct {
	ChannelName         string `json:"channel_name"`
	SourceUUID          string `json:"source_uuid"`
	SourceHost          string `json:"source_host"`
	SourcePort          uint32 `json:"source_port"`
	IORunning           bool   `json:"io_running"`
	SQLRunning          bool   `json:"sql_running"`
	SecondsBehindSource *int64 `json:"seconds_behind_source,omitempty"`
	RetrievedGTIDSet    string `json:"retrieved_gtid_set,omitempty"`
	ExecutedGTIDSet     string `json:"executed_gtid_set,omitempty"`
}

type Backend struct {
	Address string `json:"address"`
	State   string `json:"state"`
	Type    string `json:"type"`
	UUID    string `json:"uuid,omitempty"`
}

type Observation struct {
	Kind                  string    `json:"kind"`
	AdvertiseHost         string    `json:"advertise_host"`
	Port                  uint32    `json:"port,omitempty"`
	ServerUUID            string    `json:"server_uuid,omitempty"`
	ServerID              uint64    `json:"server_id,omitempty"`
	Version               string    `json:"version,omitempty"`
	ReadOnly              bool      `json:"read_only,omitempty"`
	SuperReadOnly         bool      `json:"super_read_only,omitempty"`
	GTIDMode              string    `json:"gtid_mode,omitempty"`
	ReplicationQueryState string    `json:"replication_query_state,omitempty"`
	Channels              []Channel `json:"channels,omitempty"`
	CollectionState       string    `json:"collection_state"`
	ErrorCode             string    `json:"error_code,omitempty"`
	ProxyUUID             string    `json:"proxy_uuid,omitempty"`
	DataPort              uint32    `json:"data_port,omitempty"`
	AdminPort             uint32    `json:"admin_port,omitempty"`
	ProcessState          string    `json:"process_state,omitempty"`
	BackendQueryState     string    `json:"backend_query_state,omitempty"`
	Backends              []Backend `json:"backends,omitempty"`
}

type ReportResponse struct {
	InstanceID                 string   `json:"instance_id"`
	AcceptedSequence           uint64   `json:"accepted_sequence"`
	StoredRevision             int64    `json:"stored_revision"`
	Duplicate                  bool     `json:"duplicate"`
	TopologyState              string   `json:"topology_state"`
	ReasonCodes                []string `json:"reason_codes,omitempty"`
	RouteReconcileRequired     bool     `json:"route_reconcile_required,omitempty"`
	AuthoritativeTopologyEpoch uint64   `json:"authoritative_topology_epoch,omitempty"`
}

type RegisterRequest struct {
	SchemaVersion  int      `json:"schema_version"`
	AgentID        string   `json:"agent_id"`
	BootID         string   `json:"boot_id"`
	BootGeneration uint64   `json:"boot_generation"`
	ProbeVersion   string   `json:"probe_version"`
	Capabilities   []string `json:"capabilities"`
}

type RegisterResponse struct {
	SessionID    string `json:"session_id"`
	SessionEpoch uint64 `json:"session_epoch"`
}
