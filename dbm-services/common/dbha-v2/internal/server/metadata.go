package server

import (
	"encoding/json"
	"fmt"
	"hash/crc32"
	"net"
	"net/http"
	"slices"
	"sort"
	"strconv"
)

func (s *Server) metadataCompatibility(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Token       string   `json:"db_cloud_token"`
		Cloud       int      `json:"bk_cloud_id"`
		Addresses   []string `json:"addresses"`
		Cities      []string `json:"logical_city_ids"`
		Statuses    []string `json:"statuses"`
		Types       []string `json:"cluster_types"`
		HashCount   int      `json:"hash_cnt"`
		HashValue   int      `json:"hash_value"`
		MachineOnly bool     `json:"machine_only"`
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	fail := func(code int, message string) {
		w.WriteHeader(code)
		_ = json.NewEncoder(w).Encode(map[string]any{"code": code, "message": message, "data": nil})
	}
	r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
	if decode(r, &req) != nil {
		fail(400, "invalid metadata query")
		return
	}
	token := bearer(r)
	if token == "" {
		token = req.Token
	}
	if s.Store.AuthenticateAdmin(r.Context(), token) != nil {
		fail(401, "unauthorized")
		return
	}
	if req.HashCount < 0 || req.HashValue < 0 || (req.HashCount > 0 && req.HashValue >= req.HashCount) {
		fail(400, "invalid shard")
		return
	}
	st, err := s.Store.Read(r.Context())
	if err != nil {
		fail(503, "storage unavailable")
		return
	}
	rows := []map[string]any{}
	if req.Cloud != 0 || (len(req.Types) > 0 && !slices.Contains(req.Types, "tendbha")) || (len(req.Cities) > 0 && !slices.Contains(req.Cities, "0")) {
		_ = json.NewEncoder(w).Encode(map[string]any{"code": 0, "message": "", "data": rows})
		return
	}
	status := func(id string) string {
		if st.Instances[id].Admission != "ACTIVE" || !s.freshInstance(st, id) {
			return "unavailable"
		}
		return "running"
	}
	endpoint := func(id string) map[string]any {
		i := st.Instances[id]
		host, portText, _ := net.SplitHostPort(i.Endpoint)
		port, _ := strconv.Atoi(portText)
		return map[string]any{"ip": host, "port": port, "status": status(id)}
	}
	for _, c := range st.Clusters {
		if c.TopologyState != "READY" || c.RecoveryGate != "NONE" || c.OperationState != "IDLE" {
			continue
		}
		ids := append([]string{c.PrimaryID, c.StandbyID}, c.ProxyIDs...)
		for _, id := range ids {
			i, ok := st.Instances[id]
			if !ok || i.Admission == "RETIRED" {
				continue
			}
			row := endpoint(id)
			host := row["ip"].(string)
			name := fmt.Sprintf("dbha-%d", c.ID)
			if len(req.Addresses) > 0 && !slices.Contains(req.Addresses, host) && !slices.Contains(req.Addresses, name) {
				continue
			}
			if len(req.Statuses) > 0 && !slices.Contains(req.Statuses, row["status"].(string)) {
				continue
			}
			if req.HashCount > 0 && int(crc32.ChecksumIEEE([]byte(host))%uint32(req.HashCount)) != req.HashValue {
				continue
			}
			row["bk_cloud_id"], row["bk_biz_id"], row["logical_city_id"], row["logical_city_name"] = 0, 1, 0, "standalone"
			row["cluster_id"], row["cluster"], row["cluster_type"], row["bind_entry"] = c.ID, name, "tendbha", map[string]any{}
			row["machine_type"], row["access_layer"], row["instance_role"], row["admin_port"], row["is_stand_by"] = "backend", "storage", "backend_slave", 0, id == c.StandbyID
			if i.Kind == "proxy" {
				row["machine_type"], row["access_layer"], row["instance_role"], row["admin_port"] = "proxy", "proxy", "proxy", i.AdminPort
			}
			if id == c.PrimaryID {
				row["instance_role"] = "backend_master"
				receivers := []map[string]any{}
				if backup, exists := st.Instances[c.StandbyID]; exists && backup.Admission != "RETIRED" {
					receiver := endpoint(backup.ID)
					receiver["is_stand_by"] = true
					receivers = append(receivers, receiver)
				}
				row["receiver"] = receivers
				proxies := []map[string]any{}
				for _, pid := range c.ProxyIDs {
					p := endpoint(pid)
					p["admin_port"] = st.Instances[pid].AdminPort
					proxies = append(proxies, p)
				}
				row["proxyinstance_set"] = proxies
			}
			rows = append(rows, row)
		}
	}
	sort.Slice(rows, func(i, j int) bool {
		return fmt.Sprint(rows[i]["cluster_id"], rows[i]["ip"], rows[i]["port"]) < fmt.Sprint(rows[j]["cluster_id"], rows[j]["ip"], rows[j]["port"])
	})
	_ = json.NewEncoder(w).Encode(map[string]any{"code": 0, "message": "", "data": rows})
}
