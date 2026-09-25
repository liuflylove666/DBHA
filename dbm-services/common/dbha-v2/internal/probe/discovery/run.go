package discovery

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"time"

	contract "dbm-services/common/dbha-v2/pkg/discovery"
	"dbm-services/common/dbha-v2/pkg/process"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type Runner struct {
	Config     Config
	State      State
	Client     *Client
	InstanceID string
}

var (
	errSessionExpired = errors.New("discovery session expired")
	errSampleStale    = errors.New("discovery sample became stale before delivery")
)

func Run(ctx context.Context, path string) error {
	c, err := Load(path)
	if err != nil {
		return err
	}
	if err := c.ValidateRemote(); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(c.StateFile), 0o700); err != nil {
		return err
	}
	// Keep one owner for this durable identity throughout the session. The
	// short read/modify/write lock alone cannot prevent an older process from
	// rewriting a newer generation after another process starts.
	runLock, err := process.AcquireFileLock(c.StateFile+".run.lock", time.Second)
	if err != nil {
		return err
	}
	defer runLock.Unlock()
	state, err := StartBoot(c.StateFile, c.AgentID)
	if err != nil {
		return err
	}
	client, err := NewClient(c)
	if err != nil {
		return err
	}
	defer client.Close()
	r := &Runner{Config: c, State: state, Client: client}
	if err := r.register(ctx); err != nil {
		return err
	}
	defer func() {
		closeCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = client.CloseSession(closeCtx, c.AgentID, r.State.SessionID)
	}()
	topology := time.NewTicker(5 * time.Second)
	metrics := time.NewTicker(5 * time.Second)
	heartbeat := time.NewTicker(time.Second)
	replDelay := time.NewTicker(5 * time.Second)
	defer topology.Stop()
	defer metrics.Stop()
	defer heartbeat.Stop()
	defer replDelay.Stop()
	// An immediate first report lets control confirm topology before the first tick.
	if err := r.report(ctx, "topology"); err != nil {
		return err
	}
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-topology.C:
			if err := r.report(ctx, "topology"); err != nil {
				return err
			}
		case <-metrics.C:
			if err := r.report(ctx, "default"); err != nil {
				return err
			}
		case <-heartbeat.C:
			if err := r.report(ctx, "heartbeat"); err != nil {
				return err
			}
		case <-replDelay.C:
			if err := r.report(ctx, "repldelay"); err != nil {
				return err
			}
		}
	}
}

