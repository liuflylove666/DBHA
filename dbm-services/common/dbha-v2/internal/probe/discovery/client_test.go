package discovery

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	contract "dbm-services/common/dbha-v2/pkg/discovery"
	"dbm-services/common/dbha-v2/pkg/proto"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

type plainDiscoveryServer struct {
	proto.UnimplementedDiscoveryServiceServer
}

type failoverRPC struct {
	err   error
	calls int
}

func (r *failoverRPC) PushObservation(_ context.Context, report *proto.DiscoveryReport, _ ...grpc.CallOption) (*proto.DiscoveryAck, error) {
	r.calls++
	if r.err != nil {
		return nil, r.err
	}
	return &proto.DiscoveryAck{InstanceId: report.InstanceId, AcceptedSequence: report.Sequence, StoredRevision: 9}, nil
}

func (r *failoverRPC) PushSample(ctx context.Context, report *proto.DiscoveryReport, opts ...grpc.CallOption) (*proto.DiscoveryAck, error) {
	return r.PushObservation(ctx, report, opts...)
}

func (plainDiscoveryServer) PushSample(ctx context.Context, report *proto.DiscoveryReport) (*proto.DiscoveryAck, error) {
	values := metadata.ValueFromIncomingContext(ctx, "authorization")
	if len(values) != 1 || values[0] != "Bearer test-token" {
		return nil, status.Error(codes.Unauthenticated, "UNAUTHORIZED")
	}
	return &proto.DiscoveryAck{InstanceId: report.InstanceId, AcceptedSequence: report.Sequence, StoredRevision: 7}, nil
}

func TestNewClientUsesPlainHTTPAndGRPC(t *testing.T) {
	httpServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer test-token" {
			t.Fatal("HTTP bearer token missing")
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"session_id": "session-1", "session_epoch": 1}})
	}))
	defer httpServer.Close()

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	grpcServer := grpc.NewServer()
	proto.RegisterDiscoveryServiceServer(grpcServer, plainDiscoveryServer{})
	go func() { _ = grpcServer.Serve(listener) }()
	t.Cleanup(func() { grpcServer.Stop(); _ = listener.Close() })

	tokenFile := filepath.Join(t.TempDir(), "agent.token")
	if err = os.WriteFile(tokenFile, []byte("test-token\n"), 0600); err != nil {
		t.Fatal(err)
	}
	client, err := NewClient(Config{ServerURL: httpServer.URL, ServerGRPC: listener.Addr().String(), TokenFile: tokenFile})
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	session, err := client.Register(ctx, contract.RegisterRequest{})
	if err != nil || session.SessionID != "session-1" {
		t.Fatalf("plain HTTP register failed: %+v %v", session, err)
	}
	ack, err := client.Send(ctx, false, contract.Envelope{InstanceID: "proxy:test", Sequence: 3})
	if err != nil || ack.StoredRevision != 7 || ack.AcceptedSequence != 3 {
		t.Fatalf("plain gRPC report failed: %+v %v", ack, err)
	}
}

func TestHTTPFailoverCachesSuccessfulEndpoint(t *testing.T) {
	var followerCalls, leaderCalls int
	follower := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		followerCalls++
		w.WriteHeader(http.StatusServiceUnavailable)
		_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{"code": "NOT_LEADER"}})
	}))
	defer follower.Close()
	leader := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		leaderCalls++
		_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"session_id": "session-1", "session_epoch": 1}})
	}))
	defer leader.Close()

	c := &Client{HTTP: http.DefaultClient, Token: "test-token", endpoints: []endpoint{{httpBase: follower.URL}, {httpBase: leader.URL}}}
	for i := 0; i < 2; i++ {
		if _, err := c.Register(context.Background(), contract.RegisterRequest{}); err != nil {
			t.Fatal(err)
		}
	}
	if followerCalls != 1 || leaderCalls != 2 || c.preferred.Load() != 1 {
		t.Fatalf("successful endpoint not cached: follower=%d leader=%d preferred=%d", followerCalls, leaderCalls, c.preferred.Load())
	}
}

