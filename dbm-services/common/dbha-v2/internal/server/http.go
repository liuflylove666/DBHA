package server

import (
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"dbm-services/common/dbha-v2/pkg/discovery"
	"github.com/google/uuid"
)

type endpointHandler func(http.ResponseWriter, *http.Request) (any, error)

func bearer(r *http.Request) string {
	value := r.Header.Get("Authorization")
	if !strings.HasPrefix(value, "Bearer ") {
		return ""
	}
	return strings.TrimPrefix(value, "Bearer ")
}

func decode(r *http.Request, v any) error {
	d := json.NewDecoder(r.Body)
	d.DisallowUnknownFields()
	if err := d.Decode(v); err != nil {
		return apiError(400, "INVALID_JSON", "invalid request fields or JSON")
	}
	if err := d.Decode(new(any)); err != io.EOF {
		return apiError(400, "INVALID_JSON", "one JSON object required")
	}
	return nil
}

func (s *Server) wrap(admin bool, h endpointHandler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		r.Body = http.MaxBytesReader(w, r.Body, 256<<10)
		requestID := uuid.NewString()
		w.Header().Set("X-Request-ID", requestID)
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		var value any
		var err error
		if admin {
			err = s.Store.AuthenticateAdmin(r.Context(), bearer(r))
		}
		if err == nil {
			value, err = h(w, r)
		}
		s.observeHTTP(r.URL.Path, err)
		if err != nil {
			status := 503
			code, message := "INTERNAL_ERROR", "operation could not be completed"
			var e *Error
			if errors.As(err, &e) {
				status, code, message = e.Status, e.Code, e.Message
			}
			w.WriteHeader(status)
			_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{"code": code, "message": message, "retryable": status >= 500}, "request_id": requestID})
		} else {
			revision := ""
			if st, e := s.Store.Read(r.Context()); e == nil {
				revision = strconv.FormatInt(st.Revision, 10)
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"data": value, "revision": revision})
		}
		if admin && r.Method != "GET" && s.log != nil {
			s.log.Info("management action", "identity", "administrator", "method", r.Method, "path", r.URL.Path, "request_id", requestID, "success", err == nil)
		}
	}
}

func (s *Server) Handler() http.Handler {
	s.handlerOnce.Do(func() { s.handler = s.newHandler() })
	return s.handler
}

