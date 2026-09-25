package discovery

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"dbm-services/common/dbha-v2/pkg/proto"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type registerTransport struct{ requests []map[string]any }

func (rt *registerTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	b, err := io.ReadAll(req.Body)
	if err != nil {
		return nil, err
	}
	var body map[string]any
	if err := json.Unmarshal(b, &body); err != nil {
		return nil, err
	}
	rt.requests = append(rt.requests, body)
	response := `{"data":{"session_id":"new-session","session_epoch":2}}`
	return &http.Response{StatusCode: http.StatusCreated, Body: io.NopCloser(strings.NewReader(response)), Header: http.Header{}}, nil
}

type expiringRPC struct{ reports []*proto.DiscoveryReport }

func (r *expiringRPC) PushObservation(_ context.Context, p *proto.DiscoveryReport, _ ...grpc.CallOption) (*proto.DiscoveryAck, error) {
	r.reports = append(r.reports, p)
	if len(r.reports) == 1 {
		return nil, status.Error(codes.Aborted, "SESSION_EXPIRED")
	}
	return &proto.DiscoveryAck{InstanceId: "proxy:bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbb3", AcceptedSequence: p.Sequence}, nil
}

type staleOnceRPC struct{ reports []*proto.DiscoveryReport }

func (r *staleOnceRPC) PushObservation(_ context.Context, p *proto.DiscoveryReport, _ ...grpc.CallOption) (*proto.DiscoveryAck, error) {
	r.reports = append(r.reports, p)
	if len(r.reports) == 1 {
		return nil, status.Error(codes.FailedPrecondition, "STALE_SAMPLE")
	}
	return &proto.DiscoveryAck{InstanceId: "proxy:bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbb3", AcceptedSequence: p.Sequence}, nil
}

func (r *staleOnceRPC) PushSample(_ context.Context, p *proto.DiscoveryReport, _ ...grpc.CallOption) (*proto.DiscoveryAck, error) {
	return r.PushObservation(context.Background(), p)
}
func (r *expiringRPC) PushSample(_ context.Context, p *proto.DiscoveryReport, _ ...grpc.CallOption) (*proto.DiscoveryAck, error) {
	r.reports = append(r.reports, p)
	return nil, status.Error(codes.Aborted, "SESSION_EXPIRED")
}

