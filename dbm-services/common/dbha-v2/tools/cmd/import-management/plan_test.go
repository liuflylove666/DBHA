package main

import (
	"encoding/json"
	"testing"

	"dbm-services/common/dbha-v2/pkg/discovery"
)

func fixture() ([]legacyInstance, []agentBinding, map[string]discovery.Observation) {
	lag := int64(2)
	primaryUUID := "11111111-1111-4111-8111-111111111111"
	standbyUUID := "22222222-2222-4222-8222-222222222222"
	base := legacyInstance{ClusterID: 101, ClusterName: "research", ClusterType: "tendbha", Status: "running", BkBizID: 1}
	rows := []legacyInstance{
		base, base, base, base,
	}
	rows[0].Host, rows[0].Port, rows[0].MachineType, rows[0].AccessLayer, rows[0].Role = "10.0.0.1", 3306, "backend", "storage", "backend_master"
	rows[1].Host, rows[1].Port, rows[1].MachineType, rows[1].AccessLayer, rows[1].Role = "10.0.0.2", 3306, "backend", "storage", "backend_slave"
	rows[2].Host, rows[2].Port, rows[2].MachineType, rows[2].AccessLayer = "10.0.0.3", 10000, "proxy", "proxy"
	rows[3].Host, rows[3].Port, rows[3].MachineType, rows[3].AccessLayer = "10.0.0.4", 10000, "proxy", "proxy"
	bindings := []agentBinding{
		{Kind: "mysql", Host: "10.0.0.1", Port: 3306, AgentID: "aaaaaaa1-aaaa-4aaa-8aaa-aaaaaaaaaaa1", TokenHash: tokenHash("first")},
		{Kind: "mysql", Host: "10.0.0.2", Port: 3306, AgentID: "aaaaaaa2-aaaa-4aaa-8aaa-aaaaaaaaaaa2", TokenHash: tokenHash("second")},
		{Kind: "proxy", Host: "10.0.0.3", Port: 10000, AgentID: "aaaaaaa3-aaaa-4aaa-8aaa-aaaaaaaaaaa3", ProxyUUID: "bbbbbbb3-bbbb-4bbb-8bbb-bbbbbbbbbbb3", TokenHash: tokenHash("third")},
		{Kind: "proxy", Host: "10.0.0.4", Port: 10000, AgentID: "aaaaaaa4-aaaa-4aaa-8aaa-aaaaaaaaaaa4", ProxyUUID: "bbbbbbb4-bbbb-4bbb-8bbb-bbbbbbbbbbb4", TokenHash: tokenHash("fourth")},
	}
	evidence := map[string]discovery.Observation{
		bindingKey("mysql", "10.0.0.1", 3306): {Kind: "mysql", AdvertiseHost: "10.0.0.1", Port: 3306, ServerUUID: primaryUUID, Version: "8.0.41", GTIDMode: "ON", CollectionState: "OK", ReplicationQueryState: "OK"},
		bindingKey("mysql", "10.0.0.2", 3306): {Kind: "mysql", AdvertiseHost: "10.0.0.2", Port: 3306, ServerUUID: standbyUUID, Version: "8.0.41", GTIDMode: "ON", ReadOnly: true, CollectionState: "OK", ReplicationQueryState: "OK", Channels: []discovery.Channel{{SourceUUID: primaryUUID, SourceHost: "10.0.0.1", SourcePort: 3306, IORunning: true, SQLRunning: true, SecondsBehindSource: &lag}}},
	}
	return rows, bindings, evidence
}

func TestBuildPlanPreservesRolesAndGate(t *testing.T) {
	rows, bindings, evidence := fixture()
	p, err := buildPlan(rows, bindings, evidence, nil, []legacyExclusion{{Host: "10.0.0.2", Port: 3306, Reason: "legacy skip"}})
	if err != nil {
		t.Fatal(err)
	}
	c := p.Clusters["101"]
	if c.PrimaryID != "mysql:11111111-1111-4111-8111-111111111111" || c.StandbyID != "mysql:22222222-2222-4222-8222-222222222222" || c.RecoveryGate != "MIGRATION_UNVERIFIED" || c.SwitchingEnabled || p.MaxClusterID != 101 {
		t.Fatalf("unsafe migration cluster: %+v", c)
	}
	if len(p.Agents) != 4 || len(p.Credentials) != 4 || len(p.Exclusions) != 1 || len(p.EndpointIndex) != 4 {
		t.Fatal("migration relationships incomplete")
	}
	if p.Instances[c.PrimaryID].Admission != "RETURNED_UNVERIFIED" {
		t.Fatal("imported instance prematurely active")
	}
	if p.Credentials["agent:"+bindings[0].AgentID].TokenHash != bindings[0].TokenHash {
		t.Fatal("installer token did not bind to imported agent")
	}
}

func TestBuildPlanFailsClosedOnConflictingEvidence(t *testing.T) {
	rows, bindings, evidence := fixture()
	obs := evidence[bindingKey("mysql", "10.0.0.2", 3306)]
	obs.Channels[0].SourceUUID = "33333333-3333-4333-8333-333333333333"
	evidence[bindingKey("mysql", "10.0.0.2", 3306)] = obs
	if _, err := buildPlan(rows, bindings, evidence, nil, nil); err == nil {
		t.Fatal("cross-group source accepted")
	}
	delete(evidence, bindingKey("mysql", "10.0.0.2", 3306))
	if _, err := buildPlan(rows, bindings, evidence, nil, nil); err == nil {
		t.Fatal("missing MySQL identity accepted")
	}
}

func TestBuildPlanRejectsActiveLegacyWhitelist(t *testing.T) {
	rows, bindings, evidence := fixture()
	policies := []legacyPolicy{{ID: "whitelist:1", Status: "enabled", Value: json.RawMessage(`{"switch_version":"v1"}`)}}
	if _, err := buildPlan(rows, bindings, evidence, policies, nil); err == nil {
		t.Fatal("active whitelist silently imported")
	}
}
