package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"os"
	"path/filepath"

	clientv3 "go.etcd.io/etcd/client/v3"
)

func (h *haRuntime) readState(ctx context.Context) (*State, error) {
	return (&Store{client: h.client, prefix: "/dbha/v1/" + url.PathEscape(h.config.EnvironmentID) + "/"}).Read(ctx)
}

func watermarkCovers(w watermark, st *State) bool {
	if w.ClusterID != st.EtcdClusterID || st.Revision < w.Revision {
		return false
	}
	for id, epoch := range w.Epochs {
		if c, ok := st.Clusters[id]; !ok || c.TopologyEpoch < epoch {
			return false
		}
	}
	return true
}

func (h *haRuntime) candidateSafe(ctx context.Context) bool {
	st, err := h.readState(ctx)
	if err != nil {
		return false
	}
	marker, markerExists, err := readRestoreMarker(h.config.StateDir)
	if err != nil {
		return false
	}
	b, err := os.ReadFile(filepath.Join(h.config.StateDir, "control-watermark.json"))
	if os.IsNotExist(err) {
		return markerExists || (len(st.Clusters) == 0 && len(st.Deployments) == 0)
	}
	if err != nil {
		return false
	}
	var w watermark
	if json.Unmarshal(b, &w) != nil {
		return markerExists
	}
	if markerExists && w.RestoreMarker != marker {
		return true
	}
	return watermarkCovers(w, st)
}

func restoreGated(st *State) bool {
	if len(st.Clusters) == 0 {
		return len(st.Deployments) == 0
	}
	for _, cluster := range st.Clusters {
		if cluster.RecoveryGate != "RESTORE_UNVERIFIED" || cluster.SwitchingEnabled {
			return false
		}
	}
	return true
}

// A follower may advance its local high-water mark only while another owner
// is present. This lets a new standby become electable without accepting an
// uncorroborated restored etcd snapshot.
func (h *haRuntime) syncFollowerWatermark(ctx context.Context) {
	before, err := h.leasedOwner(ctx)
	if err != nil {
		return
	}
	proved, err := h.waitOwnerHeartbeat(ctx, before)
	if err != nil {
		return
	}
	st, err := h.readState(ctx)
	if err != nil {
		return
	}
	after, err := h.leasedOwner(ctx)
	if err != nil || before.clusterID != st.EtcdClusterID || before.clusterID != after.clusterID || before.value != after.value || before.lease != after.lease || after.modRevision < proved.modRevision {
		return
	}
	path := filepath.Join(h.config.StateDir, "control-watermark.json")
	consumedMarker := ""
	if b, readErr := os.ReadFile(path); readErr == nil {
		var current watermark
		if json.Unmarshal(b, &current) != nil || (!watermarkCovers(current, st) && !restoreGated(st)) {
			return
		}
		consumedMarker = current.RestoreMarker
	} else if !os.IsNotExist(readErr) {
		return
	}
	marker, markerExists, markerErr := readRestoreMarker(h.config.StateDir)
	if markerErr != nil {
		return
	}
	if !markerExists {
		marker = consumedMarker
	}
	w := watermark{ClusterID: st.EtcdClusterID, Revision: st.Revision, Epochs: map[string]uint64{}, RestoreMarker: marker}
	for id, cluster := range st.Clusters {
		w.Epochs[id] = cluster.TopologyEpoch
	}
	if b, marshalErr := json.Marshal(w); marshalErr == nil {
		if writePrivate(path, b) == nil && markerExists {
			if err := removeRestoreMarker(h.config.StateDir); err != nil && h.log != nil {
				h.log.Warn("consumed restore marker could not be removed", "error", err.Error())
			}
		}
	}
}

func (h *haRuntime) waitOwnerHeartbeat(ctx context.Context, before ownerSnapshot) (ownerSnapshot, error) {
	waitCtx, cancel := context.WithTimeout(ctx, 3*ownerHeartbeatInterval)
	defer cancel()
	key := "/dbha/v1/" + url.PathEscape(h.config.EnvironmentID) + "/coordination/server-owner"
	watch := h.client.Watch(waitCtx, key, clientv3.WithRev(before.modRevision+1))
	for response := range watch {
		if response.Canceled {
			if err := response.Err(); err != nil {
				return ownerSnapshot{}, err
			}
			return ownerSnapshot{}, errors.New("owner heartbeat watch canceled")
		}
		for _, event := range response.Events {
			if event.Type != clientv3.EventTypePut || string(event.Kv.Value) != before.value || event.Kv.Lease != int64(before.lease) {
				return ownerSnapshot{}, errors.New("owner changed before heartbeat proof")
			}
			return ownerSnapshot{value: before.value, lease: before.lease, modRevision: event.Kv.ModRevision, clusterID: before.clusterID}, nil
		}
	}
	if err := waitCtx.Err(); err != nil {
		return ownerSnapshot{}, err
	}
	return ownerSnapshot{}, errors.New("owner heartbeat was not observed")
}

