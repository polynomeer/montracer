package ingest

import (
	"bytes"
	"context"
	"net/http"
	"strings"
	"time"

	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/keepalive"
	"google.golang.org/grpc/mem"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/durationpb"

	"github.com/polynomeer/montracer/internal/authz"
	"github.com/polynomeer/montracer/internal/telemetry/otlp"
)

// OTLP/gRPC 수신 (D02 §04 "OTLP/gRPC 및 OTLP/HTTP", ADR 0036).
//
// gRPC가 protobuf를 struct로 풀기 전에 원본 byte를 받아(RawCodec), HTTP와 같은 otlp.Decode로 푼다.
// 그래서 압축 해제 후 크기 한도·decode 전 복잡도 pre-scan·malformed 판정이 두 transport에서 같다.
// 처리(인증부터 Kafka ACK까지)는 HTTP와 같은 process다.
//
// 상태 코드는 OTLP 규격을 따른다: 재시도 가능한 것은 UNAVAILABLE과, RetryInfo가 붙은 RESOURCE_EXHAUSTED(tenant 한도)뿐이다.
// 크기 초과·burst 초과는 RetryInfo 없는 RESOURCE_EXHAUSTED(같은 크기로 재시도 금지)다.

// RawCodec은 gRPC 메시지를 byte 그대로 주고받는 codec이다. ingress gRPC server 전용이다(grpc.ForceServerCodecV2).
// 이름이 "proto"라 OTLP client의 기본 content-subtype과 맞는다.
type RawCodec struct{}

// Marshal은 *[]byte를 그대로 보낸다.
func (RawCodec) Marshal(v any) (mem.BufferSlice, error) {
	b, ok := v.(*[]byte)
	if !ok {
		return nil, status.Error(codes.Internal, "ingest: raw codec expects *[]byte")
	}
	return mem.BufferSlice{mem.SliceBuffer(*b)}, nil
}

// Unmarshal은 받은 byte를 *[]byte에 복사한다.
func (RawCodec) Unmarshal(data mem.BufferSlice, v any) error {
	b, ok := v.(*[]byte)
	if !ok {
		return status.Error(codes.Internal, "ingest: raw codec expects *[]byte")
	}
	*b = data.Materialize()
	return nil
}

// Name은 codec 이름이다.
func (RawCodec) Name() string { return "proto" }

var grpcServices = []struct {
	name   string
	sig    otlp.Signal
	action authz.Action
}{
	{"opentelemetry.proto.collector.trace.v1.TraceService", otlp.SignalTraces, authz.IngestTraces},
	{"opentelemetry.proto.collector.metrics.v1.MetricsService", otlp.SignalMetrics, authz.IngestMetrics},
	{"opentelemetry.proto.collector.logs.v1.LogsService", otlp.SignalLogs, authz.IngestLogs},
}

// RegisterGRPC는 OTLP Trace·Metrics·Logs Export 서비스를 등록한다. server는 grpc.ForceServerCodecV2(RawCodec{})로 만든다.
func (h *Handler) RegisterGRPC(s *grpc.Server) {
	for _, svc := range grpcServices {
		s.RegisterService(&grpc.ServiceDesc{
			ServiceName: svc.name,
			HandlerType: (*any)(nil),
			Methods:     []grpc.MethodDesc{{MethodName: "Export", Handler: h.grpcExport(svc.name, svc.sig, svc.action)}},
			Metadata:    "opentelemetry/proto/collector",
		}, h)
	}
}

