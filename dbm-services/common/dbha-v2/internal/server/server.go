package server

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	clientv3 "go.etcd.io/etcd/client/v3"
	"gopkg.in/natefinch/lumberjack.v2"
)

type Server struct {
	Store       *Store
	Config      Config
	started     time.Time
	log         *slog.Logger
	decisionMu  sync.Mutex
	watermarkMu sync.Mutex
	metricsOnce sync.Once
	handlerOnce sync.Once
	handler     http.Handler
	metrics     *serviceMetrics
	failures    map[string]int
	verify      func(context.Context, Instance, CredentialProfile) (observationResult, error)
	verifyStop  func(context.Context, Instance, CredentialProfile) error
	leaderCtx   context.Context
}

type watermark struct {
	ClusterID     uint64            `json:"etcd_cluster_id"`
	Revision      int64             `json:"revision"`
	Epochs        map[string]uint64 `json:"epochs"`
	RestoreMarker string            `json:"restore_marker,omitempty"`
}

const ownerAcquisitionTimeout = 35 * time.Second

// FinalizeMigration records the independent watermark after a verified offline
// import. It deliberately leaves MIGRATION_UNVERIFIED gates closed.
func FinalizeMigration(ctx context.Context, store *Store, stateDir string) error {
	if stateDir == "" {
		return errors.New("migration state directory is required")
	}
	s := &Server{Store: store, Config: Config{StateDir: stateDir}}
	return s.saveWatermark(ctx)
}

