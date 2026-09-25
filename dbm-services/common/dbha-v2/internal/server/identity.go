package server

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"net/http"
	"slices"
	"strconv"
	"time"

	"dbm-services/common/dbha-v2/pkg/discovery"
	"github.com/google/uuid"
)

type DeploymentRequest struct {
	ID                string   `json:"id"`
	IdempotencyKey    string   `json:"idempotency_key"`
	AllowedNetworks   []string `json:"allowed_networks"`
	CredentialProfile string   `json:"credential_profile"`
}

func newToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

func tokenHash(token string) string {
	h := sha256.Sum256([]byte(token))
	return hex.EncodeToString(h[:])
}

func equalToken(hash, token string) bool {
	a, err := hex.DecodeString(hash)
	if err != nil {
		return false
	}
	b := sha256.Sum256([]byte(token))
	return subtle.ConstantTimeCompare(a, b[:]) == 1
}

func (s *Store) CreateDeployment(ctx context.Context, req DeploymentRequest) (Deployment, error) {
	if req.IdempotencyKey == "" {
		return Deployment{}, apiError(http.StatusBadRequest, "IDEMPOTENCY_REQUIRED", "deployment idempotency key required")
	}
	if req.ID == "" {
		req.ID = uuid.NewString()
	}
	if _, err := uuid.Parse(req.ID); err != nil {
		return Deployment{}, apiError(http.StatusBadRequest, "INVALID_DEPLOYMENT_ID", "deployment id must be UUID")
	}
	var result Deployment
	err := s.Update(ctx, func(st *State) error {
		for _, d := range st.Deployments {
			if d.IdempotencyKey == req.IdempotencyKey {
				if d.CredentialProfile != req.CredentialProfile || !slices.Equal(d.AllowedNetworks, req.AllowedNetworks) {
					return apiError(http.StatusConflict, "IDEMPOTENCY_CONFLICT", "same idempotency key has different deployment settings")
				}
				result = d
				return nil
			}
		}
		if _, exists := st.Deployments[req.ID]; exists {
			return apiError(http.StatusConflict, "DEPLOYMENT_EXISTS", "deployment id already exists")
		}
		for {
			st.ClusterIDCounter++
			if _, used := st.Clusters[clusterKey(st.ClusterIDCounter)]; !used {
				break
			}
		}
		result = Deployment{ID: req.ID, ClusterID: st.ClusterIDCounter, IdempotencyKey: req.IdempotencyKey, MySQLBudget: 2, ProxyBudget: 2, AllowedNetworks: req.AllowedNetworks, CredentialProfile: req.CredentialProfile}
		st.Deployments[result.ID] = result
		st.Clusters[clusterKey(result.ClusterID)] = Cluster{ID: result.ClusterID, DeploymentID: result.ID, TopologyState: "DISCOVERING", HealthState: "UNKNOWN", OperationState: "IDLE", RecoveryGate: "NONE", ActiveStartPermits: make(map[string]StartPermit)}
		return nil
	})
	return result, err
}

func clusterKey(id uint64) string { return strconv.FormatUint(id, 10) }

func (s *Store) IssueAgent(ctx context.Context, deploymentID, agentID, kind string) (Agent, string, error) {
	if _, err := uuid.Parse(agentID); err != nil {
		return Agent{}, "", apiError(http.StatusBadRequest, "INVALID_AGENT_ID", "agent id must be UUID")
	}
	if kind != "mysql" && kind != "proxy" {
		return Agent{}, "", apiError(http.StatusBadRequest, "INVALID_KIND", "kind must be mysql or proxy")
	}
	token, err := newToken()
	if err != nil {
		return Agent{}, "", err
	}
	var result Agent
	err = s.Update(ctx, func(st *State) error {
		d, ok := st.Deployments[deploymentID]
		if !ok {
			return apiError(http.StatusNotFound, "DEPLOYMENT_NOT_FOUND", "deployment not found")
		}
		if _, ok := st.Agents[agentID]; ok {
			return apiError(http.StatusConflict, "AGENT_EXISTS", "agent exists; rotate its credential")
		}
		count := 0
		for _, a := range st.Agents {
			if a.DeploymentID == deploymentID && a.AllowedKind == kind {
				count++
			}
		}
		budget := d.ProxyBudget
		if kind == "mysql" {
			budget = d.MySQLBudget
		}
		if count >= budget {
			return apiError(http.StatusConflict, "MEMBER_BUDGET", "deployment member budget exhausted")
		}
		cid := "agent:" + agentID
		st.Credentials[cid] = Credential{ID: cid, AgentID: agentID, DeploymentID: deploymentID, AllowedKind: kind, TokenHash: tokenHash(token)}
		result = Agent{ID: agentID, DeploymentID: deploymentID, CredentialID: cid, AllowedKind: kind}
		st.Agents[agentID] = result
		return nil
	})
	if err != nil {
		return Agent{}, "", err
	}
	return result, token, nil
}

func (s *Store) RotateAgent(ctx context.Context, agentID string) (string, error) {
	token, err := newToken()
	if err != nil {
		return "", err
	}
	err = s.Update(ctx, func(st *State) error {
		a, ok := st.Agents[agentID]
		if !ok {
			return apiError(http.StatusNotFound, "AGENT_NOT_FOUND", "agent not found")
		}
		c := st.Credentials[a.CredentialID]
		c.TokenHash = tokenHash(token)
		c.Revoked = false
		st.Credentials[c.ID] = c
		// Rotation invalidates the old session and requires a new local boot generation.
		a.SessionClosed = true
		st.Agents[agentID] = a
		return nil
	})
	if err != nil {
		return "", err
	}
	return token, nil
}