func (r *Runner) register(ctx context.Context) error {
	req := contract.RegisterRequest{SchemaVersion: contract.SchemaVersion, AgentID: r.Config.AgentID, BootID: r.State.BootID, BootGeneration: r.State.BootGeneration, ProbeVersion: "dbha-probe", Capabilities: []string{r.Config.Kind, "discovery-v1"}}
	var err error
	for delay := time.Second; ; {
		var response contract.RegisterResponse
		attempt, cancel := context.WithTimeout(ctx, 5*time.Second)
		response, err = r.Client.Register(attempt, req)
		cancel()
		if err == nil {
			r.State.SessionID, r.State.SessionEpoch = response.SessionID, response.SessionEpoch
			return SaveState(r.Config.StateFile, r.State)
		}
		var statusErr *HTTPStatusError
		if errors.As(err, &statusErr) {
			if statusErr.Status == 401 || statusErr.Status == 403 || (statusErr.Status == 409 && statusErr.Code != "SESSION_ACTIVE") {
				return err
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(delay):
		}
		if delay < 30*time.Second {
			delay *= 2
			if delay > 30*time.Second {
				delay = 30 * time.Second
			}
		}
	}
}

// report discards the rejected envelope and starts a fresh topology round on
// a new durable boot. Replaying a payload gathered under the old session
// would create false freshness after an etcd snapshot restore.
func (r *Runner) report(ctx context.Context, stream string) error {
	for attempts := 0; attempts < 3; attempts++ {
		err := r.reportOnce(ctx, stream)
		if errors.Is(err, errSampleStale) {
			// A suspended host can resume after the envelope's 15-second
			// freshness window. The server definitively rejected it, so collect
			// a new sample instead of terminating the long-running probe.
			continue
		}
		if !errors.Is(err, errSessionExpired) {
			return err
		}
		if err := r.recoverSession(ctx); err != nil {
			return err
		}
		stream = "topology"
	}
	return errSessionExpired
}

func (r *Runner) recoverSession(ctx context.Context) error {
	state, err := StartBoot(r.Config.StateFile, r.Config.AgentID)
	if err != nil {
		return err
	}
	r.State = state
	r.InstanceID = ""
	return r.register(ctx)
}

func (r *Runner) reportOnce(ctx context.Context, stream string) error {
	start := time.Now().UTC()
	collectCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	var payload any
	var instanceID string
	var db *sql.DB
	var err error
	if r.Config.Kind == "mysql" {
		c := r.Config.MySQL
		password, e := readSecret(c.PasswordFile)
		if e != nil {
			cancel()
			return e
		}
		db, err = contract.OpenLocalMySQL(contract.LocalMySQL{Network: c.Network, Address: c.Address, User: c.User, Password: password, AdvertiseHost: r.Config.AdvertiseHost, Port: c.Port})
		if err == nil {
			defer db.Close()
		}
	} else if r.Config.Proxy.AdminAddress != "" && r.Config.Proxy.AdminPasswordFile != "" {
		c := r.Config.Proxy
		password, e := readSecret(c.AdminPasswordFile)
		if e != nil {
			cancel()
			return e
		}
		db, err = contract.OpenLocalMySQL(contract.LocalMySQL{Network: c.AdminNetwork, Address: c.AdminAddress, User: c.AdminUser, Password: password})
		if err == nil {
			defer db.Close()
		}
	}
	if stream == "topology" {
		var obs contract.Observation
		if r.Config.Kind == "mysql" {
			obs, _ = contract.CollectMySQL(collectCtx, db, r.Config.AdvertiseHost, r.Config.MySQL.Port)
			if obs.ServerUUID != "" {
				instanceID = "mysql:" + obs.ServerUUID
			}
		} else {
			obs, _ = contract.CollectProxy(collectCtx, db, r.Config.AdvertiseHost, r.Config.Proxy.DataPort, r.Config.Proxy.AdminPort, r.Config.Proxy.UUID)
			instanceID = "proxy:" + r.Config.Proxy.UUID
		}
		payload = obs
	} else {
		if r.InstanceID == "" {
			cancel()
			return nil
		}
		if r.Config.Kind == "proxy" && stream == "repldelay" {
			cancel()
			return nil
		}
		instanceID = r.InstanceID
		if db == nil {
			payload = map[string]any{"collection_state": "ERROR", "error_code": "CONNECT_FAILED"}
		} else if stream == "heartbeat" {
			if r.Config.Kind == "mysql" {
				value, _ := contract.WriteHeartbeat(collectCtx, db, r.Config.AdvertiseHost, r.Config.MySQL.Port)
				payload = value
			} else {
				var one int
				if e := db.QueryRowContext(collectCtx, "SELECT 1").Scan(&one); e != nil {
					payload = map[string]any{"collection_state": "ERROR", "error_code": "PROXY_HEARTBEAT_FAILED"}
				} else {
					payload = map[string]any{"collection_state": "OK", "reachable": one == 1}
				}
			}
		} else if stream == "default" {
			value, e := contract.CollectSample(collectCtx, db)
			if e != nil {
				payload = map[string]any{"collection_state": "ERROR", "error_code": "METRICS_QUERY_FAILED"}
			} else {
				payload = value
			}
		} else {
			obs, e := contract.CollectMySQL(collectCtx, db, r.Config.AdvertiseHost, r.Config.MySQL.Port)
			if e != nil {
				payload = map[string]any{"collection_state": "ERROR", "error_code": obs.ErrorCode}
			} else {
				payload = map[string]any{"collection_state": "OK", "channels": obs.Channels}
			}
		}
	}
	cancel()
	b, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	if stream == "topology" && len(b) > 64*1024 {
		return fmt.Errorf("topology observation exceeds 64 KiB")
	}
	if stream != "topology" && len(b) > 128*1024 {
		return fmt.Errorf("sample exceeds 128 KiB")
	}
	r.State.Sequences[stream]++
	sequence := r.State.Sequences[stream]
	if err := SaveState(r.Config.StateFile, r.State); err != nil {
		return err
	}
	report := contract.Envelope{SchemaVersion: contract.SchemaVersion, AgentID: r.Config.AgentID, SessionID: r.State.SessionID, SessionEpoch: r.State.SessionEpoch, InstanceID: instanceID, Stream: stream, Sequence: sequence, SampledAt: start, CollectionDurationMS: time.Since(start).Milliseconds(), Payload: b}
	for delay := time.Second; ; {
		attempt, done := context.WithTimeout(ctx, 4*time.Second)
		ack, err := r.Client.Send(attempt, stream == "topology", report)
		done()
		if err == nil {
			if ack.InstanceID != "" {
				r.InstanceID = ack.InstanceID
			}
			if ack.RouteReconcileRequired {
				if err := r.signalReconcile(ack); err != nil {
					return err
				}
			}
			return nil
		}
		code := status.Code(err)
		if sessionExpired(err) {
			return errSessionExpired
		}
		if staleSample(err) {
			return errSampleStale
		}
		if code == codes.Unauthenticated || code == codes.PermissionDenied || code == codes.InvalidArgument || code == codes.FailedPrecondition {
			return err
		}
		if time.Since(start) >= 12*time.Second {
			return nil
		} // Re-collect rather than replay stale facts.
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(delay):
		}
		if delay < 8*time.Second {
			delay *= 2
		}
	}
}