func Run(ctx context.Context, cfg Config) error {
	etcdTLS, err := clientTLS(cfg.EtcdCAFile, cfg.EtcdCertFile, cfg.EtcdKeyFile)
	if err != nil {
		return err
	}
	if cfg.EtcdCAFile == "" && cfg.EtcdCertFile == "" {
		etcdTLS = nil
	}
	client, err := clientv3.New(clientv3.Config{Endpoints: cfg.EtcdEndpoints, DialTimeout: requestTimeout, TLS: etcdTLS})
	if err != nil {
		return err
	}
	defer client.Close()
	if err := os.MkdirAll(filepath.Dir(cfg.LogFile), 0700); err != nil {
		return err
	}
	writer := &lumberjack.Logger{Filename: cfg.LogFile, MaxSize: 20, MaxBackups: 9, LocalTime: false}
	defer writer.Close()
	if cfg.NodeID == "" {
		cfg.NodeID, err = os.Hostname()
		if err != nil {
			return err
		}
	}
	runtime := &haRuntime{client: client, config: cfg, log: slog.New(slog.NewJSONHandler(writer, nil))}
	if err := runtime.init(ctx); err != nil {
		return err
	}
	var tlsConfig *tls.Config
	if cfg.TLSCertFile != "" {
		pair, err := tls.LoadX509KeyPair(cfg.TLSCertFile, cfg.TLSKeyFile)
		if err != nil {
			return err
		}
		tlsConfig = &tls.Config{Certificates: []tls.Certificate{pair}, MinVersion: tls.VersionTLS12}
	}
	httpListener, err := net.Listen("tcp", cfg.HTTPListen)
	if err != nil {
		return err
	}
	defer httpListener.Close()
	grpcListener, err := net.Listen("tcp", cfg.GRPCListen)
	if err != nil {
		return err
	}
	defer grpcListener.Close()
	httpServer := &http.Server{Handler: runtime.Handler(), ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 10 * time.Second, WriteTimeout: 15 * time.Second, IdleTimeout: 30 * time.Second, MaxHeaderBytes: 16 << 10}
	grpcServer := runtime.grpcServer(tlsConfig)
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	errs := make(chan error, 3)
	go func() {
		if tlsConfig == nil {
			errs <- httpServer.Serve(httpListener)
			return
		}
		errs <- httpServer.Serve(tls.NewListener(httpListener, tlsConfig))
	}()
	go func() { errs <- grpcServer.Serve(grpcListener) }()
	go func() { errs <- runtime.campaign(runCtx) }()
	select {
	case <-ctx.Done():
		err = nil
	case err = <-errs:
	}
	cancel()
	shutdown, done := context.WithTimeout(context.Background(), 5*time.Second)
	defer done()
	_ = httpServer.Shutdown(shutdown)
	grpcServer.Stop()
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

type haRuntime struct {
	client *clientv3.Client
	config Config
	log    *slog.Logger
	active atomic.Pointer[Server]
}

func (h *haRuntime) init(ctx context.Context) error {
	store := &Store{client: h.client, prefix: "/dbha/v1/" + url.PathEscape(h.config.EnvironmentID) + "/"}
	exists, err := store.validateSchema(ctx)
	if err != nil || exists {
		return err
	}
	current, err := h.client.Get(ctx, store.prefix, clientv3.WithPrefix(), clientv3.WithLimit(2))
	if err != nil {
		return err
	}
	if len(current.Kvs) != 0 {
		ownerKey := store.key("coordination/server-owner")
		if len(current.Kvs) == 1 && string(current.Kvs[0].Key) == ownerKey && current.Kvs[0].Lease != 0 {
			return nil
		}
		return apiError(http.StatusPreconditionFailed, "SCHEMA_CORRUPT", "schema is missing while DBHA state already exists")
	}
	return nil
}

func (h *haRuntime) campaign(ctx context.Context) error {
	record := ownerRecord{NodeID: h.config.NodeID, OwnerID: uuid.NewString(), AdvertiseHTTP: h.config.AdvertiseHTTP, AdvertiseGRPC: h.config.AdvertiseGRPC}
	for ctx.Err() == nil {
		if !h.candidateSafe(ctx) {
			h.syncFollowerWatermark(ctx)
		}
		if !h.candidateSafe(ctx) {
			if !sleepContext(ctx, time.Second) {
				break
			}
			continue
		}
		store, err := newStore(ctx, h.client, h.config.EnvironmentID, record)
		if err != nil {
			var api *Error
			if errors.As(err, &api) && api.Code == "OWNER_EXISTS" {
				h.syncFollowerWatermark(ctx)
				if !sleepContext(ctx, time.Second) {
					break
				}
				continue
			}
			return err
		}
		initial, err := store.Read(ctx)
		if err != nil {
			_ = store.Close()
			return err
		}
		token, err := adminToken(h.config.AdminTokenFile, len(initial.Credentials) == 0)
		if err == nil {
			err = store.InitAdmin(ctx, token)
		}
		s := &Server{Store: store, Config: h.config, started: time.Now().UTC(), log: h.log, failures: map[string]int{}}
		s.verify = s.inspect
		if err == nil {
			err = s.prepareRecovery(ctx, initial)
		}
		if err != nil {
			_ = store.Close()
			return err
		}
		leaderCtx, cancel := context.WithCancel(ctx)
		s.leaderCtx = leaderCtx
		h.active.Store(s)
		go s.loop(leaderCtx)
		select {
		case <-ctx.Done():
		case <-store.Done():
		}
		h.active.CompareAndSwap(s, nil)
		cancel()
		_ = store.Close()
	}
	return nil
}

func sleepContext(ctx context.Context, delay time.Duration) bool {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

func acquireStore(ctx context.Context, client *clientv3.Client, environmentID string, wait time.Duration) (*Store, error) {
	waitCtx, cancel := context.WithTimeout(ctx, wait)
	defer cancel()
	for {
		store, err := NewStore(waitCtx, client, environmentID)
		if err == nil {
			return store, nil
		}
		var api *Error
		if !errors.As(err, &api) || api.Code != "OWNER_EXISTS" {
			return nil, err
		}
		select {
		case <-waitCtx.Done():
			return nil, err
		case <-time.After(time.Second):
		}
	}
}

func (s *Server) prepareRecovery(ctx context.Context, initial *State) error {
	path := filepath.Join(s.Config.StateDir, "control-watermark.json")
	marker, markerExists, err := readRestoreMarker(s.Config.StateDir)
	if err != nil {
		return err
	}
	b, err := os.ReadFile(path)
	gate := "STARTUP_VALIDATION"
	var w watermark
	markerConsumed := false
	if err == nil {
		if json.Unmarshal(b, &w) != nil {
			if !markerExists {
				return errors.New("invalid control watermark; use controlled recovery")
			}
			gate = "RESTORE_UNVERIFIED"
		} else {
			markerConsumed = markerExists && w.RestoreMarker == marker
			if !watermarkCovers(w, initial) {
				gate = "RESTORE_UNVERIFIED"
			}
		}
	} else if !os.IsNotExist(err) {
		return err
	} else if len(initial.Clusters) > 0 || len(initial.Deployments) > 0 {
		gate = "RESTORE_UNVERIFIED"
	}
	if markerExists && !markerConsumed {
		gate = "RESTORE_UNVERIFIED"
	} else if markerConsumed {
		if removeErr := removeRestoreMarker(s.Config.StateDir); removeErr != nil && s.log != nil {
			s.log.Warn("consumed restore marker could not be removed", "error", removeErr.Error())
		}
	}
	if err := s.Store.Update(ctx, func(st *State) error {
		for id, c := range st.Clusters {
			if (c.RecoveryGate != "MIGRATION_UNVERIFIED" && c.RecoveryGate != "RESTORE_UNVERIFIED") || gate == "RESTORE_UNVERIFIED" {
				c.RecoveryGate = gate
			}
			if c.RecoveryGate == "RESTORE_UNVERIFIED" || c.RecoveryGate == "MIGRATION_UNVERIFIED" {
				c.SwitchingEnabled = false
			}
			if c.ActiveOperationID != "" {
				c.OperationState = "RECOVERY_REQUIRED"
			}
			c.ConfirmationCount = 0
			c.ConfirmationSince = time.Time{}
			c.LastEvidence = ""
			st.Clusters[id] = c
		}
		return nil
	}); err != nil {
		return err
	}
	if gate != "RESTORE_UNVERIFIED" {
		return s.saveWatermark(ctx)
	}
	return nil
}

func (s *Server) saveWatermark(ctx context.Context) error {
	s.watermarkMu.Lock()
	defer s.watermarkMu.Unlock()
	st, err := s.Store.Read(ctx)
	if err != nil {
		return err
	}
	marker, markerExists, err := readRestoreMarker(s.Config.StateDir)
	if err != nil {
		return err
	}
	if !markerExists {
		if previousBytes, readErr := os.ReadFile(filepath.Join(s.Config.StateDir, "control-watermark.json")); readErr == nil {
			var previous watermark
			if json.Unmarshal(previousBytes, &previous) == nil {
				marker = previous.RestoreMarker
			}
		}
	}
	w := watermark{ClusterID: st.EtcdClusterID, Revision: st.Revision, Epochs: map[string]uint64{}, RestoreMarker: marker}
	for id, c := range st.Clusters {
		w.Epochs[id] = c.TopologyEpoch
	}
	b, err := json.Marshal(w)
	if err != nil {
		return err
	}
	if err = writePrivate(filepath.Join(s.Config.StateDir, "control-watermark.json"), b); err != nil {
		_ = s.Store.Update(ctx, func(st *State) error {
			for id, c := range st.Clusters {
				c.RecoveryGate = "RESTORE_UNVERIFIED"
				c.SwitchingEnabled = false
				st.Clusters[id] = c
			}
			return nil
		})
		return fmt.Errorf("persist control watermark: %w", err)
	}
	if markerExists {
		if err = removeRestoreMarker(s.Config.StateDir); err != nil && s.log != nil {
			s.log.Warn("consumed restore marker could not be removed", "error", err.Error())
		}
	}
	return nil
}

func (s *Server) loop(ctx context.Context) {
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			if err := s.reconcile(ctx); err != nil {
				s.log.Warn("reconciliation deferred", "error", err.Error())
			}
		}
	}
}
