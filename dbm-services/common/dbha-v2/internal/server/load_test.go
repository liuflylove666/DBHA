package server

import (
	"context"
	"errors"
	"fmt"
	"os"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"dbm-services/common/dbha-v2/pkg/discovery"
	"github.com/google/uuid"
)

type loadAgent struct {
	id         string
	instanceID string
	token      string
	session    discovery.RegisterResponse
	jobs       chan uint64
}

type loadStats struct {
	attempts atomic.Uint64
	success  atomic.Uint64
	errors   atomic.Uint64
	timeouts atomic.Uint64
	dropped  atomic.Uint64

	mu        sync.Mutex
	latencies []time.Duration
	errorText map[string]uint64
}

func TestEtcdIngestLoad(t *testing.T) {
	seconds := loadEnvInt(t, "DBHA_LOAD_SECONDS", 0)
	if seconds == 0 {
		t.Skip("set DBHA_LOAD_SECONDS to run the etcd ingest load smoke test")
	}
	if os.Getenv("DBHA_TEST_ETCD") == "" {
		t.Fatal("DBHA_TEST_ETCD is required when DBHA_LOAD_SECONDS is set")
	}
	rate := loadEnvInt(t, "DBHA_LOAD_RATE", 100)
	instances := loadEnvInt(t, "DBHA_LOAD_INSTANCES", 30)
	if rate < 1 || rate > 10_000 {
		t.Fatalf("DBHA_LOAD_RATE must be between 1 and 10000, got %d", rate)
	}
	if instances < 2 || instances%2 != 0 {
		t.Fatalf("DBHA_LOAD_INSTANCES must be a positive even number of at least 2, got %d", instances)
	}

	store, ctx := integrationStore(t)
	agents := prepareLoadAgents(t, ctx, store, instances)
	stats := &loadStats{errorText: make(map[string]uint64)}
	var workers sync.WaitGroup
	for i := range agents {
		workers.Add(1)
		go runLoadAgent(store, &agents[i], i, stats, &workers)
	}

	var before runtime.MemStats
	runtime.ReadMemStats(&before)
	started := time.Now()
	deadline := started.Add(time.Duration(seconds) * time.Second)
	ticker := time.NewTicker(time.Second / time.Duration(rate))
	defer ticker.Stop()
	index := 0
	for now := range ticker.C {
		if !now.Before(deadline) {
			break
		}
		stats.attempts.Add(1)
		agent := &agents[index%len(agents)]
		sequence := uint64(index/len(agents) + 1)
		select {
		case agent.jobs <- sequence:
		default:
			stats.dropped.Add(1)
		}
		index++
	}
	for i := range agents {
		close(agents[i].jobs)
	}
	workers.Wait()
	elapsed := time.Since(started)

	var after runtime.MemStats
	runtime.ReadMemStats(&after)
	p50, p95, p99 := stats.percentiles()
	attempts := stats.attempts.Load()
	success := stats.success.Load()
	failed := stats.errors.Load()
	timeouts := stats.timeouts.Load()
	dropped := stats.dropped.Load()
	accounted := success + failed + timeouts + dropped
	if accounted != attempts {
		t.Errorf("load result accounting mismatch: attempts=%d accounted=%d", attempts, accounted)
	}
	t.Logf("load result: duration=%s instances=%d target_rate=%d/s attempts=%d success=%d errors=%d timeouts=%d dropped=%d attempted_rate=%.2f/s success_rate=%.2f/s p50=%s p95=%s p99=%s heap_start=%d heap_end=%d heap_delta=%d rss=%s",
		elapsed.Round(time.Millisecond), instances, rate, attempts, success, failed, timeouts, dropped,
		float64(attempts)/elapsed.Seconds(), float64(success)/elapsed.Seconds(), p50, p95, p99,
		before.HeapAlloc, after.HeapAlloc, int64(after.HeapAlloc)-int64(before.HeapAlloc), processRSS())
	for _, line := range stats.errorsByFrequency() {
		t.Logf("load error: %s", line)
	}
}

func loadEnvInt(t *testing.T, name string, fallback int) int {
	t.Helper()
	raw := os.Getenv(name)
	if raw == "" {
		return fallback
	}
	value, err := strconv.Atoi(raw)
	if err != nil || value <= 0 {
		t.Fatalf("%s must be a positive integer, got %q", name, raw)
	}
	return value
}

