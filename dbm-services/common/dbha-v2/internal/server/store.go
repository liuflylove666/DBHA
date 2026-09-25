package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"go.etcd.io/etcd/api/v3/etcdserverpb"
	clientv3 "go.etcd.io/etcd/client/v3"
)

type Store struct {
	client      *clientv3.Client
	prefix      string
	owner       string
	clusterID   uint64
	lease       clientv3.LeaseID
	cancel      context.CancelFunc
	done        chan struct{}
	doneOnce    sync.Once
	mu          sync.Mutex
	ingestCount int
	ingestBytes int
}

type storedValue struct {
	SchemaVersion int             `json:"schema_version"`
	Value         json.RawMessage `json:"value"`
}

const ownerHeartbeatInterval = 2 * time.Second

// NewStore owns only its lease; the caller retains ownership of client.
func NewStore(ctx context.Context, client *clientv3.Client, environmentID string) (*Store, error) {
	return newStore(ctx, client, environmentID, ownerRecord{OwnerID: uuid.NewString()})
}

type ownerRecord struct {
	NodeID        string `json:"node_id,omitempty"`
	OwnerID       string `json:"owner_id"`
	AdvertiseHTTP string `json:"advertise_http,omitempty"`
	AdvertiseGRPC string `json:"advertise_grpc,omitempty"`
	EtcdClusterID uint64 `json:"etcd_cluster_id,omitempty"`
}

func newStore(ctx context.Context, client *clientv3.Client, environmentID string, record ownerRecord) (*Store, error) {
	if client == nil || environmentID == "" || strings.ContainsAny(environmentID, "/\\") {
		return nil, apiError(http.StatusBadRequest, "INVALID_ENVIRONMENT", "invalid environment id or etcd client")
	}
	if record.OwnerID == "" {
		record.OwnerID = uuid.NewString()
	}
	s := &Store{client: client, prefix: "/dbha/v1/" + url.PathEscape(environmentID) + "/", done: make(chan struct{})}
	lease, err := client.Grant(ctx, 30)
	if err != nil {
		return nil, err
	}
	if lease.ResponseHeader == nil || lease.ClusterId == 0 {
		_, _ = client.Revoke(context.Background(), lease.ID)
		return nil, errors.New("etcd lease response is missing cluster identity")
	}
	record.EtcdClusterID = lease.ClusterId
	b, err := json.Marshal(record)
	if err != nil {
		_, _ = client.Revoke(context.Background(), lease.ID)
		return nil, err
	}
	s.owner = string(b)
	s.clusterID = record.EtcdClusterID
	s.lease = lease.ID
	ownerKey := s.key("coordination/server-owner")
	resp, err := client.Txn(ctx).If(clientv3.Compare(clientv3.Version(ownerKey), "=", 0)).Then(clientv3.OpPut(ownerKey, s.owner, clientv3.WithLease(s.lease))).Commit()
	if err != nil || !resp.Succeeded || resp.Header.ClusterId != s.clusterID {
		_, _ = client.Revoke(context.Background(), s.lease)
		if err != nil {
			return nil, err
		}
		if resp.Header.ClusterId != s.clusterID {
			return nil, apiError(http.StatusServiceUnavailable, "ETCD_CLUSTER_CHANGED", "etcd cluster changed during owner acquisition")
		}
		return nil, apiError(http.StatusServiceUnavailable, "OWNER_EXISTS", "another server owns the environment")
	}
	kaCtx, cancel := context.WithCancel(context.Background())
	s.cancel = cancel
	ch, err := client.KeepAlive(kaCtx, s.lease)
	if err != nil {
		cancel()
		_, _ = client.Revoke(context.Background(), s.lease)
		return nil, err
	}
	go func() {
		for range ch {
		}
		s.signalDone()
	}()
	go func(revision int64) {
		watch := client.Watch(kaCtx, ownerKey, clientv3.WithRev(revision+1))
		for response := range watch {
			if response.Canceled {
				s.signalDone()
				return
			}
			for _, event := range response.Events {
				if event.Type == clientv3.EventTypeDelete || string(event.Kv.Value) != s.owner || event.Kv.Lease != int64(s.lease) {
					s.signalDone()
					return
				}
			}
		}
		s.signalDone()
	}(resp.Header.Revision)
	go s.heartbeatOwner(kaCtx, ownerKey)
	if err := s.init(ctx); err != nil {
		_ = s.Close()
		return nil, err
	}
	if err := s.ensureDefaultPolicy(ctx); err != nil {
		_ = s.Close()
		return nil, err
	}
	return s, nil
}

