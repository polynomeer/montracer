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
// Export는 unary RPC지만 server에는 stream method로 등록한다(wire 형식은 같다). grpc-go는 unary method의 메시지를
// handler 호출 **전에** 끝까지 받아 압축을 푼다 — 인증 없이 8MiB 수신·압축 해제를 강제할 수 있다(리뷰에서 발견).
// stream handler는 메시지를 직접 RecvMsg하므로 HTTP와 같이 인증 → in-flight slot → 본문 수신 순서를 지킨다.
//
// 받은 byte(RawCodec)를 HTTP와 같은 otlp.Decode로 푼다. 그래서 압축 해제 후 크기 한도·decode 전 복잡도 pre-scan·
// malformed 판정이 두 transport에서 같다. 처리(인증부터 Kafka ACK까지)는 HTTP와 같은 process다.
//
// 상태 코드는 OTLP 규격을 따른다: 재시도 가능한 것은 UNAVAILABLE과, RetryInfo가 붙은 RESOURCE_EXHAUSTED(tenant 한도)뿐이다.
// 크기 초과·burst 초과는 RetryInfo 없는 RESOURCE_EXHAUSTED(같은 크기로 재시도 금지)다.

// grpcRecvTimeout은 메시지 하나를 다 받는 시간 상한이다(HTTP ReadTimeout 20초와 같다). 느린 송신이 slot을 붙잡지 않게 한다.
const grpcRecvTimeout = 20 * time.Second

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

// RegisterGRPC는 OTLP Trace·Metrics·Logs Export 서비스를 등록한다. server는 GRPCServerOptions로 만든다.
// 이 server에는 OTLP 서비스만 등록한다: RawCodec이 server 전체에 적용되므로 다른 서비스(health·reflection)는
// *[]byte가 아닌 메시지를 받아 실패한다. health는 HTTP /healthz·/readyz를 쓴다(시험으로 고정).
func (h *Handler) RegisterGRPC(s *grpc.Server) {
	for _, svc := range grpcServices {
		s.RegisterService(&grpc.ServiceDesc{
			ServiceName: svc.name,
			HandlerType: (*any)(nil),
			Streams:     []grpc.StreamDesc{{StreamName: "Export", Handler: h.grpcExport(svc.sig, svc.action)}},
			Metadata:    "opentelemetry/proto/collector",
		}, h)
	}
}

func (h *Handler) grpcExport(sig otlp.Signal, action authz.Action) grpc.StreamHandler {
	return func(_ any, stream grpc.ServerStream) error {
		ctx := stream.Context()
		start := h.cfg.Now()
		o := outcome{transport: "grpc"}
		defer h.log(ctx, sig, &o, start)
		token := grpcBearer(ctx)
		auth := func(ctx context.Context) (authz.Principal, error) { return h.cfg.Authenticate(ctx, token) }
		// 본문은 process가 인증·in-flight slot 뒤에 이 함수를 부를 때 처음 읽는다.
		v := h.process(ctx, sig, action, auth, start.UTC(), &o, func(ctx context.Context) (otlp.Payload, error) {
			raw, err := recvWithin(ctx, stream, grpcRecvTimeout)
			if err != nil {
				return otlp.Payload{}, err
			}
			// gRPC가 압축을 풀었다(상한은 server MaxRecvMsgSize). 같은 decode로 한도·pre-scan을 적용한다.
			return otlp.Decode(bytes.NewReader(raw), "application/x-protobuf", "", sig, h.cfg.Limits)
		})
		o.status = v.status
		if v.status != http.StatusOK {
			return h.grpcStatus(v)
		}
		body, err := encodeResponse(sig, otlp.EncodingProtobuf, int64(o.rejected), rejectMessage(o.reasons))
		if err != nil {
			o.status = http.StatusInternalServerError
			return status.Error(codes.Internal, "internal error")
		}
		return stream.SendMsg(&body)
	}
}

// recvWithin은 unary 메시지 하나를 timeout 안에 받는다. grpc 오류를 process가 아는 decode 오류로 바꾼다.
func recvWithin(ctx context.Context, stream grpc.ServerStream, timeout time.Duration) ([]byte, error) {
	type result struct {
		raw []byte
		err error
	}
	ch := make(chan result, 1)
	go func() {
		var raw []byte
		err := stream.RecvMsg(&raw)
		ch <- result{raw, err}
	}()
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case r := <-ch:
		switch status.Code(r.err) {
		case codes.OK:
			return r.raw, nil
		case codes.ResourceExhausted:
			return nil, otlp.ErrBodyTooLarge // 압축 해제 후 MaxRecvMsgSize 초과
		case codes.Unimplemented, codes.Internal:
			// 지원하지 않는 grpc-encoding(압축 방식) 등 — 데이터를 읽을 수 없다
			if strings.Contains(r.err.Error(), "compress") || strings.Contains(r.err.Error(), "decompress") {
				return nil, otlp.ErrUnsupportedMediaType
			}
			return nil, otlp.ErrMalformed
		default:
			return nil, otlp.ErrBodyRead // 연결 끊김·취소: 재시도 가능
		}
	case <-timer.C:
		// handler가 끝나면 grpc가 stream을 취소해 RecvMsg goroutine도 끝난다
		return nil, otlp.ErrBodyRead
	case <-ctx.Done():
		return nil, otlp.ErrBodyRead
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

// GRPCServerOptions는 ingress gRPC server 옵션이다 (ADR 0036 §4).
//   - 압축 해제 후 메시지 한도는 HTTP의 MaxDecodedBytes(D02 §04 8MiB)와 같다.
//   - 연결당 동시 stream 32개. in-flight 상한(process)과 별개로 한 연결이 자원을 독점하지 못하게 한다.
//   - 연결 수명 2분(+정리 30초): replica 사이 부하가 고르게 다시 나뉜다(ADR 0024 §3 quota 분배 전제).
//     L4 LB 뒤에서 장기 HTTP/2 연결이 한 replica에 고정되면 tenant가 한도의 1/N만 쓰게 된다.
//   - 너무 잦은 keepalive ping은 끊고, 5분 idle 연결은 닫는다.
func (h *Handler) GRPCServerOptions() []grpc.ServerOption {
	maxMsg := h.cfg.Limits.MaxDecodedBytes
	if maxMsg <= 0 {
		maxMsg = otlp.DefaultLimits.MaxDecodedBytes
	}
	return []grpc.ServerOption{
		grpc.ForceServerCodecV2(RawCodec{}),
		grpc.MaxRecvMsgSize(int(maxMsg)),
		grpc.MaxSendMsgSize(1 << 20),
		grpc.MaxConcurrentStreams(32),
		grpc.KeepaliveEnforcementPolicy(keepalive.EnforcementPolicy{MinTime: 10 * time.Second, PermitWithoutStream: true}),
		grpc.KeepaliveParams(keepalive.ServerParameters{MaxConnectionIdle: 5 * time.Minute,
			MaxConnectionAge: 2 * time.Minute, MaxConnectionAgeGrace: 30 * time.Second}),
	}
}
