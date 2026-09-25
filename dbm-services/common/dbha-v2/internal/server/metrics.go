package server

import (
	"errors"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

type serviceMetrics struct {
	mu                 sync.Mutex
	registry           *prometheus.Registry
	ack                *prometheus.HistogramVec
	duplicates         prometheus.Counter
	rejections         *prometheus.CounterVec
	http               *prometheus.CounterVec
	registrations      *prometheus.CounterVec
	routeUnavailable   prometheus.Counter
	states             *prometheus.GaugeVec
	age                *prometheus.GaugeVec
	agents             *prometheus.GaugeVec
	instances          *prometheus.GaugeVec
	pendingRecovery    prometheus.Gauge
	phaseDuration      *prometheus.GaugeVec
	queue              prometheus.Gauge
	capacity           prometheus.Gauge
	etcdReadSeconds    prometheus.Histogram
	etcdReadFailures   prometheus.Counter
	etcdReadUp         prometheus.Gauge
	etcdStatusSeconds  prometheus.Histogram
	etcdStatusFailures prometheus.Counter
	etcdStatusUp       prometheus.Gauge
	leader             prometheus.Gauge
}

func (s *Server) ensureMetrics() *serviceMetrics {
	s.metricsOnce.Do(func() {
		m := &serviceMetrics{registry: prometheus.NewRegistry(),
			ack:                prometheus.NewHistogramVec(prometheus.HistogramOpts{Name: "dbha_report_ack_seconds", Help: "Duration to durable ACK or report rejection.", Buckets: []float64{.005, .01, .025, .05, .1, .25, .5, 1, 2, 5}}, []string{"stream", "result"}),
			duplicates:         prometheus.NewCounter(prometheus.CounterOpts{Name: "dbha_duplicate_reports_total", Help: "Idempotent duplicate reports."}),
			rejections:         prometheus.NewCounterVec(prometheus.CounterOpts{Name: "dbha_report_rejections_total", Help: "Rejected discovery reports by bounded reason code."}, []string{"stream", "code"}),
			http:               prometheus.NewCounterVec(prometheus.CounterOpts{Name: "dbha_http_requests_total", Help: "Wrapped HTTP requests by fixed endpoint category and outcome."}, []string{"endpoint", "code"}),
			registrations:      prometheus.NewCounterVec(prometheus.CounterOpts{Name: "dbha_registration_requests_total", Help: "Agent registration success or refusal by bounded reason code."}, []string{"code"}),
			routeUnavailable:   prometheus.NewCounter(prometheus.CounterOpts{Name: "dbha_route_not_ready_total", Help: "Route requests rejected because no verified route was ready."}),
			states:             prometheus.NewGaugeVec(prometheus.GaugeOpts{Name: "dbha_cluster_state", Help: "Current cluster state by dimension."}, []string{"cluster_id", "dimension", "state"}),
			age:                prometheus.NewGaugeVec(prometheus.GaugeOpts{Name: "dbha_observation_age_seconds", Help: "Age of the sampled-at timestamp on the last retained topology observation, clamped to zero for clock skew."}, []string{"instance_id"}),
			agents:             prometheus.NewGaugeVec(prometheus.GaugeOpts{Name: "dbha_agents", Help: "Registered agent counts by kind and session state."}, []string{"kind", "state"}),
			instances:          prometheus.NewGaugeVec(prometheus.GaugeOpts{Name: "dbha_instances", Help: "Instance counts by kind and admission state."}, []string{"kind", "admission"}),
			pendingRecovery:    prometheus.NewGauge(prometheus.GaugeOpts{Name: "dbha_recovery_pending_operations", Help: "Nonterminal operations in clusters requiring manual recovery."}),
			phaseDuration:      prometheus.NewGaugeVec(prometheus.GaugeOpts{Name: "dbha_operation_retained_phase_duration_seconds", Help: "Sum of persisted phase residence time for retained operations; includes waiting, not SQL execution time."}, []string{"phase"}),
			queue:              prometheus.NewGauge(prometheus.GaugeOpts{Name: "dbha_ingest_pending", Help: "Current in-flight ingest calls; calls are rejected at the configured bound."}),
			capacity:           prometheus.NewGauge(prometheus.GaugeOpts{Name: "dbha_etcd_used_ratio", Help: "etcd physical database size divided by configured quota at last successful metrics scrape."}),
			etcdReadSeconds:    prometheus.NewHistogram(prometheus.HistogramOpts{Name: "dbha_metrics_etcd_read_seconds", Help: "Duration of the metrics scrape State.Read call only."}),
			etcdReadFailures:   prometheus.NewCounter(prometheus.CounterOpts{Name: "dbha_metrics_etcd_read_failures_total", Help: "Failed metrics scrape State.Read calls."}),
			etcdReadUp:         prometheus.NewGauge(prometheus.GaugeOpts{Name: "dbha_metrics_etcd_read_up", Help: "Whether the last metrics scrape could read etcd state."}),
			etcdStatusSeconds:  prometheus.NewHistogram(prometheus.HistogramOpts{Name: "dbha_metrics_etcd_status_seconds", Help: "Duration of the metrics scrape etcd Status call only."}),
			etcdStatusFailures: prometheus.NewCounter(prometheus.CounterOpts{Name: "dbha_metrics_etcd_status_failures_total", Help: "Failed metrics scrape etcd Status calls."}),
			etcdStatusUp:       prometheus.NewGauge(prometheus.GaugeOpts{Name: "dbha_metrics_etcd_status_up", Help: "Whether the last metrics scrape could query etcd Status."}),
			leader:             prometheus.NewGauge(prometheus.GaugeOpts{Name: "dbha_server_leader", Help: "Whether this dbha-server currently owns the control plane."}),
		}
		m.leader.Set(1)
		m.registry.MustRegister(m.ack, m.duplicates, m.rejections, m.http, m.registrations, m.routeUnavailable, m.states, m.age, m.agents, m.instances, m.pendingRecovery, m.phaseDuration, m.queue, m.capacity, m.etcdReadSeconds, m.etcdReadFailures, m.etcdReadUp, m.etcdStatusSeconds, m.etcdStatusFailures, m.etcdStatusUp, m.leader, collectors.NewGoCollector(), collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}))
		s.metrics = m
	})
	return s.metrics
}