func (s *Store) Done() <-chan struct{} { return s.done }

func (s *Store) signalDone() { s.doneOnce.Do(func() { close(s.done) }) }

func (s *Store) heartbeatOwner(ctx context.Context, ownerKey string) {
	ticker := time.NewTicker(ownerHeartbeatInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			requestCtx, cancel := context.WithTimeout(ctx, requestTimeout)
			response, err := s.client.Txn(requestCtx).If(
				clientv3.Compare(clientv3.Value(ownerKey), "=", s.owner),
				clientv3.Compare(clientv3.LeaseValue(ownerKey), "=", int64(s.lease)),
			).Then(clientv3.OpPut(ownerKey, s.owner, clientv3.WithLease(s.lease))).Commit()
			cancel()
			if err != nil || !response.Succeeded || response.Header.ClusterId != s.clusterID {
				s.signalDone()
				return
			}
		}
	}
}

func (s *Store) key(relative string) string { return s.prefix + relative }

func (s *Store) init(ctx context.Context) error {
	exists, err := s.validateSchema(ctx)
	if err != nil {
		return err
	}
	if exists {
		return nil
	}
	ownerKey := s.key("coordination/server-owner")
	current, err := s.client.Get(ctx, s.prefix, clientv3.WithPrefix())
	if err != nil {
		return err
	}
	if current.Header.ClusterId != s.clusterID {
		return apiError(http.StatusServiceUnavailable, "ETCD_CLUSTER_CHANGED", "etcd cluster changed during schema initialization")
	}
	if len(current.Kvs) != 1 || string(current.Kvs[0].Key) != ownerKey || string(current.Kvs[0].Value) != s.owner || current.Kvs[0].Lease != int64(s.lease) {
		return apiError(http.StatusPreconditionFailed, "SCHEMA_CORRUPT", "schema is missing while DBHA state already exists")
	}
	schemaKey := s.key("schema")
	counterKey := s.key("counters/cluster-id")
	versionKey := s.key("coordination/version")
	resp, err := s.client.Txn(ctx).If(
		clientv3.Compare(clientv3.Value(ownerKey), "=", s.owner),
		clientv3.Compare(clientv3.LeaseValue(ownerKey), "=", int64(s.lease)),
		clientv3.Compare(clientv3.Version(schemaKey), "=", 0),
		clientv3.Compare(clientv3.Version(counterKey), "=", 0),
		clientv3.Compare(clientv3.Version(versionKey), "=", 0),
	).Then(
		clientv3.OpPut(schemaKey, `{"schema_version":1,"min_protocol_version":1}`),
		clientv3.OpPut(counterKey, `{"schema_version":1,"value":0}`),
		clientv3.OpPut(versionKey, "1"),
	).Commit()
	if err != nil {
		return err
	}
	if !resp.Succeeded || resp.Header.ClusterId != s.clusterID {
		return apiError(http.StatusPreconditionFailed, "SCHEMA_CORRUPT", "schema initialization lost ownership or found existing DBHA state")
	}
	_, err = s.validateSchema(ctx)
	return err
}

func (s *Store) validateSchema(ctx context.Context) (bool, error) {
	got, err := s.client.Get(ctx, s.key("schema"))
	if err != nil || len(got.Kvs) == 0 {
		return false, err
	}
	if s.clusterID != 0 && got.Header.ClusterId != s.clusterID {
		return false, apiError(http.StatusServiceUnavailable, "ETCD_CLUSTER_CHANGED", "etcd cluster identity changed")
	}
	if len(got.Kvs) != 1 {
		return false, errors.New("invalid dbha schema cardinality")
	}
	var schema struct {
		SchemaVersion int `json:"schema_version"`
	}
	if json.Unmarshal(got.Kvs[0].Value, &schema) != nil || schema.SchemaVersion != schemaVersion {
		return false, apiError(http.StatusPreconditionFailed, "SCHEMA_UNSUPPORTED", "unsupported etcd schema")
	}
	return true, nil
}

func (s *Store) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cancel == nil {
		return nil
	}
	s.cancel()
	s.cancel = nil
	s.signalDone()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := s.client.Revoke(ctx, s.lease)
	return err
}