func (s *Server) newHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200); _, _ = w.Write([]byte("ok\n")) })
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, r *http.Request) {
		if s.Store.Ready(r.Context()) != nil {
			http.Error(w, "storage or ownership unavailable", 503)
			return
		}
		_, _ = w.Write([]byte("ready\n"))
	})
	mux.HandleFunc("GET /metrics", s.serveMetrics)
	mux.HandleFunc("POST /api/v1/deployments", s.wrap(true, func(w http.ResponseWriter, r *http.Request) (any, error) {
		var req DeploymentRequest
		if err := decode(r, &req); err != nil {
			return nil, err
		}
		req.IdempotencyKey = r.Header.Get("Idempotency-Key")
		if len(req.AllowedNetworks) == 0 {
			req.AllowedNetworks = s.Config.AllowedNetworks
		}
		for _, cidr := range req.AllowedNetworks {
			ip, network, err := net.ParseCIDR(cidr)
			if err != nil {
				return nil, apiError(400, "INVALID_NETWORK", "invalid CIDR")
			}
			within := false
			for _, allowed := range s.Config.AllowedNetworks {
				_, a, _ := net.ParseCIDR(allowed)
				ones, bits := network.Mask.Size()
				aones, abits := a.Mask.Size()
				if a.Contains(ip) && bits == abits && ones >= aones {
					within = true
				}
			}
			if !within {
				return nil, apiError(403, "INVALID_NETWORK", "deployment network exceeds server allowlist")
			}
		}
		if req.CredentialProfile == "" {
			req.CredentialProfile = "default"
		}
		if _, ok := s.Config.Profiles[req.CredentialProfile]; !ok {
			return nil, apiError(400, "INVALID_PROFILE", "unknown credential profile")
		}
		return s.Store.CreateDeployment(r.Context(), req)
	}))
	mux.HandleFunc("POST /api/v1/deployments/{id}/agents", s.wrap(true, func(w http.ResponseWriter, r *http.Request) (any, error) {
		var req struct {
			AgentID string `json:"agent_id"`
			Kind    string `json:"kind"`
		}
		if e := decode(r, &req); e != nil {
			return nil, e
		}
		a, t, e := s.Store.IssueAgent(r.Context(), r.PathValue("id"), req.AgentID, req.Kind)
		return map[string]any{"agent_id": a.ID, "token": t}, e
	}))
	mux.HandleFunc("POST /api/v1/agents/{id}/credentials/rotate", s.wrap(true, func(w http.ResponseWriter, r *http.Request) (any, error) {
		t, e := s.Store.RotateAgent(r.Context(), r.PathValue("id"))
		return map[string]any{"agent_id": r.PathValue("id"), "token": t}, e
	}))
	mux.HandleFunc("POST /api/v1/agents/{id}/credentials/revoke", s.wrap(true, func(w http.ResponseWriter, r *http.Request) (any, error) {
		return map[string]bool{"revoked": true}, s.Store.RevokeAgent(r.Context(), r.PathValue("id"))
	}))
	mux.HandleFunc("POST /api/v1/agents/register", s.wrap(false, func(w http.ResponseWriter, r *http.Request) (any, error) {
		var req discovery.RegisterRequest
		if e := decode(r, &req); e != nil {
			return nil, e
		}
		v, created, e := s.Store.Register(r.Context(), bearer(r), req)
		if e == nil && created {
			w.WriteHeader(http.StatusCreated)
		}
		return v, e
	}))
	mux.HandleFunc("POST /api/v1/agents/session-close", s.wrap(false, func(w http.ResponseWriter, r *http.Request) (any, error) {
		var req struct {
			AgentID   string `json:"agent_id"`
			SessionID string `json:"session_id"`
		}
		if e := decode(r, &req); e != nil {
			return nil, e
		}
		return map[string]bool{"closed": true}, s.Store.CloseSession(r.Context(), bearer(r), req.AgentID, req.SessionID)
	}))
	mux.HandleFunc("GET /api/v1/clusters", s.wrap(true, s.listClusters))
	mux.HandleFunc("GET /api/v1/clusters/{id}", s.wrap(true, s.getCluster))
	mux.HandleFunc("GET /api/v1/clusters/{id}/route", s.wrap(false, s.getRoute))
	mux.HandleFunc("POST /api/v1/proxies/{id}/start-permits", s.wrap(false, s.createPermit))
	mux.HandleFunc("GET /api/v1/proxies/{id}/start-permits/{permit}", s.wrap(false, s.getPermit))
	mux.HandleFunc("POST /api/v1/proxies/{id}/start-permits/{permit}/complete", s.wrap(false, s.completePermit))
	mux.HandleFunc("PUT /api/v1/clusters/{id}/switching", s.wrap(true, s.clusterFlag))
	mux.HandleFunc("PUT /api/v1/clusters/{id}/maintenance", s.wrap(true, s.clusterFlag))
	mux.HandleFunc("POST /api/v1/instances/{id}/retire", s.wrap(true, s.retireInstance))
	mux.HandleFunc("POST /api/v1/instances/{id}/reconcile", s.wrap(true, s.reconcileInstance))
	mux.HandleFunc("GET /api/v1/operations/{id}", s.wrap(true, func(w http.ResponseWriter, r *http.Request) (any, error) {
		st, e := s.Store.Read(r.Context())
		if e != nil {
			return nil, e
		}
		v, ok := st.Operations[r.PathValue("id")]
		if !ok {
			return nil, apiError(404, "OPERATION_NOT_FOUND", "operation unknown or summary expired")
		}
		return v, nil
	}))
	mux.HandleFunc("POST /api/v1/operations/{id}/resolve", s.wrap(true, func(w http.ResponseWriter, r *http.Request) (any, error) {
		var req struct {
			Resolution    string  `json:"resolution"`
			ExpectedEpoch *uint64 `json:"expected_epoch"`
		}
		if e := decode(r, &req); e != nil {
			return nil, e
		}
		if req.ExpectedEpoch == nil {
			return nil, apiError(428, "EPOCH_REQUIRED", "expected_epoch required")
		}
		return map[string]bool{"resolved": true}, s.ResolveOperation(r.Context(), r.PathValue("id"), req.Resolution, *req.ExpectedEpoch)
	}))
	mux.HandleFunc("POST /api/v1/recovery/reconcile", s.wrap(true, s.recoverClusters))
	for _, path := range []string{"/api/v1/policies", "/api/v1/policies/{id}", "/api/v1/exclusions", "/api/v1/exclusions/{id}"} {
		mux.HandleFunc(path, s.wrap(true, s.policyResource))
	}
	mux.HandleFunc("POST /api/v1/metadata", s.metadataCompatibility)
	for _, path := range []string{"/api/v1/update-status", "/api/v1/swap-mysql-role"} {
		mux.HandleFunc("POST "+path, s.wrap(true, func(w http.ResponseWriter, r *http.Request) (any, error) {
			return nil, apiError(412, "OPERATION_CONTEXT_REQUIRED", "use verified operation resolution; arbitrary role mutation is disabled")
		}))
	}
	return mux
}

