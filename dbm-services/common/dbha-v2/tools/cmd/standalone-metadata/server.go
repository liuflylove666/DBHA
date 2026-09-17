package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"
)

type store interface {
	PingContext(context.Context) error
	queryMetadata(context.Context, metadataRequest) ([]*instanceMetadata, error)
	updateStatus(context.Context, updateStatusRequest) error
	swapRole(context.Context, swapRoleRequest) error
}

type sqlStore struct{ db *sql.DB }

type metadataRequest struct {
	BkCloudID      int      `json:"bk_cloud_id"`
	Token          string   `json:"db_cloud_token"`
	Addresses      []string `json:"addresses"`
	LogicalCityIDs []string `json:"logical_city_ids"`
	Statuses       []string `json:"statuses"`
	ClusterTypes   []string `json:"cluster_types"`
	HashCnt        int      `json:"hash_cnt"`
	HashValue      int      `json:"hash_value"`
	MachineOnly    bool     `json:"machine_only"`
}

type updateStatusRequest struct {
	BkCloudID int    `json:"bk_cloud_id"`
	Token     string `json:"db_cloud_token"`
	Payloads  []struct {
		IP     string `json:"ip"`
		Port   int    `json:"port"`
		Status string `json:"status"`
	} `json:"payloads"`
}

type swapRoleRequest struct {
	BkCloudID int           `json:"bk_cloud_id"`
	Token     string        `json:"db_cloud_token"`
	Payloads  []swapPayload `json:"payloads"`
}

type swapPayload struct {
	Instance1 endpoint `json:"instance1"`
	Instance2 endpoint `json:"instance2"`
}

type endpoint struct {
	IP   string `json:"ip"`
	Port int    `json:"port"`
}

// instanceMetadata mirrors the JSON contract consumed by internal/analysis/dbm.DbInstMetadata.
// Keeping the wire struct local prevents this tiny binary from importing the Analysis service.
type instanceMetadata struct {
	BkCloudID       int            `json:"bk_cloud_id"`
	BkBizID         int            `json:"bk_biz_id"`
	LogicalCityID   int            `json:"logical_city_id"`
	LogicalCityName string         `json:"logical_city_name"`
	ClusterID       int            `json:"cluster_id"`
	Cluster         string         `json:"cluster"`
	ClusterType     string         `json:"cluster_type"`
	MachineType     string         `json:"machine_type"`
	AccessLayer     string         `json:"access_layer"`
	InstanceRole    string         `json:"instance_role"`
	Status          string         `json:"status"`
	IP              string         `json:"ip"`
	Port            int            `json:"port"`
	AdminPort       int            `json:"admin_port"`
	IsStandBy       bool           `json:"is_stand_by"`
	Receiver        []slaveInfo    `json:"receiver,omitempty"`
	ProxyInstances  []proxyInfo    `json:"proxyinstance_set,omitempty"`
	BindEntry       map[string]any `json:"bind_entry"`
}

type slaveInfo struct {
	IP        string `json:"ip"`
	Port      int    `json:"port"`
	IsStandBy bool   `json:"is_stand_by"`
	Status    string `json:"status"`
}
type proxyInfo struct {
	IP        string `json:"ip"`
	Port      int    `json:"port"`
	AdminPort int    `json:"admin_port"`
	Status    string `json:"status"`
}
type api struct {
	store store
	token string
}

func newAPI(db *sql.DB, token string) http.Handler { return newAPIWithStore(&sqlStore{db: db}, token) }

func newAPIWithStore(s store, token string) http.Handler {
	a := &api{store: s, token: token}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", a.health)
	mux.HandleFunc("POST /api/v1/metadata", a.metadata)
	mux.HandleFunc("POST /api/v1/update-status", a.updateStatus)
	mux.HandleFunc("POST /api/v1/swap-mysql-role", a.swapRole)
	for _, path := range []string{"/api/v1/domain", "/api/v1/domain/delete", "/api/v1/clb/deregister", "/api/v1/polaris/unbind", "/api/v1/dumper/switch", "/api/v1/swap-tendis"} {
		mux.HandleFunc(path, a.unsupported)
	}
	return mux
}