func (s *Store) Ready(ctx context.Context) error {
	r, err := s.client.Get(ctx, s.key("coordination/server-owner"))
	if err != nil {
		return err
	}
	if r.Header.ClusterId != s.clusterID {
		return apiError(http.StatusServiceUnavailable, "ETCD_CLUSTER_CHANGED", "etcd cluster identity changed")
	}
	if len(r.Kvs) != 1 || string(r.Kvs[0].Value) != s.owner || r.Kvs[0].Lease != int64(s.lease) {
		return apiError(http.StatusServiceUnavailable, "OWNER_LOST", "server owner lease is not current")
	}
	alarms, err := s.client.AlarmList(ctx)
	if err != nil {
		return err
	}
	for _, alarm := range alarms.Alarms {
		if alarm.Alarm == etcdserverpb.AlarmType_NOSPACE {
			return apiError(http.StatusServiceUnavailable, "ETCD_NOSPACE", "etcd storage alarm is active")
		}
	}
	return nil
}

func (s *Store) MaxDBSize(ctx context.Context) (int64, error) {
	endpoints := s.client.Endpoints()
	if len(endpoints) == 0 {
		return 0, errors.New("no etcd endpoint")
	}
	var maximum int64
	var lastErr error
	reachable := false
	for _, endpoint := range endpoints {
		if err := ctx.Err(); err != nil {
			return 0, err
		}
		requestCtx, cancel := context.WithTimeout(ctx, requestTimeout)
		status, err := s.client.Status(requestCtx, endpoint)
		cancel()
		if err != nil || status.Header == nil || (s.clusterID != 0 && status.Header.ClusterId != s.clusterID) {
			if err == nil {
				err = apiError(http.StatusServiceUnavailable, "ETCD_CLUSTER_CHANGED", "etcd cluster identity changed")
			}
			lastErr = err
			continue
		}
		reachable = true
		if status.DbSize > maximum {
			maximum = status.DbSize
		}
	}
	if !reachable {
		return 0, lastErr
	}
	return maximum, nil
}

