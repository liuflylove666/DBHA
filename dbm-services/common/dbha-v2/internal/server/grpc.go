package server

import (
	"context"
	"crypto/tls"
	"errors"
	"strings"
	"time"

	"dbm-services/common/dbha-v2/pkg/discovery"
	"dbm-services/common/dbha-v2/pkg/proto"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

type discoveryRPC struct {
	proto.UnimplementedDiscoveryServiceServer
	server *Server
}

type haDiscoveryRPC struct {
	proto.UnimplementedDiscoveryServiceServer
	runtime *haRuntime
}

func (s *Server) grpcServer(c *tls.Config) *grpc.Server {
	options := []grpc.ServerOption{grpc.MaxRecvMsgSize(128 << 10), grpc.MaxSendMsgSize(64 << 10), grpc.MaxConcurrentStreams(128)}
	if c != nil {
		options = append(options, grpc.Creds(credentials.NewTLS(c)))
	}
	g := grpc.NewServer(options...)
	proto.RegisterDiscoveryServiceServer(g, &discoveryRPC{server: s})
	// The old unauthenticated receiver is intentionally not registered.
	return g
}

func (h *haRuntime) grpcServer(c *tls.Config) *grpc.Server {
	options := []grpc.ServerOption{grpc.MaxRecvMsgSize(128 << 10), grpc.MaxSendMsgSize(64 << 10), grpc.MaxConcurrentStreams(128)}
	if c != nil {
		options = append(options, grpc.Creds(credentials.NewTLS(c)))
	}
	g := grpc.NewServer(options...)
	proto.RegisterDiscoveryServiceServer(g, &haDiscoveryRPC{runtime: h})
	return g
}

func (r *haDiscoveryRPC) active(ctx context.Context) (*discoveryRPC, error) {
	if active := r.runtime.active.Load(); active != nil {
		return &discoveryRPC{server: active}, nil
	}
	if owner, err := r.runtime.owner(ctx); err == nil {
		_ = grpc.SetTrailer(ctx, metadata.Pairs("dbha-leader-grpc", owner.AdvertiseGRPC))
	}
	return nil, status.Error(codes.Unavailable, "NOT_LEADER")
}

func (r *haDiscoveryRPC) PushObservation(ctx context.Context, request *proto.DiscoveryReport) (*proto.DiscoveryAck, error) {
	active, err := r.active(ctx)
	if err != nil {
		return nil, err
	}
	return active.PushObservation(ctx, request)
}

func (r *haDiscoveryRPC) PushSample(ctx context.Context, request *proto.DiscoveryReport) (*proto.DiscoveryAck, error) {
	active, err := r.active(ctx)
	if err != nil {
		return nil, err
	}
	return active.PushSample(ctx, request)
}

func grpcError(err error) error {
	var e *Error
	if !errors.As(err, &e) {
		return status.Error(codes.Unavailable, "STORAGE_UNAVAILABLE")
	}
	code := codes.FailedPrecondition
	switch e.Status {
	case 400:
		code = codes.InvalidArgument
	case 401:
		code = codes.Unauthenticated
	case 403:
		code = codes.PermissionDenied
	case 404:
		code = codes.NotFound
	case 409:
		code = codes.Aborted
	case 413, 429:
		code = codes.ResourceExhausted
	case 503:
		code = codes.Unavailable
	}
	return status.Error(code, e.Code)
}

func (r *discoveryRPC) push(ctx context.Context, request *proto.DiscoveryReport, topology bool) (*proto.DiscoveryAck, error) {
	started := time.Now()
	result := "rejected"
	rejectionCode := "UNAUTHORIZED"
	stream := "sample"
	if topology {
		stream = "topology"
	}
	defer func() {
		m := r.server.ensureMetrics()
		m.ack.WithLabelValues(stream, result).Observe(time.Since(started).Seconds())
		if result == "rejected" {
			m.rejections.WithLabelValues(stream, rejectionCode).Inc()
		}
	}()
	md, ok := metadata.FromIncomingContext(ctx)
	if !ok {
		return nil, status.Error(codes.Unauthenticated, "UNAUTHORIZED")
	}
	values := md.Get("authorization")
	if len(values) != 1 || !strings.HasPrefix(values[0], "Bearer ") {
		return nil, status.Error(codes.Unauthenticated, "UNAUTHORIZED")
	}
	envelope, err := discovery.EnvelopeFromProto(request)
	if err != nil {
		rejectionCode = "INVALID_ENVELOPE"
		return nil, status.Error(codes.InvalidArgument, "INVALID_ENVELOPE")
	}
	if (envelope.Stream == "topology") != topology {
		rejectionCode = "INVALID_STREAM"
		return nil, status.Error(codes.InvalidArgument, "INVALID_STREAM")
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	response, err := r.server.Store.Ingest(ctx, strings.TrimPrefix(values[0], "Bearer "), envelope)
	if err != nil {
		rejectionCode = metricCode(err)
		_ = grpc.SetTrailer(ctx, metadata.Pairs("dbha-error-code", rejectionCode))
		return nil, grpcError(err)
	}
	result = "accepted"
	if response.Duplicate {
		r.server.ensureMetrics().duplicates.Inc()
	}
	return response.ToProto(), nil
}

func (r *discoveryRPC) PushObservation(ctx context.Context, request *proto.DiscoveryReport) (*proto.DiscoveryAck, error) {
	return r.push(ctx, request, true)
}
func (r *discoveryRPC) PushSample(ctx context.Context, request *proto.DiscoveryReport) (*proto.DiscoveryAck, error) {
	return r.push(ctx, request, false)
}