func (s *Server) listClusters(w http.ResponseWriter, r *http.Request) (any, error) {
	st, e := s.Store.Read(r.Context())
	if e != nil {
		return nil, e
	}
	limit := 50
	if v := r.URL.Query().Get("limit"); v != "" {
		limit, e = strconv.Atoi(v)
		if e != nil || limit < 1 || limit > 100 {
			return nil, apiError(400, "INVALID_LIMIT", "limit must be 1..100")
		}
	}
	keys := []string{}
	for id := range st.Clusters {
		keys = append(keys, id)
	}
	sort.Strings(keys)
	items := []Cluster{}
	next := ""
	cursor := r.URL.Query().Get("cursor")
	for _, id := range keys {
		if id <= cursor {
			continue
		}
		if len(items) == limit {
			next = clusterKey(items[len(items)-1].ID)
			break
		}
		items = append(items, st.Clusters[id])
	}
	return map[string]any{"items": items, "next_cursor": next}, nil
}

func (s *Server) getCluster(w http.ResponseWriter, r *http.Request) (any, error) {
	st, e := s.Store.Read(r.Context())
	if e != nil {
		return nil, e
	}
	c, ok := st.Clusters[r.PathValue("id")]
	if !ok {
		return nil, apiError(404, "CLUSTER_NOT_FOUND", "cluster not found")
	}
	items := []map[string]any{}
	for _, i := range st.Instances {
		if i.DeploymentID == c.DeploymentID {
			items = append(items, map[string]any{"instance": i, "observation": st.Observations[i.ID], "observation_age_seconds": time.Since(st.Observations[i.ID].ReceivedAt).Seconds()})
		}
	}
	return map[string]any{"cluster": c, "instances": items, "ready_for_switch": s.switchReady(st, c)}, nil
}

func (s *Server) proxyContext(r *http.Request) (*State, Agent, Cluster, error) {
	a, e := s.Store.AuthenticateAgent(r.Context(), bearer(r))
	if e != nil {
		return nil, a, Cluster{}, e
	}
	if a.AllowedKind != "proxy" {
		return nil, a, Cluster{}, apiError(403, "FORBIDDEN", "proxy identity required")
	}
	st, e := s.Store.Read(r.Context())
	if e != nil {
		return nil, a, Cluster{}, e
	}
	c := st.Clusters[clusterKey(st.Deployments[a.DeploymentID].ClusterID)]
	if id := r.PathValue("id"); strings.Contains(r.URL.Path, "/proxies/") {
		if i, ok := st.Instances[id]; !ok || i.AgentID != a.ID {
			return nil, a, c, apiError(403, "FORBIDDEN", "proxy identity mismatch")
		}
	} else if id != clusterKey(c.ID) {
		return nil, a, c, apiError(403, "FORBIDDEN", "route outside deployment")
	}
	return st, a, c, nil
}