func (a *api) health(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()
	if err := a.store.PingContext(ctx); err != nil {
		writeError(w, http.StatusServiceUnavailable, "database unavailable")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (a *api) metadata(w http.ResponseWriter, r *http.Request) {
	var req metadataRequest
	if !a.decode(w, r, &req, &req.Token) {
		return
	}
	if req.HashCnt < 0 || req.HashValue < 0 || (req.HashCnt > 0 && req.HashValue >= req.HashCnt) {
		writeError(w, http.StatusBadRequest, "invalid hash shard")
		return
	}
	data, err := a.store.queryMetadata(r.Context(), req)
	if err != nil {
		log.Printf("query metadata: %v", err)
		writeError(w, http.StatusInternalServerError, "metadata database error")
		return
	}
	writeResult(w, data)
}

func (a *api) updateStatus(w http.ResponseWriter, r *http.Request) {
	var req updateStatusRequest
	if !a.decode(w, r, &req, &req.Token) {
		return
	}
	if len(req.Payloads) == 0 {
		writeResult(w, "")
		return
	}
	for _, p := range req.Payloads {
		if p.IP == "" || p.Port <= 0 || !validStatus(p.Status) {
			writeError(w, http.StatusBadRequest, "invalid status payload")
			return
		}
	}
	if err := a.store.updateStatus(r.Context(), req); err != nil {
		writeStoreError(w, err)
		return
	}
	writeResult(w, "")
}

func (a *api) swapRole(w http.ResponseWriter, r *http.Request) {
	var req swapRoleRequest
	if !a.decode(w, r, &req, &req.Token) {
		return
	}
	if len(req.Payloads) == 0 {
		writeResult(w, "")
		return
	}
	for _, p := range req.Payloads {
		if p.Instance1.IP == "" || p.Instance1.Port <= 0 || p.Instance2.IP == "" || p.Instance2.Port <= 0 || p.Instance1 == p.Instance2 {
			writeError(w, http.StatusBadRequest, "invalid role swap payload")
			return
		}
	}
	if err := a.store.swapRole(r.Context(), req); err != nil {
		writeStoreError(w, err)
		return
	}
	writeResult(w, "")
}

func (a *api) unsupported(w http.ResponseWriter, r *http.Request) {
	var body map[string]any
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	if token == "" {
		token, _ = body["db_cloud_token"].(string)
	}
	if token != a.token {
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	writeError(w, http.StatusNotImplemented, "unsupported in standalone mode")
}

func (a *api) decode(w http.ResponseWriter, r *http.Request, dst any, bodyToken *string) bool {
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return false
	}
	token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	if token == "" {
		token = *bodyToken
	}
	if token != a.token {
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return false
	}
	return true
}

var errConflict = errors.New("metadata conflict")

func writeStoreError(w http.ResponseWriter, err error) {
	if errors.Is(err, sql.ErrNoRows) {
		writeError(w, http.StatusNotFound, "instance not found")
		return
	}
	if errors.Is(err, errConflict) {
		writeError(w, http.StatusConflict, err.Error())
		return
	}
	log.Printf("metadata mutation: %v", err)
	writeError(w, http.StatusInternalServerError, "metadata database error")
}

func writeResult(w http.ResponseWriter, data any) {
	writeJSON(w, http.StatusOK, map[string]any{"result": true, "code": 0, "message": "ok", "request_id": requestID(), "data": data})
}
func writeError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]any{"result": false, "code": status, "message": message, "request_id": requestID(), "data": ""})
}
func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}
func requestID() string         { return fmt.Sprintf("standalone-%d", time.Now().UnixNano()) }
func validStatus(s string) bool { return s == "running" || s == "available" || s == "unavailable" }