func prepareLoadAgents(t *testing.T, ctx context.Context, store *Store, count int) []loadAgent {
	t.Helper()
	agents := make([]loadAgent, 0, count)
	for i := 0; i < count; i += 2 {
		deployment, err := store.CreateDeployment(ctx, DeploymentRequest{IdempotencyKey: fmt.Sprintf("load-%d-%s", i/2, uuid.NewString())})
		if err != nil {
			t.Fatalf("create load deployment %d: %v", i/2, err)
		}
		for member := 0; member < 2; member++ {
			ordinal := i + member
			agentID := uuid.NewString()
			_, token, err := store.IssueAgent(ctx, deployment.ID, agentID, "proxy")
			if err != nil {
				t.Fatalf("issue load agent %d: %v", ordinal, err)
			}
			session, _, err := store.Register(ctx, token, discovery.RegisterRequest{SchemaVersion: discovery.SchemaVersion, AgentID: agentID, BootID: uuid.NewString(), BootGeneration: 1, ProbeVersion: "load-smoke"})
			if err != nil {
				t.Fatalf("register load agent %d: %v", ordinal, err)
			}
			proxyID := uuid.NewString()
			payload := []byte(fmt.Sprintf(`{"kind":"proxy","advertise_host":"127.0.0.1","proxy_uuid":%q,"data_port":%d,"admin_port":%d,"process_state":"STARTING","backend_query_state":"NOT_READY","collection_state":"OK"}`, proxyID, 20_000+ordinal, 30_000+ordinal))
			ack, err := store.Ingest(ctx, token, discovery.Envelope{SchemaVersion: discovery.SchemaVersion, AgentID: agentID, SessionID: session.SessionID, SessionEpoch: session.SessionEpoch, Stream: "topology", Sequence: 1, SampledAt: time.Now().UTC(), Payload: payload})
			if err != nil {
				t.Fatalf("bootstrap load agent %d: %v", ordinal, err)
			}
			agents = append(agents, loadAgent{id: agentID, instanceID: ack.InstanceID, token: token, session: session, jobs: make(chan uint64, 1)})
		}
	}
	return agents
}

func runLoadAgent(store *Store, agent *loadAgent, ordinal int, stats *loadStats, workers *sync.WaitGroup) {
	defer workers.Done()
	for sequence := range agent.jobs {
		targetBytes := 1024 + int((sequence*1_103_515_245+uint64(ordinal)*12_345)%3073)
		payload := loadPayload(targetBytes)
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		started := time.Now()
		_, err := store.Ingest(ctx, agent.token, discovery.Envelope{
			SchemaVersion: discovery.SchemaVersion, AgentID: agent.id, SessionID: agent.session.SessionID,
			SessionEpoch: agent.session.SessionEpoch, InstanceID: agent.instanceID, Stream: "default",
			Sequence: sequence, SampledAt: time.Now().UTC(), Payload: payload,
		})
		latency := time.Since(started)
		cancel()
		stats.record(latency, err)
	}
}

func loadPayload(size int) []byte {
	const prefix = `{"collection_state":"OK","data":"`
	const suffix = `"}`
	return []byte(prefix + strings.Repeat("x", size-len(prefix)-len(suffix)) + suffix)
}

func (s *loadStats) record(latency time.Duration, err error) {
	s.mu.Lock()
	s.latencies = append(s.latencies, latency)
	if err != nil {
		s.errorText[err.Error()]++
	}
	s.mu.Unlock()
	if err == nil {
		s.success.Add(1)
	} else if errors.Is(err, context.DeadlineExceeded) {
		s.timeouts.Add(1)
	} else {
		s.errors.Add(1)
	}
}

func (s *loadStats) percentiles() (time.Duration, time.Duration, time.Duration) {
	s.mu.Lock()
	values := append([]time.Duration(nil), s.latencies...)
	s.mu.Unlock()
	if len(values) == 0 {
		return 0, 0, 0
	}
	sort.Slice(values, func(i, j int) bool { return values[i] < values[j] })
	at := func(percent int) time.Duration {
		return values[(len(values)*percent+99)/100-1]
	}
	return at(50), at(95), at(99)
}

func (s *loadStats) errorsByFrequency() []string {
	s.mu.Lock()
	lines := make([]string, 0, len(s.errorText))
	for message, count := range s.errorText {
		lines = append(lines, fmt.Sprintf("count=%d error=%q", count, message))
	}
	s.mu.Unlock()
	sort.Strings(lines)
	return lines
}

func processRSS() string {
	var usage syscall.Rusage
	if syscall.Getrusage(syscall.RUSAGE_SELF, &usage) != nil {
		return "unavailable"
	}
	bytes := int64(usage.Maxrss)
	if runtime.GOOS == "linux" {
		bytes *= 1024
	}
	return strconv.FormatInt(bytes, 10)
}
