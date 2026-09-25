package discovery

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"sync/atomic"
	"time"

	contract "dbm-services/common/dbha-v2/pkg/discovery"
	"dbm-services/common/dbha-v2/pkg/proto"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

type endpoint struct {
	httpBase string
	conn     *grpc.ClientConn
	rpc      proto.DiscoveryServiceClient
}

type Client struct {
	HTTP      *http.Client
	Conn      *grpc.ClientConn
	RPC       proto.DiscoveryServiceClient
	Token     string
	HTTPBase  string
	endpoints []endpoint
	preferred atomic.Uint32
}

type HTTPStatusError struct {
	Status int
	Code   string
}

func (e *HTTPStatusError) Error() string { return fmt.Sprintf("HTTP %d %s", e.Status, e.Code) }

func NewClient(c Config) (*Client, error) {
	token, err := readSecret(c.TokenFile)
	if err != nil {
		return nil, err
	}
	if token == "" {
		return nil, fmt.Errorf("empty agent token")
	}
	urls, grpcEndpoints, err := c.serverEndpoints()
	if err != nil {
		return nil, err
	}
	client := &Client{Token: token}
	var transport http.RoundTripper = http.DefaultTransport
	for i, raw := range urls {
		u, parseErr := url.Parse(raw)
		if parseErr != nil {
			client.Close()
			return nil, parseErr
		}
		var grpcCredentials credentials.TransportCredentials = insecure.NewCredentials()
		if u.Scheme == "https" {
			pool, poolErr := x509.SystemCertPool()
			if poolErr != nil {
				client.Close()
				return nil, poolErr
			}
			if c.CAFile != "" {
				ca, readErr := os.ReadFile(c.CAFile)
				if readErr != nil {
					client.Close()
					return nil, readErr
				}
				if !pool.AppendCertsFromPEM(ca) {
					client.Close()
					return nil, fmt.Errorf("invalid CA certificate")
				}
			}
			tlsConfig := &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}
			transport = &http.Transport{TLSClientConfig: tlsConfig}
			grpcCredentials = credentials.NewTLS(tlsConfig)
		}
		conn, dialErr := grpc.NewClient(grpcEndpoints[i], grpc.WithTransportCredentials(grpcCredentials), grpc.WithDefaultCallOptions(grpc.MaxCallSendMsgSize(128*1024+4096), grpc.MaxCallRecvMsgSize(64*1024)))
		if dialErr != nil {
			client.Close()
			return nil, dialErr
		}
		client.endpoints = append(client.endpoints, endpoint{httpBase: raw, conn: conn, rpc: proto.NewDiscoveryServiceClient(conn)})
	}
	client.HTTP = &http.Client{Transport: transport, Timeout: 5 * time.Second}
	client.Conn, client.RPC, client.HTTPBase = client.endpoints[0].conn, client.endpoints[0].rpc, client.endpoints[0].httpBase
	return client, nil
}

func (c *Client) Close() error {
	var first error
	if len(c.endpoints) == 0 {
		if c.Conn != nil {
			return c.Conn.Close()
		}
		return nil
	}
	for _, ep := range c.endpoints {
		if ep.conn == nil {
			continue
		}
		if err := ep.conn.Close(); err != nil && first == nil {
			first = err
		}
	}
	return first
}

func (c *Client) Register(ctx context.Context, req contract.RegisterRequest) (contract.RegisterResponse, error) {
	var result contract.RegisterResponse
	b, err := json.Marshal(req)
	if err != nil {
		return result, err
	}
	var last error
	for n := 0; n < c.endpointCount(); n++ {
		i := c.endpointIndex(n)
		base, _ := c.endpointAt(i)
		rsp, callErr := c.doJSON(ctx, base+"/api/v1/agents/register", b)
		if callErr != nil {
			last = callErr
			continue
		}
		var wire struct {
			Data contract.RegisterResponse `json:"data"`
		}
		if rsp.StatusCode == 200 || rsp.StatusCode == 201 {
			decodeErr := json.NewDecoder(io.LimitReader(rsp.Body, 16*1024)).Decode(&wire)
			rsp.Body.Close()
			if decodeErr != nil {
				return result, decodeErr
			}
			if wire.Data.SessionID == "" || wire.Data.SessionEpoch == 0 {
				return result, fmt.Errorf("invalid registration response")
			}
			c.preferred.Store(uint32(i))
			return wire.Data, nil
		}
		statusErr := decodeHTTPStatus(rsp)
		if statusErr.Code != "NOT_LEADER" {
			return result, statusErr
		}
		last = statusErr
	}
	return result, last
}

