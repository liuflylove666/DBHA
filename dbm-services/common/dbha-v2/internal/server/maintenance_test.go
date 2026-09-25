package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"dbm-services/common/dbha-v2/pkg/discovery"
)

func maintenanceRequest(method, path, id, body string) *http.Request {
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	r.SetPathValue("id", id)
	return r
}

func TestPolicyRejectsScriptsAndConcurrentOverwrite(t *testing.T) {
	store, ctx := integrationStore(t)
	s := &Server{Store: store}
	bad := maintenanceRequest("PUT", "/api/v1/policies/custom", "custom", `{"name":"x","status":"enabled","trigger_event_name":"dbha_detect_db_failure","trigger_event_name_reason":"connection exception","trigger_count":3,"priority":0,"scope":"cluster","action":"switch","sql":"DROP TABLE x"}`)
	bad.Header.Set("If-None-Match", "*")
	_, err := s.policyResource(httptest.NewRecorder(), bad)
	expectCode(t, err, "INVALID_JSON")
	body := `{"name":"mysql-failure","status":"enabled","trigger_event_name":"dbha_detect_db_failure","trigger_event_name_reason":"connection exception","trigger_count":3,"priority":1,"scope":"cluster","action":"switch"}`
	create := maintenanceRequest("PUT", "/api/v1/policies/custom", "custom", body)
	create.Header.Set("If-None-Match", "*")
	got, err := s.policyResource(httptest.NewRecorder(), create)
	if err != nil || got.(Policy).Version != 1 {
		t.Fatalf("create: %+v %v", got, err)
	}
	stale := maintenanceRequest("PUT", "/api/v1/policies/custom", "custom", body)
	stale.Header.Set("If-Match", "2")
	_, err = s.policyResource(httptest.NewRecorder(), stale)
	expectCode(t, err, "VERSION_CONFLICT")
	st, err := store.Read(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var value PolicyValue
	if err = json.Unmarshal(st.Policies["custom"].Value, &value); err != nil || value.TriggerCount != 3 {
		t.Fatalf("policy changed after stale update: %+v %v", value, err)
	}
}

func TestRestoreReconcileRequiresLiveRouteAndClearsMarker(t *testing.T) {
	store, ctx := integrationStore(t)
	d, err := store.CreateDeployment(ctx, DeploymentRequest{IdempotencyKey: "restore-test"})
	if err != nil {
		t.Fatal(err)
	}
	primaryID, proxyID := "mysql:12345678-1234-1234-1234-123456789012", "proxy:p1"
	err = store.Update(ctx, func(st *State) error {
		c := st.Clusters[clusterKey(d.ClusterID)]
		c.PrimaryID = primaryID
		c.ProxyIDs = []string{proxyID}
		c.TopologyEpoch = 4
		c.TopologyState = "READY"
		c.RecoveryGate = "RESTORE_UNVERIFIED"
		c.SwitchingEnabled = false
		st.Clusters[clusterKey(c.ID)] = c
		st.Instances[primaryID] = Instance{ID: primaryID, Kind: "mysql", DeploymentID: d.ID, Endpoint: "127.0.0.1:3306", Admission: "ACTIVE"}
		st.Instances[proxyID] = Instance{ID: proxyID, Kind: "proxy", DeploymentID: d.ID, Endpoint: "127.0.0.2:3306", Admission: "ACTIVE"}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	marker := filepath.Join(dir, "restore-required")
	if err = os.WriteFile(marker, []byte("required"), 0600); err != nil {
		t.Fatal(err)
	}
	s := &Server{Store: store, Config: Config{StateDir: dir, Profiles: map[string]CredentialProfile{"default": {}}}}
	goodRoute := false
	s.verify = func(_ context.Context, i Instance, _ CredentialProfile) (observationResult, error) {
		if i.Kind == "mysql" {
			return discovery.Observation{Kind: "mysql", ServerUUID: strings.TrimPrefix(i.ID, "mysql:")}, nil
		}
		address := "127.0.0.9:3306"
		if goodRoute {
			address = "127.0.0.1:3306"
		}
		return discovery.Observation{Kind: "proxy", CollectionState: "OK", BackendQueryState: "OK", Backends: []discovery.Backend{{Address: address}}}, nil
	}
	body := `{"gate":"RESTORE_UNVERIFIED","clusters":[{"cluster_id":1,"expected_epoch":4}]}`
	r := httptest.NewRequest("POST", "/api/v1/recovery/reconcile", strings.NewReader(body))
	_, err = s.recoverClusters(httptest.NewRecorder(), r)
	expectCode(t, err, "RECOVERY_EVIDENCE")
	if _, err = os.Stat(marker); err != nil {
		t.Fatal("marker removed after failed verification")
	}
	st, err := store.Read(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if st.Clusters[clusterKey(d.ClusterID)].RecoveryGate != "RESTORE_UNVERIFIED" {
		t.Fatal("recovery gate cleared without matching proxy")
	}
	goodRoute = true
	r = httptest.NewRequest("POST", "/api/v1/recovery/reconcile", strings.NewReader(body))
	_, err = s.recoverClusters(httptest.NewRecorder(), r)
	if err != nil {
		t.Fatal(err)
	}
	st, err = store.Read(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if st.Clusters[clusterKey(d.ClusterID)].RecoveryGate != "NONE" || st.Clusters[clusterKey(d.ClusterID)].SwitchingEnabled {
		t.Fatal("recovery gate or switching state wrong")
	}
	if _, err = os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("restore marker remains: %v", err)
	}
}