func (s *Store) Read(ctx context.Context) (*State, error) {
	r, err := s.client.Get(ctx, s.prefix, clientv3.WithPrefix())
	if err != nil {
		return nil, err
	}
	if s.clusterID != 0 && r.Header.ClusterId != s.clusterID {
		return nil, apiError(http.StatusServiceUnavailable, "ETCD_CLUSTER_CHANGED", "etcd cluster identity changed")
	}
	st := emptyState()
	st.Revision = r.Header.Revision
	st.EtcdClusterID = r.Header.ClusterId
	type reportMark struct {
		revision int64
		sequence uint64
		session  string
		digest   string
	}
	reportRevisions := make(map[string]reportMark)
	streamRevisions := make(map[string]SequenceMark)
	for _, kv := range r.Kvs {
		k := strings.TrimPrefix(string(kv.Key), s.prefix)
		st.ModRevisions[k] = kv.ModRevision
		if k == "coordination/version" {
			st.VersionRevision = kv.ModRevision
			continue
		}
		if strings.HasPrefix(k, "coordination/switch/") {
			st.SwitchLocks[strings.TrimPrefix(k, "coordination/switch/")] = string(kv.Value)
			continue
		}
		if strings.HasPrefix(k, "stream-marks/") {
			encoded := strings.TrimPrefix(k, "stream-marks/")
			streamID, err := url.PathUnescape(encoded)
			if err != nil {
				return nil, err
			}
			var wrapped storedValue
			if err := json.Unmarshal(kv.Value, &wrapped); err != nil || wrapped.SchemaVersion != schemaVersion {
				return nil, apiError(http.StatusPreconditionFailed, "SCHEMA_UNSUPPORTED", "invalid stream mark")
			}
			var mark SequenceMark
			if err := json.Unmarshal(wrapped.Value, &mark); err != nil {
				return nil, err
			}
			mark.StoredRevision = kv.ModRevision
			streamRevisions[streamID] = mark
			continue
		}
		if k == "counters/cluster-id" {
			var v storedValue
			if err := json.Unmarshal(kv.Value, &v); err != nil {
				return nil, err
			}
			if err := json.Unmarshal(v.Value, &st.ClusterIDCounter); err != nil {
				return nil, err
			}
			continue
		}
		section, encoded, ok := strings.Cut(k, "/")
		if !ok {
			continue
		}
		id, err := url.PathUnescape(encoded)
		if err != nil {
			return nil, err
		}
		if section == "coordination" || section == "counters" {
			continue
		}
		var v storedValue
		if err := json.Unmarshal(kv.Value, &v); err != nil {
			return nil, fmt.Errorf("decode %s: %w", k, err)
		}
		if v.SchemaVersion != schemaVersion {
			return nil, apiError(http.StatusPreconditionFailed, "SCHEMA_UNSUPPORTED", k)
		}
		switch section {
		case "deployments":
			var x Deployment
			err = json.Unmarshal(v.Value, &x)
			st.Deployments[id] = x
		case "credentials":
			var x Credential
			err = json.Unmarshal(v.Value, &x)
			st.Credentials[id] = x
		case "agents":
			var x Agent
			err = json.Unmarshal(v.Value, &x)
			for stream, mark := range x.Streams {
				mark.StoredRevision = kv.ModRevision
				x.Streams[stream] = mark
			}
			st.Agents[id] = x
		case "instances":
			var x Instance
			err = json.Unmarshal(v.Value, &x)
			st.Instances[id] = x
		case "endpoint-index":
			var x string
			err = json.Unmarshal(v.Value, &x)
			st.EndpointIndex[id] = x
		case "clusters":
			var x Cluster
			err = json.Unmarshal(v.Value, &x)
			st.Clusters[id] = x
		case "observations":
			var x Report
			err = json.Unmarshal(v.Value, &x)
			x.StoredRevision = kv.ModRevision
			reportRevisions[x.Envelope.AgentID+"/"+x.Envelope.Stream] = reportMark{kv.ModRevision, x.Envelope.Sequence, x.Envelope.SessionID, x.Digest}
			st.Observations[id] = x
		case "samples":
			var x Report
			err = json.Unmarshal(v.Value, &x)
			x.StoredRevision = kv.ModRevision
			reportRevisions[x.Envelope.AgentID+"/"+x.Envelope.Stream] = reportMark{kv.ModRevision, x.Envelope.Sequence, x.Envelope.SessionID, x.Digest}
			st.Samples[id] = x
		case "operations":
			var x Operation
			err = json.Unmarshal(v.Value, &x)
			st.Operations[id] = x
		case "policies":
			var x Policy
			err = json.Unmarshal(v.Value, &x)
			st.Policies[id] = x
		case "exclusions":
			var x Exclusion
			err = json.Unmarshal(v.Value, &x)
			st.Exclusions[id] = x
		default:
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("decode %s: %w", k, err)
		}
	}
	for id, agent := range st.Agents {
		for stream, mark := range agent.Streams {
			if persisted := streamRevisions[id+"|"+stream]; persisted.StoredRevision != 0 && persisted.Sequence == mark.Sequence && persisted.SessionID == mark.SessionID && persisted.Digest == mark.Digest {
				mark.StoredRevision = persisted.StoredRevision
				agent.Streams[stream] = mark
			} else if report := reportRevisions[id+"/"+stream]; report.revision != 0 && report.sequence == mark.Sequence && report.session == mark.SessionID && report.digest == mark.Digest {
				mark.StoredRevision = report.revision
				agent.Streams[stream] = mark
			}
		}
		st.Agents[id] = agent
	}
	return st, nil
}

func (s *Store) Update(ctx context.Context, mutate func(*State) error) error {
	_, err := s.updateRevision(ctx, mutate)
	return err
}