// metricCode exposes only a bounded vocabulary, never request paths or messages.
func metricCode(err error) string {
	if err == nil {
		return "OK"
	}
	var e *Error
	if !errors.As(err, &e) {
		return "STORAGE_UNAVAILABLE"
	}
	switch e.Code {
	case "UNAUTHORIZED", "INVALID_REGISTER", "INVALID_BOOT_ID", "BOOT_SUPERSEDED", "BOOT_CONFLICT", "SESSION_ACTIVE", "SESSION_CLOSED", "SESSION_EXPIRED", "OUT_OF_ORDER", "SEQUENCE_CONFLICT", "STALE_SAMPLE", "CLOCK_SKEW", "REPORT_TOO_LARGE", "INGEST_BACKPRESSURE", "INVALID_ENVELOPE", "INVALID_STREAM", "INVALID_PAYLOAD", "INVALID_DURATION", "INVALID_OBSERVATION", "INSTANCE_UNKNOWN", "INSTANCE_RETIRED", "KIND_FORBIDDEN", "IDENTITY_MISSING", "IDENTITY_CONFLICT", "ENDPOINT_CONFLICT", "CROSS_DEPLOYMENT_IDENTITY", "CROSS_DEPLOYMENT_ENDPOINT", "ADDRESS_UNRESOLVED", "ADDRESS_FORBIDDEN", "ROUTE_NOT_READY", "ETCD_NOSPACE", "ETCD_CLUSTER_CHANGED", "OWNER_LOST", "CAS_CONFLICT", "TXN_TOO_LARGE":
		return e.Code
	default:
		return "OTHER"
	}
}

func httpMetricEndpoint(path string) string {
	if path == "/api/v1/agents/register" {
		return "register"
	}
	if strings.HasPrefix(path, "/api/v1/clusters/") && strings.HasSuffix(path, "/route") {
		return "route"
	}
	return "other"
}

