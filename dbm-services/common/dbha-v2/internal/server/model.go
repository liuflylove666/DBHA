package server

import (
	"encoding/json"
	"fmt"
	"time"

	"dbm-services/common/dbha-v2/pkg/discovery"
)

const schemaVersion = 1

// Error carries a stable API code and an HTTP status. Callers may map it to gRPC.
type Error struct {
	Code    string `json:"code"`
	Status  int    `json:"-"`
	Message string `json:"message"`
}

func (e *Error) Error() string { return fmt.Sprintf("%s: %s", e.Code, e.Message) }

func apiError(status int, code, message string) error {
	return &Error{Code: code, Status: status, Message: message}
}

type Deployment struct {
	ID                string   `json:"id"`
	ClusterID         uint64   `json:"cluster_id"`
	IdempotencyKey    string   `json:"idempotency_key"`
	MySQLBudget       int      `json:"mysql_budget"`
	ProxyBudget       int      `json:"proxy_budget"`
	AllowedNetworks   []string `json:"allowed_networks,omitempty"`
	CredentialProfile string   `json:"credential_profile,omitempty"`
}

type Credential struct {
	ID           string `json:"id"`
	AgentID      string `json:"agent_id,omitempty"`
	DeploymentID string `json:"deployment_id,omitempty"`
	AllowedKind  string `json:"allowed_kind,omitempty"`
	TokenHash    string `json:"token_hash"`
	Revoked      bool   `json:"revoked"`
	Admin        bool   `json:"admin,omitempty"`
}

type Agent struct {
	ID                string                  `json:"id"`
	DeploymentID      string                  `json:"deployment_id"`
	CredentialID      string                  `json:"credential_id"`
	AllowedKind       string                  `json:"allowed_kind"`
	BootID            string                  `json:"boot_id,omitempty"`
	BootGeneration    uint64                  `json:"boot_generation"`
	SessionID         string                  `json:"session_id,omitempty"`
	SessionEpoch      uint64                  `json:"session_epoch"`
	SessionClosed     bool                    `json:"session_closed"`
	LastContactAt     time.Time               `json:"last_contact_at,omitempty"`
	LastValidReportAt time.Time               `json:"last_valid_report_at,omitempty"`
	LastErrorCode     string                  `json:"last_error_code,omitempty"`
	Streams           map[string]SequenceMark `json:"streams,omitempty"`
}

type SequenceMark struct {
	SessionID      string `json:"session_id"`
	Sequence       uint64 `json:"sequence"`
	Digest         string `json:"digest"`
	InstanceID     string `json:"instance_id,omitempty"`
	StoredRevision int64  `json:"stored_revision"`
}

type Instance struct {
	ID           string `json:"id"`
	AgentID      string `json:"agent_id"`
	DeploymentID string `json:"deployment_id"`
	Kind         string `json:"kind"`
	Endpoint     string `json:"endpoint"`
	AdminPort    uint32 `json:"admin_port,omitempty"`
	Admission    string `json:"admission"`
	RetireReason string `json:"retire_reason,omitempty"`
}

type StartPermit struct {
	ID        string    `json:"id"`
	RequestID string    `json:"request_id"`
	ProxyID   string    `json:"proxy_id"`
	Epoch     uint64    `json:"epoch"`
	ExpiresAt time.Time `json:"expires_at"`
}

type Cluster struct {
	ID                 uint64                 `json:"id"`
	DeploymentID       string                 `json:"deployment_id"`
	PrimaryID          string                 `json:"primary_id,omitempty"`
	StandbyID          string                 `json:"standby_id,omitempty"`
	ProxyIDs           []string               `json:"proxy_ids,omitempty"`
	TopologyEpoch      uint64                 `json:"topology_epoch"`
	TopologyState      string                 `json:"topology_state"`
	HealthState        string                 `json:"health_state"`
	OperationState     string                 `json:"operation_state"`
	RecoveryGate       string                 `json:"recovery_gate"`
	ActiveOperationID  string                 `json:"active_operation_id,omitempty"`
	ActiveStartPermits map[string]StartPermit `json:"active_start_permits,omitempty"`
	Maintenance        bool                   `json:"maintenance"`
	SwitchingEnabled   bool                   `json:"switching_enabled"`
	ReasonCodes        []string               `json:"reason_codes,omitempty"`
	ConfirmationCount  int                    `json:"confirmation_count,omitempty"`
	ConfirmationSince  time.Time              `json:"confirmation_since,omitempty"`
	LastEvidence       string                 `json:"last_evidence,omitempty"`
	LastHealthyAt      time.Time              `json:"last_healthy_at,omitempty"`
}