func TestExpiredSessionCreatesDurableBootAndFreshTopology(t *testing.T) {
	path := filepath.Join(t.TempDir(), "identity.json")
	state, err := StartBoot(path, "agent-1")
	if err != nil {
		t.Fatal(err)
	}
	state.SessionID = "old-session"
	state.SessionEpoch = 7
	state.Sequences["topology"] = 3
	if err := SaveState(path, state); err != nil {
		t.Fatal(err)
	}
	transport := &registerTransport{}
	rpc := &expiringRPC{}
	r := &Runner{Config: Config{AgentID: "agent-1", StateFile: path, Kind: "proxy", AdvertiseHost: "10.0.0.3", Proxy: ProxyConfig{UUID: "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbb3", DataPort: 10000, AdminPort: 11000}}, State: state, Client: &Client{HTTP: &http.Client{Transport: transport}, HTTPBase: "https://test.invalid", RPC: rpc, Token: "secret"}, InstanceID: "proxy:bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbb3"}
	if err := r.report(context.Background(), "topology"); err != nil {
		t.Fatal(err)
	}
	if len(rpc.reports) != 2 || rpc.reports[0].SessionId != "old-session" || rpc.reports[0].Sequence != 4 || rpc.reports[1].SessionId != "new-session" || rpc.reports[1].Sequence != 1 {
		t.Fatalf("stale packet replayed instead of fresh session: %+v", rpc.reports)
	}
	oldAt, err := time.Parse(time.RFC3339Nano, rpc.reports[0].SampledAt)
	if err != nil {
		t.Fatal(err)
	}
	newAt, err := time.Parse(time.RFC3339Nano, rpc.reports[1].SampledAt)
	if err != nil {
		t.Fatal(err)
	}
	if newAt.Before(oldAt) {
		t.Fatal("recovery sent older sample")
	}
	if len(transport.requests) != 1 || transport.requests[0]["boot_generation"] != float64(2) || transport.requests[0]["boot_id"] == state.BootID {
		t.Fatalf("new boot not registered: %+v", transport.requests)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var saved State
	if err := json.Unmarshal(b, &saved); err != nil {
		t.Fatal(err)
	}
	if saved.BootGeneration != 2 || saved.SessionID != "new-session" || saved.Sequences["topology"] != 1 {
		t.Fatalf("recovery state not durable: %+v", saved)
	}
}

func TestStaleSampleIsRecollectedWithoutTerminatingProbe(t *testing.T) {
	path := filepath.Join(t.TempDir(), "identity.json")
	state, err := StartBoot(path, "agent-1")
	if err != nil {
		t.Fatal(err)
	}
	state.SessionID = "session-1"
	state.SessionEpoch = 1
	if err := SaveState(path, state); err != nil {
		t.Fatal(err)
	}
	rpc := &staleOnceRPC{}
	r := &Runner{Config: Config{AgentID: "agent-1", StateFile: path, Kind: "proxy", AdvertiseHost: "10.0.0.3", Proxy: ProxyConfig{UUID: "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbb3", DataPort: 10000, AdminPort: 11000}}, State: state, Client: &Client{RPC: rpc}, InstanceID: "proxy:bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbb3"}
	if err := r.report(context.Background(), "topology"); err != nil {
		t.Fatal(err)
	}
	if len(rpc.reports) != 2 || rpc.reports[0].Sequence != 1 || rpc.reports[1].Sequence != 2 {
		t.Fatalf("stale envelope was not replaced: %+v", rpc.reports)
	}
	first, err := time.Parse(time.RFC3339Nano, rpc.reports[0].SampledAt)
	if err != nil {
		t.Fatal(err)
	}
	second, err := time.Parse(time.RFC3339Nano, rpc.reports[1].SampledAt)
	if err != nil {
		t.Fatal(err)
	}
	if second.Before(first) || r.State.BootGeneration != 1 || r.State.SessionID != "session-1" {
		t.Fatalf("stale recovery changed session or moved time backwards: first=%s second=%s state=%+v", first, second, r.State)
	}
}

func TestSessionExpiredRequiresExactGRPCStatus(t *testing.T) {
	if !sessionExpired(status.Error(codes.Aborted, "SESSION_EXPIRED")) {
		t.Fatal("missed server's session expiration status")
	}
	for _, err := range []error{status.Error(codes.Aborted, "SEQUENCE_CONFLICT"), status.Error(codes.Unavailable, "SESSION_EXPIRED"), errors.New("SESSION_EXPIRED")} {
		if sessionExpired(err) {
			t.Fatalf("unrelated error rotated boot: %v", err)
		}
	}
}

func TestStaleSampleRequiresExactGRPCStatus(t *testing.T) {
	if !staleSample(status.Error(codes.FailedPrecondition, "STALE_SAMPLE")) {
		t.Fatal("missed server stale-sample status")
	}
	for _, err := range []error{status.Error(codes.FailedPrecondition, "CLOCK_SKEW"), status.Error(codes.Aborted, "STALE_SAMPLE"), errors.New("STALE_SAMPLE")} {
		if staleSample(err) {
			t.Fatalf("unrelated error treated as stale sample: %v", err)
		}
	}
}

func TestExpiredSampleDropsOldPayloadAndReportsTopologyFirst(t *testing.T) {
	path := filepath.Join(t.TempDir(), "identity.json")
	state, err := StartBoot(path, "agent-1")
	if err != nil {
		t.Fatal(err)
	}
	state.SessionID = "old-session"
	state.SessionEpoch = 7
	if err := SaveState(path, state); err != nil {
		t.Fatal(err)
	}
	transport := &registerTransport{}
	rpc := &expiringRPC{}
	r := &Runner{Config: Config{AgentID: "agent-1", StateFile: path, Kind: "proxy", AdvertiseHost: "10.0.0.3", Proxy: ProxyConfig{UUID: "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbb3", DataPort: 10000, AdminPort: 11000}}, State: state, Client: &Client{HTTP: &http.Client{Transport: transport}, HTTPBase: "https://test.invalid", RPC: rpc, Token: "secret"}, InstanceID: "proxy:bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbb3"}
	if err := r.report(context.Background(), "default"); err != nil {
		t.Fatal(err)
	}
	if len(rpc.reports) != 2 || rpc.reports[0].Stream != "default" || rpc.reports[1].Stream != "topology" || rpc.reports[1].Sequence != 1 || rpc.reports[1].SessionId != "new-session" {
		t.Fatalf("old sample replayed or topology not first: %+v", rpc.reports)
	}
	if r.State.Sequences["default"] != 0 {
		t.Fatalf("old sample sequence leaked into new session: %+v", r.State.Sequences)
	}
}