func (s *Server) observeHTTP(path string, err error) {
	m := s.ensureMetrics()
	endpoint := httpMetricEndpoint(path)
	code := metricCode(err)
	m.http.WithLabelValues(endpoint, code).Inc()
	if endpoint == "register" {
		m.registrations.WithLabelValues(code).Inc()
	}
	if endpoint == "route" && code == "ROUTE_NOT_READY" {
		m.routeUnavailable.Inc()
	}
}

func (s *Server) serveMetrics(w http.ResponseWriter, r *http.Request) {
	m := s.ensureMetrics()
	m.mu.Lock()
	defer m.mu.Unlock()
	start := time.Now()
	st, err := s.Store.Read(r.Context())
	m.etcdReadSeconds.Observe(time.Since(start).Seconds())
	if err != nil {
		m.etcdReadUp.Set(0)
		m.etcdReadFailures.Inc()
		http.Error(w, "storage unavailable", http.StatusServiceUnavailable)
		return
	}
	m.etcdReadUp.Set(1)
	m.states.Reset()
	m.age.Reset()
	m.agents.Reset()
	m.instances.Reset()
	m.phaseDuration.Reset()
	for _, c := range st.Clusters {
		for dimension, value := range map[string]string{"topology": c.TopologyState, "health": c.HealthState, "operation": c.OperationState, "recovery": c.RecoveryGate} {
			m.states.WithLabelValues(clusterKey(c.ID), dimension, value).Set(1)
		}
	}
	for id, report := range st.Observations {
		age := time.Since(report.Envelope.SampledAt).Seconds()
		if age < 0 {
			age = 0
		}
		m.age.WithLabelValues(id).Set(age)
	}
	for _, a := range st.Agents {
		kind := a.AllowedKind
		if kind != "mysql" && kind != "proxy" {
			kind = "other"
		}
		state := "inactive"
		if st.Credentials[a.CredentialID].Revoked {
			state = "revoked"
		} else if a.SessionID != "" && !a.SessionClosed && time.Since(a.LastContactAt) < 30*time.Second {
			state = "active"
		}
		m.agents.WithLabelValues(kind, state).Inc()
	}
	for _, i := range st.Instances {
		kind := i.Kind
		if kind != "mysql" && kind != "proxy" {
			kind = "other"
		}
		admission := i.Admission
		switch admission {
		case "CANDIDATE", "ACTIVE", "RETURNED_UNVERIFIED", "QUARANTINED", "RETIRED":
		default:
			admission = "UNKNOWN"
		}
		m.instances.WithLabelValues(kind, admission).Inc()
	}
	pending := 0
	for _, op := range st.Operations {
		for _, phase := range []string{"PREPARED", "ROUTES_BLOCKED", "PROMOTED", "ROUTED"} {
			if ms := op.PhaseDurationsMS[phase]; ms > 0 {
				m.phaseDuration.WithLabelValues(phase).Add(float64(ms) / 1000)
			}
		}
		if op.CompletedAt.IsZero() && st.Clusters[clusterKey(op.ClusterID)].OperationState == "RECOVERY_REQUIRED" {
			pending++
		}
	}
	m.pendingRecovery.Set(float64(pending))
	s.Store.mu.Lock()
	m.queue.Set(float64(s.Store.ingestCount))
	s.Store.mu.Unlock()
	start = time.Now()
	dbSize, statusErr := s.Store.MaxDBSize(r.Context())
	m.etcdStatusSeconds.Observe(time.Since(start).Seconds())
	if statusErr != nil {
		m.etcdStatusUp.Set(0)
		m.etcdStatusFailures.Inc()
	} else {
		m.etcdStatusUp.Set(1)
		if s.Config.EtcdQuotaBytes > 0 {
			ratio := float64(dbSize) / float64(s.Config.EtcdQuotaBytes)
			m.capacity.Set(ratio)
			if ratio >= .7 && s.log != nil {
				s.log.Warn("etcd capacity warning", "used_ratio", ratio)
			}
		}
	}
	promhttp.HandlerFor(m.registry, promhttp.HandlerOpts{}).ServeHTTP(w, r)
}
