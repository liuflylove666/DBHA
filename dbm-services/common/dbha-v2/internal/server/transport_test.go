package server

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"dbm-services/common/dbha-v2/pkg/discovery"
	"dbm-services/common/dbha-v2/pkg/proto"
	"github.com/google/uuid"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

func TestPlainHTTPAndGRPCTransport(t *testing.T) {
	s := &Server{}
	httpServer := httptest.NewServer(s.Handler())
	defer httpServer.Close()
	response, err := http.Get(httpServer.URL + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("plain HTTP health status %d", response.StatusCode)
	}

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	grpcServer := s.grpcServer(nil)
	go func() { _ = grpcServer.Serve(listener) }()
	t.Cleanup(func() { grpcServer.Stop(); _ = listener.Close() })
	conn, err := grpc.NewClient(listener.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err = proto.NewDiscoveryServiceClient(conn).PushSample(ctx, &proto.DiscoveryReport{})
	if status.Code(err) != codes.Unauthenticated {
		t.Fatalf("plain gRPC did not reach authenticated handler: %v", err)
	}
}

func TestTLSHTTPAndGRPCDiscoveryPersistence(t *testing.T) {
	store, ctx := integrationStore(t)
	adminToken, err := newToken()
	if err != nil {
		t.Fatal(err)
	}
	if err = store.InitAdmin(ctx, adminToken); err != nil {
		t.Fatal(err)
	}
	s := &Server{Store: store, Config: Config{AllowedNetworks: []string{"127.0.0.0/8"}, Profiles: map[string]CredentialProfile{"default": {}}}, started: time.Now().UTC()}
	httpServer := httptest.NewTLSServer(s.Handler())
	defer httpServer.Close()
	client := httpServer.Client()
	request := func(method, path, bearer string, body any, headers map[string]string) (int, json.RawMessage) {
		t.Helper()
		var payload []byte
		if body != nil {
			var e error
			payload, e = json.Marshal(body)
			if e != nil {
				t.Fatal(e)
			}
		}
		req, e := http.NewRequestWithContext(ctx, method, httpServer.URL+path, bytes.NewReader(payload))
		if e != nil {
			t.Fatal(e)
		}
		if bearer != "" {
			req.Header.Set("Authorization", "Bearer "+bearer)
		}
		if body != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		for k, v := range headers {
			req.Header.Set(k, v)
		}
		resp, e := client.Do(req)
		if e != nil {
			t.Fatal(e)
		}
		defer resp.Body.Close()
		var wrapped struct {
			Data  json.RawMessage `json:"data"`
			Error any             `json:"error"`
		}
		if e = json.NewDecoder(resp.Body).Decode(&wrapped); e != nil {
			t.Fatal(e)
		}
		return resp.StatusCode, wrapped.Data
	}
	statusCode, raw := request(http.MethodPost, "/api/v1/deployments", adminToken, DeploymentRequest{}, map[string]string{"Idempotency-Key": "tls-transport-test"})
	if statusCode != http.StatusOK {
		t.Fatalf("deployment HTTP status %d", statusCode)
	}
	var deployment Deployment
	if err = json.Unmarshal(raw, &deployment); err != nil || deployment.ClusterID == 0 {
		t.Fatalf("deployment response %s: %v", raw, err)
	}
	agentID := uuid.NewString()
	statusCode, raw = request(http.MethodPost, "/api/v1/deployments/"+deployment.ID+"/agents", adminToken, map[string]string{"agent_id": agentID, "kind": "proxy"}, nil)
	if statusCode != http.StatusOK {
		t.Fatalf("credential HTTP status %d", statusCode)
	}
	var issued struct {
		AgentID string `json:"agent_id"`
		Token   string `json:"token"`
	}
	if err = json.Unmarshal(raw, &issued); err != nil || issued.AgentID != agentID || len(issued.Token) < 64 {
		t.Fatalf("credential response %s: %v", raw, err)
	}
	bootID := uuid.NewString()
	register := discovery.RegisterRequest{SchemaVersion: 1, AgentID: agentID, BootID: bootID, BootGeneration: 1}
	statusCode, raw = request(http.MethodPost, "/api/v1/agents/register", issued.Token, register, nil)
	if statusCode != http.StatusCreated {
		t.Fatalf("first registration status=%d body=%s", statusCode, raw)
	}
	var session discovery.RegisterResponse
	if err = json.Unmarshal(raw, &session); err != nil || session.SessionID == "" || session.SessionEpoch == 0 {
		t.Fatalf("register response %s: %v", raw, err)
	}
	statusCode, raw = request(http.MethodPost, "/api/v1/agents/register", issued.Token, register, nil)
	if statusCode != http.StatusOK {
		t.Fatalf("registration retry status=%d body=%s", statusCode, raw)
	}
	var retry discovery.RegisterResponse
	if err = json.Unmarshal(raw, &retry); err != nil || retry != session {
		t.Fatalf("retry changed session %s: %v", raw, err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	grpcTLS := &tls.Config{Certificates: httpServer.TLS.Certificates, MinVersion: tls.VersionTLS12}
	grpcServer := s.grpcServer(grpcTLS)
	go func() { _ = grpcServer.Serve(listener) }()
	t.Cleanup(func() { grpcServer.Stop(); _ = listener.Close() })
	pool := x509.NewCertPool()
	pool.AddCert(httpServer.Certificate())
	dialCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	conn, err := grpc.NewClient(listener.Addr().String(), grpc.WithTransportCredentials(credentials.NewTLS(&tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12})))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	clientRPC := proto.NewDiscoveryServiceClient(conn)
	observation := discovery.Observation{Kind: "proxy", ProxyUUID: uuid.NewString(), AdvertiseHost: "127.0.0.1", DataPort: 3306, AdminPort: 3307, ProcessState: "STARTING", CollectionState: "OK"}
	observationJSON, _ := json.Marshal(observation)
	env := discovery.Envelope{SchemaVersion: 1, AgentID: agentID, SessionID: session.SessionID, SessionEpoch: session.SessionEpoch, Stream: "topology", Sequence: 1, SampledAt: time.Now().UTC(), Payload: observationJSON}
	unauthCtx := dialCtx
	if _, err = clientRPC.PushObservation(unauthCtx, env.ToProto()); status.Code(err) != codes.Unauthenticated {
		t.Fatalf("unauthenticated grpc accepted: %v", err)
	}
	badCtx := metadata.AppendToOutgoingContext(dialCtx, "authorization", "Bearer invalid-token")
	if _, err = clientRPC.PushObservation(badCtx, env.ToProto()); status.Code(err) != codes.Unauthenticated {
		t.Fatalf("invalid bearer accepted: %v", err)
	}
	authedCtx := metadata.AppendToOutgoingContext(dialCtx, "authorization", "Bearer "+issued.Token)
	ack, err := clientRPC.PushObservation(authedCtx, env.ToProto())
	if err != nil || ack.Duplicate || ack.StoredRevision == 0 || ack.InstanceId == "" {
		t.Fatalf("first durable grpc ACK %+v: %v", ack, err)
	}
	stored, err := store.Read(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if report, ok := stored.Observations[ack.InstanceId]; !ok || report.StoredRevision != ack.StoredRevision || report.Envelope.SessionID != session.SessionID {
		t.Fatal("ACK was not backed by persisted observation")
	}
	beforeRevision := stored.Revision
	duplicate, err := clientRPC.PushObservation(authedCtx, env.ToProto())
	if err != nil || !duplicate.Duplicate || duplicate.StoredRevision != ack.StoredRevision {
		t.Fatalf("duplicate grpc ACK %+v: %v", duplicate, err)
	}
	stored, err = store.Read(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Revision != beforeRevision {
		t.Fatalf("duplicate changed durable revision %d to %d", beforeRevision, stored.Revision)
	}
	statusCode, _ = request(http.MethodPost, "/api/v1/agents/session-close", issued.Token, map[string]string{"agent_id": agentID, "session_id": session.SessionID}, nil)
	if statusCode != http.StatusOK {
		t.Fatalf("session close HTTP status=%d", statusCode)
	}
	register.BootID = uuid.NewString()
	register.BootGeneration = 2
	statusCode, raw = request(http.MethodPost, "/api/v1/agents/register", issued.Token, register, nil)
	if statusCode != http.StatusCreated {
		t.Fatalf("new boot HTTP status=%d body=%s", statusCode, raw)
	}
	var newSession discovery.RegisterResponse
	if err = json.Unmarshal(raw, &newSession); err != nil || newSession.SessionEpoch <= session.SessionEpoch {
		t.Fatalf("new boot session %s: %v", raw, err)
	}
	env.Sequence = 2
	env.SampledAt = time.Now().UTC()
	if _, err = clientRPC.PushObservation(authedCtx, env.ToProto()); status.Code(err) != codes.Aborted {
		t.Fatalf("superseded session accepted: %v", err)
	}
	stored, err = store.Read(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Agents[agentID].SessionID != newSession.SessionID || stored.Observations[ack.InstanceId].Envelope.SessionID != session.SessionID {
		t.Fatal("stale session overwrote durable report")
	}
}