type ownerSnapshot struct {
	value       string
	lease       clientv3.LeaseID
	modRevision int64
	clusterID   uint64
}

func (h *haRuntime) leasedOwner(ctx context.Context) (ownerSnapshot, error) {
	r, err := h.client.Get(ctx, "/dbha/v1/"+url.PathEscape(h.config.EnvironmentID)+"/coordination/server-owner")
	if err != nil {
		return ownerSnapshot{}, err
	}
	if len(r.Kvs) != 1 || r.Kvs[0].Lease == 0 {
		return ownerSnapshot{}, errors.New("active owner lease is missing")
	}
	var owner ownerRecord
	if json.Unmarshal(r.Kvs[0].Value, &owner) != nil || owner.OwnerID == "" {
		return ownerSnapshot{}, errors.New("active owner identity is missing")
	}
	if owner.EtcdClusterID == 0 || owner.EtcdClusterID != r.Header.ClusterId {
		return ownerSnapshot{}, errors.New("active owner belongs to a different etcd cluster")
	}
	lease := clientv3.LeaseID(r.Kvs[0].Lease)
	ttl, err := h.client.TimeToLive(ctx, lease)
	if err != nil || ttl.TTL <= 0 {
		if err == nil {
			err = errors.New("active owner lease is expired")
		}
		return ownerSnapshot{}, err
	}
	if ttl.ResponseHeader == nil || ttl.ClusterId != owner.EtcdClusterID {
		return ownerSnapshot{}, errors.New("active owner lease belongs to a different etcd cluster")
	}
	return ownerSnapshot{value: string(r.Kvs[0].Value), lease: lease, modRevision: r.Kvs[0].ModRevision, clusterID: owner.EtcdClusterID}, nil
}

func (h *haRuntime) owner(ctx context.Context) (ownerRecord, error) {
	r, err := h.client.Get(ctx, "/dbha/v1/"+url.PathEscape(h.config.EnvironmentID)+"/coordination/server-owner")
	if err != nil || len(r.Kvs) == 0 {
		return ownerRecord{}, err
	}
	var owner ownerRecord
	if json.Unmarshal(r.Kvs[0].Value, &owner) != nil {
		owner.OwnerID = string(r.Kvs[0].Value) // legacy owner values carry no address hint
	}
	return owner, nil
}

func (h *haRuntime) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthz" {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("ok\n"))
			return
		}
		if r.URL.Path == "/api/v1/leader" {
			h.writeLeader(w, r)
			return
		}
		if active := h.active.Load(); active != nil {
			request, done := active.requestWithLeadership(r)
			defer done()
			active.Handler().ServeHTTP(w, request)
			return
		}
		if r.URL.Path == "/metrics" {
			w.Header().Set("Content-Type", "text/plain; version=0.0.4")
			_, _ = w.Write([]byte("# TYPE dbha_server_leader gauge\ndbha_server_leader 0\n"))
			return
		}
		h.writeNotLeader(w, r)
	})
}

func (s *Server) requestWithLeadership(r *http.Request) (*http.Request, func()) {
	if s.leaderCtx == nil {
		return r, func() {}
	}
	ctx, cancel := context.WithCancel(r.Context())
	stop := context.AfterFunc(s.leaderCtx, cancel)
	return r.WithContext(ctx), func() {
		stop()
		cancel()
	}
}

func (h *haRuntime) writeLeader(w http.ResponseWriter, r *http.Request) {
	owner, _ := h.owner(r.Context())
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"data": owner})
}

func (h *haRuntime) writeNotLeader(w http.ResponseWriter, r *http.Request) {
	owner, _ := h.owner(r.Context())
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Retry-After", "1")
	w.WriteHeader(http.StatusServiceUnavailable)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"error":  map[string]any{"code": "NOT_LEADER", "message": "request must be sent to the active controller", "retryable": true},
		"leader": owner,
	})
}