type row struct {
	HostID, BkCloudID, BkBizID, LogicalCityID, ClusterID, Port, AdminPort                     int
	IP, LogicalCityName, Cluster, ClusterType, MachineType, AccessLayer, InstanceRole, Status string
}

func (s *sqlStore) PingContext(ctx context.Context) error { return s.db.PingContext(ctx) }

func (s *sqlStore) queryMetadata(ctx context.Context, req metadataRequest) ([]*instanceMetadata, error) {
	q := `SELECT host_id,bk_cloud_id,bk_biz_id,logical_city_id,logical_city_name,cluster_id,cluster,cluster_type,machine_type,access_layer,instance_role,status,ip,port,admin_port FROM standalone_instances WHERE bk_cloud_id=?`
	args := []any{req.BkCloudID}
	q, args = stringFilter(q, args, "status", req.Statuses)
	q, args = stringFilter(q, args, "cluster_type", req.ClusterTypes)
	q, args = stringFilter(q, args, "CAST(logical_city_id AS CHAR)", req.LogicalCityIDs)
	if len(req.Addresses) > 0 {
		marks := strings.TrimRight(strings.Repeat("?,", len(req.Addresses)), ",")
		q += " AND (ip IN (" + marks + ") OR cluster IN (" + marks + "))"
		for _, v := range req.Addresses {
			args = append(args, v)
		}
		for _, v := range req.Addresses {
			args = append(args, v)
		}
	}
	if req.HashCnt > 0 {
		q += " AND MOD(host_id,?)=?"
		args = append(args, req.HashCnt, req.HashValue)
	}
	q += " ORDER BY cluster_id,machine_type,ip,port"
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var raw []row
	for rows.Next() {
		var v row
		if err := rows.Scan(&v.HostID, &v.BkCloudID, &v.BkBizID, &v.LogicalCityID, &v.LogicalCityName, &v.ClusterID, &v.Cluster, &v.ClusterType, &v.MachineType, &v.AccessLayer, &v.InstanceRole, &v.Status, &v.IP, &v.Port, &v.AdminPort); err != nil {
			return nil, err
		}
		raw = append(raw, v)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return s.enrich(ctx, raw)
}

func stringFilter(q string, args []any, column string, values []string) (string, []any) {
	if len(values) == 0 {
		return q, args
	}
	q += " AND " + column + " IN (" + strings.TrimRight(strings.Repeat("?,", len(values)), ",") + ")"
	for _, v := range values {
		args = append(args, v)
	}
	return q, args
}

func (s *sqlStore) enrich(ctx context.Context, rows []row) ([]*instanceMetadata, error) {
	out := make([]*instanceMetadata, 0, len(rows))
	for _, v := range rows {
		m := &instanceMetadata{BkCloudID: v.BkCloudID, BkBizID: v.BkBizID, LogicalCityID: v.LogicalCityID, LogicalCityName: v.LogicalCityName, ClusterID: v.ClusterID, Cluster: v.Cluster, ClusterType: v.ClusterType, MachineType: v.MachineType, AccessLayer: v.AccessLayer, InstanceRole: v.InstanceRole, Status: v.Status, IP: v.IP, Port: v.Port, AdminPort: v.AdminPort, BindEntry: map[string]any{}}
		if v.InstanceRole == "backend_master" {
			rel, err := s.clusterRelations(ctx, v.BkCloudID, v.ClusterID)
			if err != nil {
				return nil, err
			}
			m.Receiver = rel.receivers
			m.ProxyInstances = rel.proxies
		}
		out = append(out, m)
	}
	return out, nil
}

type relations struct {
	receivers []slaveInfo
	proxies   []proxyInfo
}

func (s *sqlStore) clusterRelations(ctx context.Context, cloudID, clusterID int) (relations, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT ip,port,admin_port,machine_type,instance_role,status FROM standalone_instances WHERE bk_cloud_id=? AND cluster_id=? ORDER BY ip,port`, cloudID, clusterID)
	if err != nil {
		return relations{}, err
	}
	defer rows.Close()
	var out relations
	for rows.Next() {
		var ip, machineType, role, status string
		var port, adminPort int
		if err := rows.Scan(&ip, &port, &adminPort, &machineType, &role, &status); err != nil {
			return relations{}, err
		}
		if role == "backend_slave" {
			out.receivers = append(out.receivers, slaveInfo{IP: ip, Port: port, IsStandBy: true, Status: status})
		}
		if machineType == "proxy" {
			out.proxies = append(out.proxies, proxyInfo{IP: ip, Port: port, AdminPort: adminPort, Status: status})
		}
	}
	return out, rows.Err()
}

func (s *sqlStore) updateStatus(ctx context.Context, req updateStatusRequest) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, p := range req.Payloads {
		res, err := tx.ExecContext(ctx, `UPDATE standalone_instances SET status=?,updated_at=CURRENT_TIMESTAMP WHERE bk_cloud_id=? AND ip=? AND port=?`, p.Status, req.BkCloudID, p.IP, p.Port)
		if err != nil {
			return err
		}
		n, err := res.RowsAffected()
		if err != nil {
			return err
		}
		if n == 0 {
			var exists int
			if err := tx.QueryRowContext(ctx, `SELECT 1 FROM standalone_instances WHERE bk_cloud_id=? AND ip=? AND port=?`, req.BkCloudID, p.IP, p.Port).Scan(&exists); err != nil {
				return err
			}
		}
	}
	return tx.Commit()
}

type roleState struct {
	clusterID         int
	machineType, role string
}

func (s *sqlStore) swapRole(ctx context.Context, req swapRoleRequest) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, p := range req.Payloads {
		first, err := readRole(ctx, tx, req.BkCloudID, p.Instance1)
		if err != nil {
			return err
		}
		second, err := readRole(ctx, tx, req.BkCloudID, p.Instance2)
		if err != nil {
			return err
		}
		doSwap, err := planSwap(first, second)
		if err != nil {
			return err
		}
		if !doSwap {
			continue
		}
		if _, err = tx.ExecContext(ctx, `UPDATE standalone_instances SET instance_role='backend_slave',updated_at=CURRENT_TIMESTAMP WHERE bk_cloud_id=? AND ip=? AND port=?`, req.BkCloudID, p.Instance1.IP, p.Instance1.Port); err != nil {
			return err
		}
		if _, err = tx.ExecContext(ctx, `UPDATE standalone_instances SET instance_role='backend_master',updated_at=CURRENT_TIMESTAMP WHERE bk_cloud_id=? AND ip=? AND port=?`, req.BkCloudID, p.Instance2.IP, p.Instance2.Port); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func readRole(ctx context.Context, tx *sql.Tx, cloudID int, ep endpoint) (roleState, error) {
	var out roleState
	err := tx.QueryRowContext(ctx, `SELECT cluster_id,machine_type,instance_role FROM standalone_instances WHERE bk_cloud_id=? AND ip=? AND port=? FOR UPDATE`, cloudID, ep.IP, ep.Port).Scan(&out.clusterID, &out.machineType, &out.role)
	return out, err
}

// planSwap distinguishes the requested transition from its already-committed state. A blind
// role toggle would undo a successful failover when Analysis retries after losing the response.
func planSwap(first, second roleState) (bool, error) {
	if first.clusterID != second.clusterID || first.machineType != "backend" || second.machineType != "backend" {
		return false, fmt.Errorf("%w: instances are not backend peers in one cluster", errConflict)
	}
	if first.role == "backend_master" && second.role == "backend_slave" {
		return true, nil
	}
	if first.role == "backend_slave" && second.role == "backend_master" {
		return false, nil
	}
	return false, fmt.Errorf("%w: expected one master and one slave", errConflict)
}
