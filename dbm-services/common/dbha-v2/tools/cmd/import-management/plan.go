package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"net/netip"
	"sort"
	"strconv"
	"strings"

	"dbm-services/common/dbha-v2/internal/server"
	"dbm-services/common/dbha-v2/pkg/discovery"
	"github.com/google/uuid"
)

type legacyInstance struct {
	ClusterID   int
	ClusterName string
	ClusterType string
	MachineType string
	AccessLayer string
	Role        string
	Status      string
	Host        string
	Port        int
	AdminPort   int
	BkBizID     int
	BkCloudID   int
}

type agentBinding struct {
	Kind      string `json:"kind"`
	Host      string `json:"host"`
	Port      int    `json:"port"`
	AgentID   string `json:"agent_id"`
	TokenFile string `json:"token_file"`
	ProxyUUID string `json:"proxy_uuid,omitempty"`
	TokenHash string `json:"-"`
}

type legacyPolicy struct {
	ID     string
	Status string
	Value  json.RawMessage
}

type legacyExclusion struct {
	Host   string
	Port   int
	Reason string
}

type importPlan struct {
	Deployments   map[string]server.Deployment
	Credentials   map[string]server.Credential
	Agents        map[string]server.Agent
	Instances     map[string]server.Instance
	EndpointIndex map[string]string
	Clusters      map[string]server.Cluster
	Policies      map[string]server.Policy
	Exclusions    map[string]server.Exclusion
	MaxClusterID  uint64
}

func endpoint(host string, port int) string         { return net.JoinHostPort(host, strconv.Itoa(port)) }
func bindingKey(kind, host string, port int) string { return kind + ":" + endpoint(host, port) }