func (s *Store) RevokeAgent(ctx context.Context, agentID string) error {
	return s.Update(ctx, func(st *State) error {
		a, ok := st.Agents[agentID]
		if !ok {
			return apiError(http.StatusNotFound, "AGENT_NOT_FOUND", "agent not found")
		}
		c := st.Credentials[a.CredentialID]
		c.Revoked = true
		st.Credentials[c.ID] = c
		a.SessionClosed = true
		st.Agents[agentID] = a
		return nil
	})
}

func (s *Store) InitAdmin(ctx context.Context, token string) error {
	if len(token) < 64 {
		return apiError(http.StatusBadRequest, "WEAK_ADMIN_TOKEN", "administrator token must contain at least 256 random bits")
	}
	return s.Update(ctx, func(st *State) error {
		if existing, ok := st.Credentials["admin"]; ok {
			if existing.Admin && !existing.Revoked && equalToken(existing.TokenHash, token) {
				return nil
			}
			return apiError(http.StatusConflict, "ADMIN_EXISTS", "administrator credential does not match initialized token")
		}
		st.Credentials["admin"] = Credential{ID: "admin", Admin: true, TokenHash: tokenHash(token)}
		return nil
	})
}

func (s *Store) AuthenticateAdmin(ctx context.Context, token string) error {
	st, err := s.Read(ctx)
	if err != nil {
		return err
	}
	c, ok := st.Credentials["admin"]
	if !ok || !c.Admin || c.Revoked || !equalToken(c.TokenHash, token) {
		return apiError(http.StatusUnauthorized, "UNAUTHORIZED", "invalid administrator credential")
	}
	return nil
}

func (s *Store) AuthenticateAgent(ctx context.Context, token string) (Agent, error) {
	st, err := s.Read(ctx)
	if err != nil {
		return Agent{}, err
	}
	for _, c := range st.Credentials {
		if c.Admin || c.Revoked || !equalToken(c.TokenHash, token) {
			continue
		}
		a, ok := st.Agents[c.AgentID]
		if ok && a.CredentialID == c.ID && a.DeploymentID == c.DeploymentID && a.AllowedKind == c.AllowedKind {
			return a, nil
		}
	}
	return Agent{}, apiError(http.StatusUnauthorized, "UNAUTHORIZED", "invalid agent credential")
}

func (s *Store) Register(ctx context.Context, token string, req discovery.RegisterRequest) (discovery.RegisterResponse, bool, error) {
	if req.SchemaVersion != discovery.SchemaVersion || req.BootGeneration == 0 || req.AgentID == "" {
		return discovery.RegisterResponse{}, false, apiError(http.StatusBadRequest, "INVALID_REGISTER", "invalid registration envelope")
	}
	if _, err := uuid.Parse(req.BootID); err != nil {
		return discovery.RegisterResponse{}, false, apiError(http.StatusBadRequest, "INVALID_BOOT_ID", "boot id must be UUID")
	}
	newSession := uuid.NewString()
	now := time.Now().UTC()
	var result discovery.RegisterResponse
	created := false
	err := s.Update(ctx, func(st *State) error {
		a, ok := st.Agents[req.AgentID]
		if !ok {
			return apiError(http.StatusUnauthorized, "UNAUTHORIZED", "unknown agent")
		}
		c := st.Credentials[a.CredentialID]
		if c.Revoked || !equalToken(c.TokenHash, token) {
			return apiError(http.StatusUnauthorized, "UNAUTHORIZED", "invalid agent credential")
		}
		if req.BootGeneration < a.BootGeneration {
			return apiError(http.StatusConflict, "BOOT_SUPERSEDED", "boot generation is stale")
		}
		if req.BootGeneration == a.BootGeneration && a.BootGeneration != 0 {
			if req.BootID != a.BootID {
				return apiError(http.StatusConflict, "BOOT_CONFLICT", "generation belongs to a different boot")
			}
			if a.SessionClosed {
				return apiError(http.StatusConflict, "SESSION_CLOSED", "closed boot cannot reopen")
			}
			result = discovery.RegisterResponse{SessionID: a.SessionID, SessionEpoch: a.SessionEpoch}
			created = false
			return nil
		}
		if a.SessionID != "" && !a.SessionClosed && now.Sub(a.LastContactAt) < 30*time.Second {
			return apiError(http.StatusConflict, "SESSION_ACTIVE", "previous session is active")
		}
		a.BootID = req.BootID
		a.BootGeneration = req.BootGeneration
		a.SessionID = newSession
		a.SessionEpoch++
		a.SessionClosed = false
		a.LastContactAt = now
		st.Agents[a.ID] = a
		result = discovery.RegisterResponse{SessionID: a.SessionID, SessionEpoch: a.SessionEpoch}
		created = true
		return nil
	})
	if err != nil {
		return discovery.RegisterResponse{}, false, err
	}
	return result, created, nil
}

func (s *Store) CloseSession(ctx context.Context, token, agentID, sessionID string) error {
	return s.Update(ctx, func(st *State) error {
		a, ok := st.Agents[agentID]
		if !ok {
			return apiError(http.StatusUnauthorized, "UNAUTHORIZED", "unknown agent")
		}
		c := st.Credentials[a.CredentialID]
		if c.Revoked || !equalToken(c.TokenHash, token) {
			return apiError(http.StatusUnauthorized, "UNAUTHORIZED", "invalid agent credential")
		}
		if a.SessionID != sessionID {
			return apiError(http.StatusConflict, "SESSION_SUPERSEDED", "session is not active")
		}
		if a.SessionClosed {
			return nil
		}
		a.SessionClosed = true
		st.Agents[agentID] = a
		return nil
	})
}
