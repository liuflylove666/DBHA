package discovery

import (
	"fmt"
	"time"

	"dbm-services/common/dbha-v2/pkg/proto"
)

func (e Envelope) ToProto() *proto.DiscoveryReport {
	return &proto.DiscoveryReport{SchemaVersion: uint32(e.SchemaVersion), AgentId: e.AgentID, SessionId: e.SessionID, SessionEpoch: e.SessionEpoch, InstanceId: e.InstanceID, Stream: e.Stream, Sequence: e.Sequence, SampledAt: e.SampledAt.UTC().Format(time.RFC3339Nano), CollectionDurationMs: e.CollectionDurationMS, Payload: e.Payload}
}

func EnvelopeFromProto(p *proto.DiscoveryReport) (Envelope, error) {
	if p == nil {
		return Envelope{}, fmt.Errorf("missing discovery report")
	}
	t, err := time.Parse(time.RFC3339Nano, p.SampledAt)
	if err != nil {
		return Envelope{}, fmt.Errorf("invalid sampled_at: %w", err)
	}
	return Envelope{SchemaVersion: int(p.SchemaVersion), AgentID: p.AgentId, SessionID: p.SessionId, SessionEpoch: p.SessionEpoch, InstanceID: p.InstanceId, Stream: p.Stream, Sequence: p.Sequence, SampledAt: t, CollectionDurationMS: p.CollectionDurationMs, Payload: p.Payload}, nil
}

func (r ReportResponse) ToProto() *proto.DiscoveryAck {
	return &proto.DiscoveryAck{InstanceId: r.InstanceID, AcceptedSequence: r.AcceptedSequence, StoredRevision: r.StoredRevision, Duplicate: r.Duplicate, TopologyState: r.TopologyState, ReasonCodes: r.ReasonCodes, RouteReconcileRequired: r.RouteReconcileRequired, AuthoritativeTopologyEpoch: r.AuthoritativeTopologyEpoch}
}

func ReportResponseFromProto(p *proto.DiscoveryAck) ReportResponse {
	if p == nil {
		return ReportResponse{}
	}
	return ReportResponse{InstanceID: p.InstanceId, AcceptedSequence: p.AcceptedSequence, StoredRevision: p.StoredRevision, Duplicate: p.Duplicate, TopologyState: p.TopologyState, ReasonCodes: p.ReasonCodes, RouteReconcileRequired: p.RouteReconcileRequired, AuthoritativeTopologyEpoch: p.AuthoritativeTopologyEpoch}
}