func (h *Handler) grpcExport(service string, sig otlp.Signal, action authz.Action) func(any, context.Context, func(any) error, grpc.UnaryServerInterceptor) (any, error) {
	call := func(ctx context.Context, req any) (any, error) {
		raw, _ := req.(*[]byte)
		start := h.cfg.Now()
		o := outcome{transport: "grpc"}
		defer func() { h.log(ctx, sig, &o, start) }()
		v := h.process(ctx, sig, action, grpcBearer(ctx), start.UTC(), &o, func() (otlp.Payload, error) {
			if raw == nil {
				return otlp.Payload{}, otlp.ErrMalformed
			}
			// gRPC가 압축을 이미 풀었다(크기 상한은 server MaxRecvMsgSize). 같은 decode로 한도·pre-scan을 적용한다.
			return otlp.Decode(bytes.NewReader(*raw), "application/x-protobuf", "", sig, h.cfg.Limits)
		})
		o.status = v.status
		if v.status != http.StatusOK {
			return nil, h.grpcStatus(v)
		}
		body, err := encodeResponse(sig, otlp.EncodingProtobuf, int64(o.rejected), rejectMessage(o.reasons))
		if err != nil {
			o.status = http.StatusInternalServerError
			return nil, status.Error(codes.Internal, "internal error")
		}
		return &body, nil
	}
	method := "/" + service + "/Export"
	return func(_ any, ctx context.Context, dec func(any) error, interceptor grpc.UnaryServerInterceptor) (any, error) {
		var raw []byte
		if err := dec(&raw); err != nil {
			return nil, err
		}
		if interceptor == nil {
			return call(ctx, &raw)
		}
		return interceptor(ctx, &raw, &grpc.UnaryServerInfo{Server: h, FullMethod: method}, call)
	}
}

// grpcStatus는 verdict를 OTLP 규격의 gRPC status로 바꾼다.
func (h *Handler) grpcStatus(v verdict) error {
	st := status.New(codes.Code(grpcCode(v.status)), v.message) //nolint:gosec // grpcCode는 0~16
	var delay time.Duration
	switch {
	case v.retryAfter > 0:
		delay = time.Duration(v.retryAfter) * time.Second
	case v.retry:
		delay = time.Duration(h.cfg.RetryAfter) * time.Second
	}
	if delay > 0 {
		// RESOURCE_EXHAUSTED는 RetryInfo가 있어야 재시도한다(tenant 한도). UNAVAILABLE에는 backoff 힌트다.
		if withInfo, err := st.WithDetails(&errdetails.RetryInfo{RetryDelay: durationpb.New(delay)}); err == nil {
			st = withInfo
		}
	}
	return st.Err()
}

// grpcBearer는 metadata의 authorization: Bearer <token>을 읽는다.
func grpcBearer(ctx context.Context) string {
	md, ok := metadata.FromIncomingContext(ctx)
	if !ok {
		return ""
	}
	for _, v := range md.Get("authorization") {
		const prefix = "bearer "
		if len(v) > len(prefix) && strings.EqualFold(v[:len(prefix)], prefix) {
			return strings.TrimSpace(v[len(prefix):])
		}
	}
	return ""
}

// GRPCServerOptions는 ingress gRPC server 옵션이다. 압축 해제 후 메시지 한도는 HTTP의 MaxDecodedBytes(D02 §04 8MiB)와 같다.
func (h *Handler) GRPCServerOptions() []grpc.ServerOption {
	maxMsg := h.cfg.Limits.MaxDecodedBytes
	if maxMsg <= 0 {
		maxMsg = otlp.DefaultLimits.MaxDecodedBytes
	}
	return []grpc.ServerOption{
		grpc.ForceServerCodecV2(RawCodec{}),
		grpc.MaxRecvMsgSize(int(maxMsg)),
		grpc.MaxSendMsgSize(1 << 20),
		// 너무 잦은 keepalive ping은 끊는다(자원 남용 방지). 일반 OTLP exporter 설정(수십 초)은 허용한다.
		grpc.KeepaliveEnforcementPolicy(keepalive.EnforcementPolicy{MinTime: 10 * time.Second, PermitWithoutStream: true}),
		grpc.KeepaliveParams(keepalive.ServerParameters{MaxConnectionIdle: 5 * time.Minute}),
	}
}