func (s *Store) updateRevision(ctx context.Context, mutate func(*State) error) (int64, error) {
	if mutate == nil {
		return 0, errors.New("nil mutate callback")
	}
	for attempts := 0; attempts < 8; attempts++ {
		if err := s.Ready(ctx); err != nil {
			return 0, err
		}
		before, err := s.Read(ctx)
		if err != nil {
			return 0, err
		}
		// The callback is run again after a concurrent CAS conflict; it must have no external side effects.
		copyBytes, err := json.Marshal(before)
		if err != nil {
			return 0, err
		}
		after := emptyState()
		if err := json.Unmarshal(copyBytes, after); err != nil {
			return 0, err
		}
		after.Revision = before.Revision
		after.VersionRevision = before.VersionRevision
		after.EtcdClusterID = before.EtcdClusterID
		if err := mutate(after); err != nil {
			return 0, err
		}
		ops, err := s.diff(before, after)
		if err != nil {
			return 0, err
		}
		if len(ops) == 0 {
			return 0, nil
		}
		if len(ops) > 120 {
			return 0, apiError(http.StatusRequestEntityTooLarge, "TXN_TOO_LARGE", "too many keys in one update")
		}
		versionKey := s.key("coordination/version")
		ownerKey := s.key("coordination/server-owner")
		// The version revision comes from the same snapshot as state. Unrelated
		// keys can advance the cluster revision without making it stale.
		if before.VersionRevision == 0 {
			return 0, errors.New("missing coordination version")
		}
		ops = append(ops, clientv3.OpPut(versionKey, strconv.FormatInt(time.Now().UnixNano(), 10)))
		r, err := s.client.Txn(ctx).If(
			clientv3.Compare(clientv3.ModRevision(versionKey), "=", before.VersionRevision),
			clientv3.Compare(clientv3.Value(ownerKey), "=", s.owner),
			clientv3.Compare(clientv3.LeaseValue(ownerKey), "=", int64(s.lease)),
		).Then(ops...).Commit()
		if err != nil {
			return 0, err
		} // Uncertain commit is never reported as success.
		if r.Header.ClusterId != s.clusterID {
			return 0, apiError(http.StatusServiceUnavailable, "ETCD_CLUSTER_CHANGED", "etcd cluster changed during update")
		}
		if r.Succeeded {
			return r.Header.Revision, nil
		}
	}
	return 0, apiError(http.StatusConflict, "CAS_CONFLICT", "concurrent update; retry")
}

// updateIngestRevision uses record-level compares so independent agents can
// persist reports concurrently. Every successful report also advances the
// global version key without comparing it; management/topology transactions
// compare that key and therefore retry if their evidence changed meanwhile.
func (s *Store) updateIngestRevision(ctx context.Context, agentID string, observation bool, mutate func(*State) error) (int64, error) {
	if mutate == nil {
		return 0, errors.New("nil mutate callback")
	}
	for attempts := 0; attempts < 8; attempts++ {
		before, err := s.Read(ctx)
		if err != nil {
			return 0, err
		}
		copyBytes, err := json.Marshal(before)
		if err != nil {
			return 0, err
		}
		after := emptyState()
		if err := json.Unmarshal(copyBytes, after); err != nil {
			return 0, err
		}
		after.Revision = before.Revision
		after.VersionRevision = before.VersionRevision
		after.EtcdClusterID = before.EtcdClusterID
		after.ModRevisions = before.ModRevisions
		if err := mutate(after); err != nil {
			return 0, err
		}
		ops, err := s.diff(before, after)
		if err != nil {
			return 0, err
		}
		if len(ops) == 0 {
			if err := s.Ready(ctx); err != nil {
				return 0, err
			}
			return 0, nil
		}
		if len(ops) > 120 {
			return 0, apiError(http.StatusRequestEntityTooLarge, "TXN_TOO_LARGE", "too many keys in one update")
		}

		dependencies := make(map[string]struct{})
		for _, op := range ops {
			dependencies[strings.TrimPrefix(string(op.KeyBytes()), s.prefix)] = struct{}{}
		}
		agent, ok := before.Agents[agentID]
		if !ok {
			return 0, apiError(http.StatusUnauthorized, "UNAUTHORIZED", "unknown agent")
		}
		dependencies["agents/"+url.PathEscape(agentID)] = struct{}{}
		dependencies["credentials/"+url.PathEscape(agent.CredentialID)] = struct{}{}
		dependencies["deployments/"+url.PathEscape(agent.DeploymentID)] = struct{}{}
		if deployment, ok := before.Deployments[agent.DeploymentID]; ok {
			dependencies["clusters/"+url.PathEscape(clusterKey(deployment.ClusterID))] = struct{}{}
		}
		if observation {
			// Admission and endpoint conflict decisions inspect the complete,
			// bounded instance/index set (30 instances in the required load).
			for id := range before.Instances {
				dependencies["instances/"+url.PathEscape(id)] = struct{}{}
			}
			for endpoint := range before.EndpointIndex {
				dependencies["endpoint-index/"+url.PathEscape(endpoint)] = struct{}{}
			}
		}
		compares := []clientv3.Cmp{
			clientv3.Compare(clientv3.Value(s.key("coordination/server-owner")), "=", s.owner),
			clientv3.Compare(clientv3.LeaseValue(s.key("coordination/server-owner")), "=", int64(s.lease)),
		}
		for relative := range dependencies {
			key := s.key(relative)
			if revision := before.ModRevisions[relative]; revision == 0 {
				compares = append(compares, clientv3.Compare(clientv3.Version(key), "=", 0))
			} else {
				compares = append(compares, clientv3.Compare(clientv3.ModRevision(key), "=", revision))
			}
		}
		ops = append(ops, clientv3.OpPut(s.key("coordination/version"), strconv.FormatInt(time.Now().UnixNano(), 10)))
		r, err := s.client.Txn(ctx).If(compares...).Then(ops...).Commit()
		if err != nil {
			return 0, err
		}
		if r.Header.ClusterId != s.clusterID {
			return 0, apiError(http.StatusServiceUnavailable, "ETCD_CLUSTER_CHANGED", "etcd cluster changed during ingest")
		}
		if r.Succeeded {
			return r.Header.Revision, nil
		}
	}
	return 0, apiError(http.StatusConflict, "CAS_CONFLICT", "concurrent update; retry")
}