func (s *Server) getRoute(w http.ResponseWriter, r *http.Request) (any, error) {
	st, _, c, e := s.proxyContext(r)
	if e != nil {
		return nil, e
	}
	if e = s.routeReady(r.Context(), st, c); e != nil {
		return nil, e
	}
	return routeData(st, c), nil
}

func (s *Server) createPermit(w http.ResponseWriter, r *http.Request) (any, error) {
	var req struct {
		RequestID string `json:"request_id"`
	}
	if e := decode(r, &req); e != nil {
		return nil, e
	}
	if _, e := uuid.Parse(req.RequestID); e != nil {
		return nil, apiError(400, "REQUEST_ID_REQUIRED", "UUID request_id required")
	}
	st, a, c, e := s.proxyContext(r)
	if e != nil {
		return nil, e
	}
	if e = s.routeReady(r.Context(), st, c); e != nil {
		return nil, e
	}
	permit := StartPermit{ID: uuid.NewString(), ProxyID: r.PathValue("id"), RequestID: req.RequestID, Epoch: c.TopologyEpoch, ExpiresAt: time.Now().UTC().Add(time.Minute)}
	e = s.Store.Update(r.Context(), func(current *State) error {
		cred := current.Credentials[current.Agents[a.ID].CredentialID]
		if cred.Revoked || !equalToken(cred.TokenHash, bearer(r)) {
			return apiError(401, "UNAUTHORIZED", "credential revoked")
		}
		v := current.Clusters[clusterKey(c.ID)]
		if v.TopologyEpoch != c.TopologyEpoch || v.OperationState != "IDLE" || v.ActiveOperationID != "" {
			return apiError(409, "OPERATION_ACTIVE", "route changed or operation active")
		}
		if v.RecoveryGate != c.RecoveryGate || v.TopologyState != c.TopologyState || v.PrimaryID != c.PrimaryID || !s.freshInstance(current, v.PrimaryID) || !evidenceUnchanged(st, current, []string{c.PrimaryID, permit.ProxyID}) {
			return apiError(503, "ROUTE_NOT_READY", "route evidence or recovery gate changed")
		}
		if v.ActiveStartPermits == nil {
			v.ActiveStartPermits = map[string]StartPermit{}
		}
		for _, p := range v.ActiveStartPermits {
			if p.ProxyID == permit.ProxyID {
				if p.RequestID != req.RequestID {
					return apiError(409, "PERMIT_ACTIVE", "complete or cancel the existing permit")
				}
				permit = p
				return nil
			}
		}
		v.ActiveStartPermits[permit.ID] = permit
		current.Clusters[clusterKey(v.ID)] = v
		return nil
	})
	if e != nil {
		return nil, e
	}
	data := routeData(st, c)
	data["permit_id"] = permit.ID
	data["proxy_instance_id"] = permit.ProxyID
	data["expires_at"] = permit.ExpiresAt
	return data, nil
}

func (s *Server) getPermit(w http.ResponseWriter, r *http.Request) (any, error) {
	st, _, c, e := s.proxyContext(r)
	if e != nil {
		return nil, e
	}
	p, ok := c.ActiveStartPermits[r.PathValue("permit")]
	if !ok || p.ProxyID != r.PathValue("id") {
		return nil, apiError(404, "PERMIT_NOT_FOUND", "permit not found")
	}
	if time.Now().After(p.ExpiresAt) || p.Epoch != c.TopologyEpoch {
		return nil, apiError(412, "START_PERMIT_EXPIRED", "permit expired or route changed")
	}
	if e = s.routeReady(r.Context(), st, c); e != nil {
		return nil, e
	}
	data := routeData(st, c)
	data["permit_id"] = p.ID
	data["expires_at"] = p.ExpiresAt
	return data, nil
}