type Report struct {
	Envelope       discovery.Envelope `json:"envelope"`
	Digest         string             `json:"digest"`
	ReceivedAt     time.Time          `json:"received_at"`
	StoredRevision int64              `json:"stored_revision"`
	Payload        json.RawMessage    `json:"payload"`
	Valid          bool               `json:"valid"`
}

type OperationStep struct {
	IntentWritten bool `json:"intent_written"`
	ActionStarted bool `json:"action_started"`
	Verified      bool `json:"verified"`
}

type Operation struct {
	ID               string                   `json:"id"`
	ClusterID        uint64                   `json:"cluster_id"`
	Phase            string                   `json:"phase"`
	InputEpoch       uint64                   `json:"input_epoch"`
	OldPrimaryID     string                   `json:"old_primary_id"`
	CandidateID      string                   `json:"candidate_id"`
	ProxyIDs         []string                 `json:"proxy_ids,omitempty"`
	PolicySnapshot   []Policy                 `json:"policy_snapshot,omitempty"`
	EvidenceRev      int64                    `json:"evidence_revision"`
	Steps            map[string]OperationStep `json:"steps,omitempty"`
	PhaseEnteredAt   time.Time                `json:"phase_entered_at,omitempty"`
	PhaseDurationsMS map[string]int64         `json:"phase_durations_ms,omitempty"`
	Result           string                   `json:"result,omitempty"`
	CreatedAt        time.Time                `json:"created_at"`
	CompletedAt      time.Time                `json:"completed_at,omitempty"`
}

type Policy struct {
	ID      string          `json:"id"`
	Scope   string          `json:"scope"`
	Version uint64          `json:"version"`
	Value   json.RawMessage `json:"value"`
}

type Exclusion struct {
	InstanceID string    `json:"instance_id"`
	Reason     string    `json:"reason"`
	ExpiresAt  time.Time `json:"expires_at,omitempty"`
	Version    uint64    `json:"version"`
}

type State struct {
	Revision         int64                 `json:"-"`
	VersionRevision  int64                 `json:"-"`
	EtcdClusterID    uint64                `json:"-"`
	ModRevisions     map[string]int64      `json:"-"`
	ClusterIDCounter uint64                `json:"cluster_id_counter"`
	Deployments      map[string]Deployment `json:"deployments"`
	Credentials      map[string]Credential `json:"credentials"`
	Agents           map[string]Agent      `json:"agents"`
	Instances        map[string]Instance   `json:"instances"`
	EndpointIndex    map[string]string     `json:"endpoint_index"`
	Clusters         map[string]Cluster    `json:"clusters"`
	Observations     map[string]Report     `json:"observations"`
	Samples          map[string]Report     `json:"samples"`
	Operations       map[string]Operation  `json:"operations"`
	Policies         map[string]Policy     `json:"policies"`
	Exclusions       map[string]Exclusion  `json:"exclusions"`
	SwitchLocks      map[string]string     `json:"switch_locks"`
}

func emptyState() *State {
	return &State{
		Deployments: make(map[string]Deployment), Credentials: make(map[string]Credential),
		Agents: make(map[string]Agent), Instances: make(map[string]Instance),
		EndpointIndex: make(map[string]string), Clusters: make(map[string]Cluster),
		Observations: make(map[string]Report), Samples: make(map[string]Report),
		Operations: make(map[string]Operation), Policies: make(map[string]Policy),
		Exclusions:   make(map[string]Exclusion),
		SwitchLocks:  make(map[string]string),
		ModRevisions: make(map[string]int64),
	}
}