func TestHTTPBusinessErrorDoesNotFailOver(t *testing.T) {
	var secondCalls int
	first := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{"code": "UNAUTHORIZED"}})
	}))
	defer first.Close()
	second := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { secondCalls++; w.WriteHeader(http.StatusCreated) }))
	defer second.Close()
	c := &Client{HTTP: http.DefaultClient, endpoints: []endpoint{{httpBase: first.URL}, {httpBase: second.URL}}}
	_, err := c.Register(context.Background(), contract.RegisterRequest{})
	statusErr, ok := err.(*HTTPStatusError)
	if !ok || statusErr.Status != http.StatusUnauthorized || secondCalls != 0 {
		t.Fatalf("business error incorrectly failed over: err=%v second calls=%d", err, secondCalls)
	}
}

func TestHTTPStorageUnavailableDoesNotHideRootCause(t *testing.T) {
	var secondCalls int
	first := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
		_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{"code": "ETCD_NOSPACE"}})
	}))
	defer first.Close()
	second := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { secondCalls++ }))
	defer second.Close()
	c := &Client{HTTP: http.DefaultClient, endpoints: []endpoint{{httpBase: first.URL}, {httpBase: second.URL}}}
	_, err := c.Register(context.Background(), contract.RegisterRequest{})
	statusErr, ok := err.(*HTTPStatusError)
	if !ok || statusErr.Code != "ETCD_NOSPACE" || secondCalls != 0 {
		t.Fatalf("storage error was hidden: err=%v second calls=%d", err, secondCalls)
	}
}

func TestGRPCFailoverCachesSuccessfulEndpoint(t *testing.T) {
	follower := &failoverRPC{err: status.Error(codes.Unavailable, "NOT_LEADER")}
	leader := &failoverRPC{}
	c := &Client{Token: "test-token", endpoints: []endpoint{{rpc: follower}, {rpc: leader}}}
	report := contract.Envelope{InstanceID: "proxy:test", Sequence: 3}
	for i := 0; i < 2; i++ {
		ack, err := c.Send(context.Background(), false, report)
		if err != nil || ack.StoredRevision != 9 {
			t.Fatalf("gRPC failover failed: %+v %v", ack, err)
		}
	}
	if follower.calls != 1 || leader.calls != 2 || c.preferred.Load() != 1 {
		t.Fatalf("successful gRPC endpoint not cached: follower=%d leader=%d preferred=%d", follower.calls, leader.calls, c.preferred.Load())
	}
}

func TestGRPCBusinessErrorDoesNotFailOver(t *testing.T) {
	first := &failoverRPC{err: status.Error(codes.InvalidArgument, "bad report")}
	second := &failoverRPC{}
	c := &Client{endpoints: []endpoint{{rpc: first}, {rpc: second}}}
	_, err := c.Send(context.Background(), false, contract.Envelope{})
	if status.Code(err) != codes.InvalidArgument || second.calls != 0 {
		t.Fatalf("business error incorrectly failed over: err=%v second calls=%d", err, second.calls)
	}
}

func TestGRPCStorageUnavailableDoesNotHideRootCause(t *testing.T) {
	first := &failoverRPC{err: status.Error(codes.Unavailable, "STORAGE_UNAVAILABLE")}
	second := &failoverRPC{err: status.Error(codes.Unavailable, "NOT_LEADER")}
	c := &Client{endpoints: []endpoint{{rpc: first}, {rpc: second}}}
	_, err := c.Send(context.Background(), false, contract.Envelope{})
	if status.Convert(err).Message() != "STORAGE_UNAVAILABLE" || second.calls != 0 {
		t.Fatalf("storage error was hidden: err=%v second calls=%d", err, second.calls)
	}
}