func (c *Client) CloseSession(ctx context.Context, agentID, sessionID string) error {
	b, err := json.Marshal(map[string]string{"agent_id": agentID, "session_id": sessionID})
	if err != nil {
		return err
	}
	var last error
	for n := 0; n < c.endpointCount(); n++ {
		i := c.endpointIndex(n)
		base, _ := c.endpointAt(i)
		rsp, callErr := c.doJSON(ctx, base+"/api/v1/agents/session-close", b)
		if callErr != nil {
			last = callErr
			continue
		}
		if rsp.StatusCode == 200 || rsp.StatusCode == 204 {
			rsp.Body.Close()
			c.preferred.Store(uint32(i))
			return nil
		}
		statusErr := decodeHTTPStatus(rsp)
		if statusErr.Code != "NOT_LEADER" {
			return statusErr
		}
		last = statusErr
	}
	return last
}

func (c *Client) Send(ctx context.Context, observation bool, report contract.Envelope) (contract.ReportResponse, error) {
	ctx = metadata.AppendToOutgoingContext(ctx, "authorization", "Bearer "+c.Token)
	var last error
	for n := 0; n < c.endpointCount(); n++ {
		i := c.endpointIndex(n)
		_, rpc := c.endpointAt(i)
		var ack *proto.DiscoveryAck
		var err error
		var trailer metadata.MD
		if observation {
			ack, err = rpc.PushObservation(ctx, report.ToProto(), grpc.Trailer(&trailer))
		} else {
			ack, err = rpc.PushSample(ctx, report.ToProto(), grpc.Trailer(&trailer))
		}
		if err == nil {
			c.preferred.Store(uint32(i))
			return contract.ReportResponseFromProto(ack), nil
		}
		if status.Code(err) != codes.Unavailable || len(trailer.Get("dbha-error-code")) != 0 || isServerUnavailableCode(status.Convert(err).Message()) {
			return contract.ReportResponse{}, err
		}
		last = err
	}
	return contract.ReportResponse{}, last
}

func isServerUnavailableCode(message string) bool {
	switch message {
	case "STORAGE_UNAVAILABLE", "OWNER_LOST", "ETCD_NOSPACE", "ETCD_CAPACITY":
		return true
	default:
		return false
	}
}

func (c *Client) endpointCount() int {
	if len(c.endpoints) == 0 {
		return 1
	}
	return len(c.endpoints)
}
func (c *Client) endpointIndex(offset int) int {
	return (int(c.preferred.Load()) + offset) % c.endpointCount()
}
func (c *Client) endpointAt(i int) (string, proto.DiscoveryServiceClient) {
	if len(c.endpoints) == 0 {
		return c.HTTPBase, c.RPC
	}
	return c.endpoints[i].httpBase, c.endpoints[i].rpc
}
func (c *Client) doJSON(ctx context.Context, target string, body []byte) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.Token)
	return c.HTTP.Do(req)
}
func decodeHTTPStatus(rsp *http.Response) *HTTPStatusError {
	defer rsp.Body.Close()
	var body struct {
		Code  string `json:"code"`
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	_ = json.NewDecoder(io.LimitReader(rsp.Body, 4096)).Decode(&body)
	if body.Code == "" {
		body.Code = body.Error.Code
	}
	return &HTTPStatusError{Status: rsp.StatusCode, Code: body.Code}
}