func (s *Store) diff(a, b *State) ([]clientv3.Op, error) {
	var ops []clientv3.Op
	add := func(section string, old, new any) error {
		ao := reflect.ValueOf(old)
		bn := reflect.ValueOf(new)
		keys := make(map[string]struct{})
		for _, k := range ao.MapKeys() {
			keys[k.String()] = struct{}{}
		}
		for _, k := range bn.MapKeys() {
			keys[k.String()] = struct{}{}
		}
		for id := range keys {
			o := ao.MapIndex(reflect.ValueOf(id))
			n := bn.MapIndex(reflect.ValueOf(id))
			key := s.key(section + "/" + url.PathEscape(id))
			if !n.IsValid() {
				ops = append(ops, clientv3.OpDelete(key))
				continue
			}
			if o.IsValid() && reflect.DeepEqual(o.Interface(), n.Interface()) {
				continue
			}
			enc, err := marshalValue(n.Interface())
			if err != nil {
				return err
			}
			ops = append(ops, clientv3.OpPut(key, string(enc)))
		}
		return nil
	}
	for _, item := range []struct {
		section  string
		old, new any
	}{
		{"deployments", a.Deployments, b.Deployments}, {"credentials", a.Credentials, b.Credentials},
		{"agents", a.Agents, b.Agents}, {"instances", a.Instances, b.Instances},
		{"endpoint-index", a.EndpointIndex, b.EndpointIndex}, {"clusters", a.Clusters, b.Clusters},
		{"observations", a.Observations, b.Observations}, {"samples", a.Samples, b.Samples},
		{"operations", a.Operations, b.Operations}, {"policies", a.Policies, b.Policies},
		{"exclusions", a.Exclusions, b.Exclusions},
	} {
		if err := add(item.section, item.old, item.new); err != nil {
			return nil, err
		}
	}
	// A separate key per stream makes the accepted sequence's exact commit
	// revision recoverable even when later updates rewrite the agent record.
	for agentID, next := range b.Agents {
		previous := a.Agents[agentID]
		for stream, mark := range next.Streams {
			old, ok := previous.Streams[stream]
			old.StoredRevision, mark.StoredRevision = 0, 0
			if ok && reflect.DeepEqual(old, mark) {
				continue
			}
			key := s.key("stream-marks/" + url.PathEscape(agentID+"|"+stream))
			encoded, err := marshalValue(mark)
			if err != nil {
				return nil, err
			}
			ops = append(ops, clientv3.OpPut(key, string(encoded)))
		}
	}
	if a.ClusterIDCounter != b.ClusterIDCounter {
		v, err := marshalValue(b.ClusterIDCounter)
		if err != nil {
			return nil, err
		}
		ops = append(ops, clientv3.OpPut(s.key("counters/cluster-id"), string(v)))
	}
	for id, old := range a.SwitchLocks {
		if _, ok := b.SwitchLocks[id]; !ok {
			ops = append(ops, clientv3.OpDelete(s.key("coordination/switch/"+id)))
		} else if b.SwitchLocks[id] != old {
			return nil, apiError(http.StatusConflict, "SWITCH_LOCK_CONFLICT", "switch lock cannot be changed")
		}
	}
	for id, value := range b.SwitchLocks {
		if _, ok := a.SwitchLocks[id]; !ok {
			ops = append(ops, clientv3.OpPut(s.key("coordination/switch/"+id), value, clientv3.WithLease(s.lease)))
		}
	}
	return ops, nil
}

func marshalValue(v any) ([]byte, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	return json.Marshal(storedValue{SchemaVersion: schemaVersion, Value: b})
}