func sessionExpired(err error) bool {
	s, ok := status.FromError(err)
	return ok && s.Code() == codes.Aborted && s.Message() == "SESSION_EXPIRED"
}

func staleSample(err error) bool {
	s, ok := status.FromError(err)
	return ok && s.Code() == codes.FailedPrecondition && s.Message() == "STALE_SAMPLE"
}

func (r *Runner) signalReconcile(ack contract.ReportResponse) error {
	path := r.Config.RouteReconcileSignalFile
	if path == "" {
		return errors.New("route reconciliation requested but no local signal file configured")
	}
	b, err := json.Marshal(map[string]any{"route_reconcile_required": true, "authoritative_topology_epoch": ack.AuthoritativeTopologyEpoch, "requested_at": time.Now().UTC()})
	if err != nil {
		return err
	}
	_, err = process.WriteFileWithLock(path, b, 5*time.Second)
	return err
}

// HealthCheck performs one local read without contacting control or changing
// the boot generation. Exit status 2 means network-level DB DOWN only; 3 means
// credentials, SQL or other indeterminate errors. SSH transport failures remain
// distinguishable to the caller because this command was never run.
func HealthCheck(path string) (map[string]any, int) {
	c, err := Load(path)
	if err != nil {
		return map[string]any{"collection_state": "ERROR", "error_code": "CONFIG_INVALID"}, 3
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if c.Kind != "mysql" {
		return map[string]any{"collection_state": "ERROR", "error_code": "UNSUPPORTED_KIND"}, 3
	}
	password, err := readSecret(c.MySQL.PasswordFile)
	if err != nil {
		return map[string]any{"collection_state": "ERROR", "error_code": "CREDENTIAL_UNREADABLE"}, 3
	}
	db, err := contract.OpenLocalMySQL(contract.LocalMySQL{Network: c.MySQL.Network, Address: c.MySQL.Address, User: c.MySQL.User, Password: password})
	if err != nil {
		return map[string]any{"collection_state": "ERROR", "error_code": "CONNECT_SETUP_FAILED"}, 3
	}
	defer db.Close()
	obs, err := contract.CollectMySQL(ctx, db, c.AdvertiseHost, c.MySQL.Port)
	result := map[string]any{"collection_state": obs.CollectionState, "error_code": obs.ErrorCode, "advertise_host": c.AdvertiseHost, "port": c.MySQL.Port, "server_uuid": obs.ServerUUID, "replication_query_state": obs.ReplicationQueryState, "channels": obs.Channels}
	if err != nil {
		if isDBDown(err) {
			result["error_code"] = "DB_DOWN"
			return result, 2
		}
		return result, 3
	}
	return result, 0
}

func isDBDown(err error) bool {
	var netErr *net.OpError
	return errors.As(err, &netErr)
}