func (s *Server) completePermit(w http.ResponseWriter, r *http.Request) (any, error) {
	var req struct {
		Stopped bool `json:"stopped"`
	}
	if e := decode(r, &req); e != nil {
		return nil, e
	}
	st, _, c, e := s.proxyContext(r)
	if e != nil {
		return nil, e
	}
	p, ok := c.ActiveStartPermits[r.PathValue("permit")]
	if !ok {
		return map[string]bool{"complete": true}, nil
	}
	if p.ProxyID != r.PathValue("id") {
		return nil, apiError(403, "FORBIDDEN", "permit mismatch")
	}
	o, inspectErr := s.verify(r.Context(), st.Instances[p.ProxyID], s.profile(st, c.DeploymentID))
	if req.Stopped {
		// A connection refusal alone is not proof of a stopped remote process.
		// Require a new authenticated STARTING observation and SSH health proof.
		rpt := st.Observations[p.ProxyID]
		obs := observation(st, p.ProxyID)
		if inspectErr == nil || !networkFailure(inspectErr) || obs.ProcessState != "STARTING" || rpt.ReceivedAt.Before(p.ExpiresAt.Add(-time.Minute)) || time.Since(rpt.ReceivedAt) > 15*time.Second {
			return nil, apiError(412, "STOP_NOT_VERIFIED", "fresh STARTING observation and unreachable proxy required")
		}
		if err := s.verifyProxyStopped(r.Context(), st.Instances[p.ProxyID], s.profile(st, c.DeploymentID)); err != nil {
			return nil, apiError(412, "STOP_NOT_VERIFIED", "proxy process stop could not be independently verified")
		}
	} else if inspectErr != nil || !sameBackend(o, st.Instances[c.PrimaryID].Endpoint) {
		return nil, apiError(412, "ROUTE_NOT_VERIFIED", "proxy backend verification failed")
	}
	e = s.Store.Update(r.Context(), func(current *State) error {
		v := current.Clusters[clusterKey(c.ID)]
		if v.TopologyEpoch != p.Epoch || v.OperationState != "IDLE" {
			return apiError(409, "VERSION_CONFLICT", "route changed")
		}
		delete(v.ActiveStartPermits, p.ID)
		current.Clusters[clusterKey(v.ID)] = v
		return nil
	})
	return map[string]bool{"complete": e == nil}, e
}

func (s *Server) clusterFlag(w http.ResponseWriter, r *http.Request) (any, error) {
	var req struct {
		ExpectedEpoch *uint64 `json:"expected_epoch"`
		Enabled       bool    `json:"enabled"`
		Reason        string  `json:"reason"`
	}
	if e := decode(r, &req); e != nil {
		return nil, e
	}
	if req.ExpectedEpoch == nil {
		return nil, apiError(428, "EPOCH_REQUIRED", "expected_epoch required")
	}
	var result Cluster
	e := s.Store.Update(r.Context(), func(st *State) error {
		c, ok := st.Clusters[r.PathValue("id")]
		if !ok {
			return apiError(404, "CLUSTER_NOT_FOUND", "cluster not found")
		}
		if c.TopologyEpoch != *req.ExpectedEpoch {
			return apiError(409, "VERSION_CONFLICT", "epoch changed")
		}
		if strings.HasSuffix(r.URL.Path, "/maintenance") {
			if req.Enabled && req.Reason == "" {
				return apiError(400, "REASON_REQUIRED", "maintenance reason required")
			}
			c.Maintenance = req.Enabled
		} else {
			if req.Enabled {
				candidate := c
				candidate.SwitchingEnabled = true
				if !s.switchReady(st, candidate) || c.HealthState != "HEALTHY" {
					return apiError(412, "NOT_READY", "healthy verified topology required")
				}
			}
			c.SwitchingEnabled = req.Enabled
		}
		st.Clusters[clusterKey(c.ID)] = c
		result = c
		return nil
	})
	return result, e
}