func buildPlan(rows []legacyInstance, bindings []agentBinding, evidence map[string]discovery.Observation, policies []legacyPolicy, exclusions []legacyExclusion) (importPlan, error) {
	p := importPlan{Deployments: map[string]server.Deployment{}, Credentials: map[string]server.Credential{}, Agents: map[string]server.Agent{}, Instances: map[string]server.Instance{}, EndpointIndex: map[string]string{}, Clusters: map[string]server.Cluster{}, Policies: map[string]server.Policy{}, Exclusions: map[string]server.Exclusion{}}
	groups := map[int][]legacyInstance{}
	byEndpoint := map[string]agentBinding{}
	for _, b := range bindings {
		key := bindingKey(b.Kind, b.Host, b.Port)
		if _, ok := byEndpoint[key]; ok {
			return p, fmt.Errorf("duplicate agent binding %s", key)
		}
		if _, err := uuid.Parse(b.AgentID); err != nil {
			return p, fmt.Errorf("invalid agent_id for %s", key)
		}
		if b.Kind != "mysql" && b.Kind != "proxy" {
			return p, fmt.Errorf("invalid binding kind %s", b.Kind)
		}
		if b.Kind == "proxy" {
			if _, err := uuid.Parse(b.ProxyUUID); err != nil {
				return p, fmt.Errorf("invalid proxy_uuid for %s", key)
			}
		}
		if len(b.TokenHash) != 64 {
			return p, fmt.Errorf("missing 256-bit token hash for %s", key)
		}
		byEndpoint[key] = b
	}
	if len(rows) == 0 {
		return p, fmt.Errorf("legacy source has no rows")
	}
	seen := map[string]bool{}
	for _, r := range rows {
		if r.ClusterID <= 0 || r.Port <= 0 || r.Port > 65535 || r.Host == "" {
			return p, fmt.Errorf("invalid legacy cluster or endpoint")
		}
		if r.ClusterType != "tendbha" || (r.MachineType != "backend" && r.MachineType != "proxy") || (r.AccessLayer != "storage" && r.AccessLayer != "proxy") {
			return p, fmt.Errorf("unsupported legacy instance %s:%d", r.Host, r.Port)
		}
		if r.Status != "running" && r.Status != "available" {
			return p, fmt.Errorf("legacy instance %s:%d is not active", r.Host, r.Port)
		}
		kind := "mysql"
		if r.MachineType == "proxy" {
			kind = "proxy"
		}
		key := bindingKey(kind, r.Host, r.Port)
		if seen[key] {
			return p, fmt.Errorf("duplicate legacy endpoint %s", key)
		}
		seen[key] = true
		if _, ok := byEndpoint[key]; !ok {
			return p, fmt.Errorf("missing installer agent binding %s", key)
		}
		groups[r.ClusterID] = append(groups[r.ClusterID], r)
		if uint64(r.ClusterID) > p.MaxClusterID {
			p.MaxClusterID = uint64(r.ClusterID)
		}
	}
	if len(byEndpoint) != len(seen) {
		return p, fmt.Errorf("agent map contains endpoints absent from legacy metadata")
	}
	for clusterID, members := range groups {
		if len(members) != 4 {
			return p, fmt.Errorf("cluster %d requires two MySQL and two Proxy nodes", clusterID)
		}
		name := members[0].ClusterName
		if name == "" {
			return p, fmt.Errorf("cluster %d has no name", clusterID)
		}
		biz := members[0].BkBizID
		cloud := members[0].BkCloudID
		deploymentID := uuid.NewSHA1(uuid.NameSpaceOID, []byte(fmt.Sprintf("dbha-migration:%d:%d:%d", cloud, biz, clusterID))).String()
		cluster := server.Cluster{ID: uint64(clusterID), DeploymentID: deploymentID, TopologyEpoch: 1, TopologyState: "DATABASES_CONFIRMED", HealthState: "UNKNOWN", OperationState: "IDLE", RecoveryGate: "MIGRATION_UNVERIFIED", SwitchingEnabled: false, ActiveStartPermits: map[string]server.StartPermit{}, ReasonCodes: []string{"MIGRATION_UNVERIFIED"}}
		mysqlCount, proxyCount := 0, 0
		allowed := map[string]bool{}
		for _, r := range members {
			if r.ClusterName != name || r.BkBizID != biz || r.BkCloudID != cloud {
				return p, fmt.Errorf("cluster %d has inconsistent group identity", clusterID)
			}
			addr, err := netip.ParseAddr(r.Host)
			if err != nil {
				return p, fmt.Errorf("legacy advertised host %q is not an IP address", r.Host)
			}
			allowed[netip.PrefixFrom(addr, addr.BitLen()).String()] = true
			kind := "mysql"
			if r.MachineType == "proxy" {
				kind = "proxy"
			}
			key := bindingKey(kind, r.Host, r.Port)
			b := byEndpoint[key]
			id := ""
			if kind == "mysql" {
				mysqlCount++
				obs, ok := evidence[key]
				if !ok || obs.ServerUUID == "" || obs.CollectionState != "OK" || obs.ReplicationQueryState != "OK" || obs.AdvertiseHost != r.Host || obs.Port != uint32(r.Port) {
					return p, fmt.Errorf("missing valid MySQL identity evidence for %s", key)
				}
				if _, err := uuid.Parse(obs.ServerUUID); err != nil {
					return p, fmt.Errorf("invalid MySQL server_uuid for %s", key)
				}
				if obs.GTIDMode != "ON" || !supportedVersion(obs.Version) || len(obs.Channels) > 1 {
					return p, fmt.Errorf("unsupported MySQL topology for %s", key)
				}
				id = "mysql:" + obs.ServerUUID
				if r.Role == "backend_master" {
					if cluster.PrimaryID != "" || obs.ReadOnly || obs.SuperReadOnly || len(obs.Channels) != 0 {
						return p, fmt.Errorf("invalid primary role for %s", key)
					}
					cluster.PrimaryID = id
				} else if r.Role == "backend_slave" {
					if cluster.StandbyID != "" || !obs.ReadOnly || len(obs.Channels) != 1 || !obs.Channels[0].IORunning || !obs.Channels[0].SQLRunning || obs.Channels[0].SecondsBehindSource == nil || *obs.Channels[0].SecondsBehindSource < 0 || *obs.Channels[0].SecondsBehindSource > 30 {
						return p, fmt.Errorf("invalid standby role for %s", key)
					}
					cluster.StandbyID = id
				} else {
					return p, fmt.Errorf("unsupported role %q for %s", r.Role, key)
				}
			} else {
				proxyCount++
				if r.Role != "" {
					return p, fmt.Errorf("unexpected proxy role for %s", key)
				}
				id = "proxy:" + b.ProxyUUID
				cluster.ProxyIDs = append(cluster.ProxyIDs, id)
			}
			if _, ok := p.Instances[id]; ok {
				return p, fmt.Errorf("duplicate stable identity %s", id)
			}
			if _, ok := p.Agents[b.AgentID]; ok {
				return p, fmt.Errorf("duplicate agent_id %s", b.AgentID)
			}
			credID := "agent:" + b.AgentID
			p.Credentials[credID] = server.Credential{ID: credID, AgentID: b.AgentID, DeploymentID: deploymentID, AllowedKind: kind, TokenHash: b.TokenHash}
			p.Agents[b.AgentID] = server.Agent{ID: b.AgentID, DeploymentID: deploymentID, CredentialID: credID, AllowedKind: kind}
			p.Instances[id] = server.Instance{ID: id, AgentID: b.AgentID, DeploymentID: deploymentID, Kind: kind, Endpoint: endpoint(r.Host, r.Port), AdminPort: uint32(r.AdminPort), Admission: "RETURNED_UNVERIFIED"}
			p.EndpointIndex[key] = id
		}
		if mysqlCount != 2 || proxyCount != 2 || cluster.PrimaryID == "" || cluster.StandbyID == "" {
			return p, fmt.Errorf("cluster %d has invalid member roles", clusterID)
		}
		// Cross-check Source_UUID against the authoritative old primary UUID.
		primaryHost, primaryPortText, _ := net.SplitHostPort(p.Instances[cluster.PrimaryID].Endpoint)
		primaryPort, _ := strconv.Atoi(primaryPortText)
		for _, r := range members {
			if r.Role == "backend_slave" {
				obs := evidence[bindingKey("mysql", r.Host, r.Port)]
				if obs.Channels[0].SourceUUID != strings.TrimPrefix(cluster.PrimaryID, "mysql:") || obs.Channels[0].SourceHost != primaryHost || obs.Channels[0].SourcePort != uint32(primaryPort) {
					return p, fmt.Errorf("cluster %d standby source UUID disagrees with primary", clusterID)
				}
			}
		}
		var networks []string
		for value := range allowed {
			networks = append(networks, value)
		}
		sort.Strings(networks)
		p.Deployments[deploymentID] = server.Deployment{ID: deploymentID, ClusterID: uint64(clusterID), IdempotencyKey: "migration:" + deploymentID, MySQLBudget: 2, ProxyBudget: 2, AllowedNetworks: networks}
		p.Clusters[strconv.Itoa(clusterID)] = cluster
	}
	for _, policy := range policies {
		if policy.Status != "disabled" && policy.Status != "deleted" {
			return p, fmt.Errorf("active legacy policy %s cannot be represented safely", policy.ID)
		}
		id := "legacy:" + policy.ID
		p.Policies[id] = server.Policy{ID: id, Scope: "legacy-import-disabled", Version: 1, Value: policy.Value}
	}
	for _, x := range exclusions {
		id, ok := p.EndpointIndex[bindingKey("mysql", x.Host, x.Port)]
		if !ok {
			return p, fmt.Errorf("skip entry %s:%d cannot be mapped", x.Host, x.Port)
		}
		p.Exclusions[id] = server.Exclusion{InstanceID: id, Reason: x.Reason, Version: 1}
	}
	return p, nil
}

func tokenHash(token string) string {
	h := sha256.Sum256([]byte(token))
	return hex.EncodeToString(h[:])
}

func supportedVersion(version string) bool {
	if !strings.HasPrefix(version, "8.0.") || strings.Contains(strings.ToLower(version), "mariadb") {
		return false
	}
	patch := strings.Split(strings.TrimPrefix(version, "8.0."), "-")[0]
	n, err := strconv.Atoi(patch)
	return err == nil && n >= 22
}
