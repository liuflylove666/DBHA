package server

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"

	"dbm-services/common/dbha-v2/internal/analysis/dbm"
)

func TestMetadataUsesExistingClientContractWithoutInventedStandby(t *testing.T) {
	store, ctx := integrationStore(t)
	s, fixture, c := topologyFixture()
	s.Store = store
	c.StandbyID = ""
	fixture.Clusters["1"] = c
	persistTopologyFixture(t, store, ctx, fixture)
	token, err := newToken()
	if err != nil {
		t.Fatal(err)
	}
	if err := store.InitAdmin(ctx, token); err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest("POST", "/api/v1/metadata", strings.NewReader(`{"bk_cloud_id":0,"cluster_types":["tendbha"]}`)).WithContext(ctx)
	r.Header.Set("Authorization", "Bearer "+token)
	w := httptest.NewRecorder()
	s.metadataCompatibility(w, r)
	var body struct {
		Code int                  `json:"code"`
		Data []dbm.DbInstMetadata `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if w.Code != 200 || body.Code != 0 || len(body.Data) != 3 {
		t.Fatalf("unexpected metadata: %s", w.Body.String())
	}
	for _, row := range body.Data {
		if row.InstanceRole == "backend_master" && (len(row.Receiver) != 0 || len(row.ProxyInstanceSet) != 2) {
			t.Fatalf("invalid committed topology: %+v", row)
		}
	}
	if err := store.Update(ctx, func(st *State) error {
		v := st.Clusters["1"]
		v.RecoveryGate = "RESTORE_UNVERIFIED"
		st.Clusters["1"] = v
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	r = httptest.NewRequest("POST", "/api/v1/metadata", strings.NewReader(`{}`)).WithContext(ctx)
	r.Header.Set("Authorization", "Bearer "+token)
	w = httptest.NewRecorder()
	s.metadataCompatibility(w, r)
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if len(body.Data) != 0 {
		t.Fatal("unverified restored topology leaked through compatibility API")
	}
}
