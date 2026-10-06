package ingest

import (
	"context"
	"errors"
	"net"
	"strings"
	"testing"
	"time"

	"go.opentelemetry.io/collector/pdata/plog/plogotlp"
	"go.opentelemetry.io/collector/pdata/ptrace"
	"go.opentelemetry.io/collector/pdata/ptrace/ptraceotlp"
	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	_ "google.golang.org/grpc/encoding/gzip" // client gzip
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"

	"github.com/polynomeer/montracer/internal/authz"
	"github.com/polynomeer/montracer/internal/quota"
	"github.com/polynomeer/montracer/internal/telemetry/envelope"
	"github.com/polynomeer/montracer/internal/telemetry/otlp"
)

// grpcConn은 같은 Handler를 gRPC server로 띄우고(cmd/ingress와 같은 옵션) client 연결을 돌려준다.
func grpcConn(t *testing.T, s setup) *grpc.ClientConn {
	t.Helper()
	lis := bufconn.Listen(1 << 20)
	srv := grpc.NewServer(s.h.GRPCServerOptions()...)
	s.h.RegisterGRPC(srv)
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.Stop)
	conn, err := grpc.NewClient("passthrough:///bufnet",
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) { return lis.DialContext(ctx) }),
		grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return conn
}

func withToken(token string) context.Context {
	return metadata.AppendToOutgoingContext(context.Background(), "authorization", "Bearer "+token)
}

func tracesRequest(t *testing.T) ptraceotlp.ExportRequest {
	t.Helper()
	td, err := (&ptrace.JSONUnmarshaler{}).UnmarshalTraces(fixture(t, "otlp", "traces_checkout.json"))
	if err != nil {
		t.Fatal(err)
	}
	return ptraceotlp.NewExportRequestFromTraces(td)
}

func retryDelay(err error) (time.Duration, bool) {
	for _, d := range status.Convert(err).Details() {
		if ri, ok := d.(*errdetails.RetryInfo); ok {
			return ri.GetRetryDelay().AsDuration(), true
		}
	}
	return 0, false
}

// HTTP와 같은 처리: 인증·tenant 주입·envelope·Kafka append 뒤 OK, 빈 partial_success.
func TestGRPCTracesAccepted(t *testing.T) {
	s := newSetup(t)
	tok := s.keys.issue(t, allSignals, []string{"production"})
	client := ptraceotlp.NewGRPCClient(grpcConn(t, s))
	resp, err := client.Export(withToken(tok), tracesRequest(t))
	if err != nil {
		t.Fatal(err)
	}
	if resp.PartialSuccess().RejectedSpans() != 0 || len(s.prod.records) != 4 {
		t.Fatalf("rejected=%d produced=%d", resp.PartialSuccess().RejectedSpans(), len(s.prod.records))
	}
	if got := string(headerVal(s.prod.records[0], envelope.HeaderTenant)); got != tenantA.String() {
		t.Errorf("tenant header = %q", got)
	}
	// gzip 압축 요청도 같다
	if _, err := client.Export(withToken(tok), tracesRequest(t), grpc.UseCompressor("gzip")); err != nil {
		t.Errorf("gzip: %v", err)
	}
}

// record 일부 거절은 OK + partial_success (D02 §04 표).
func TestGRPCPartialSuccess(t *testing.T) {
	s := newSetup(t)
	tok := s.keys.issue(t, allSignals, []string{"production"}) // fixture는 production 1개 resource
	req := tracesRequest(t)
	// environment가 없는 resource의 span → 거절(나머지 4개는 저장)
	orig := req.Traces().ResourceSpans().At(0)
	extra := req.Traces().ResourceSpans().AppendEmpty()
	orig.ScopeSpans().At(0).Spans().At(0).CopyTo(extra.ScopeSpans().AppendEmpty().Spans().AppendEmpty())
	resp, err := ptraceotlp.NewGRPCClient(grpcConn(t, s)).Export(withToken(tok), req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.PartialSuccess().RejectedSpans() != 1 || !strings.Contains(resp.PartialSuccess().ErrorMessage(), "environment_not_allowed") || len(s.prod.records) != 4 {
		t.Errorf("partial = %d %q", resp.PartialSuccess().RejectedSpans(), resp.PartialSuccess().ErrorMessage())
	}
}

func TestGRPCAuthAndScope(t *testing.T) {
	s := newSetup(t)
	conn := grpcConn(t, s)
	traces := ptraceotlp.NewGRPCClient(conn)
	if _, err := traces.Export(context.Background(), tracesRequest(t)); status.Code(err) != codes.Unauthenticated {
		t.Errorf("no token: %v", err)
	}
	if _, err := traces.Export(withToken("mti_0000000000000000_"+strings.Repeat("A", 43)), tracesRequest(t)); status.Code(err) != codes.Unauthenticated {
		t.Errorf("unknown key: %v", err)
	}
	logsOnly := s.keys.issue(t, []authz.Action{authz.IngestLogs}, []string{"production"})
	if _, err := traces.Export(withToken(logsOnly), tracesRequest(t)); status.Code(err) != codes.PermissionDenied {
		t.Errorf("logs-only key on traces: %v", err)
	}
	// 같은 key로 logs 서비스는 된다(서비스별 scope)
	if _, err := plogotlp.NewGRPCClient(conn).Export(withToken(logsOnly), plogotlp.NewExportRequest()); err != nil {
		t.Errorf("logs export: %v", err)
	}
	if len(s.prod.records) != 0 {
		t.Errorf("produced %d on auth failures", len(s.prod.records))
	}
}

// 재시도 가능성은 OTLP 규격대로: tenant 한도는 RESOURCE_EXHAUSTED + RetryInfo, append 실패는 UNAVAILABLE,
// burst·크기 초과는 RetryInfo 없는 RESOURCE_EXHAUSTED(같은 크기로 재시도 금지).
func TestGRPCRetryability(t *testing.T) {
	s := newSetup(t)
	s.h.cfg.Quota = quota.New(quota.Config{Default: quota.Limits{RecordsPerSecond: 2, RecordsBurst: 6, BytesPerSecond: 1e9, BytesBurst: 1e9}})
	tok := s.keys.issue(t, allSignals, []string{"production"})
	client := ptraceotlp.NewGRPCClient(grpcConn(t, s))
	if _, err := client.Export(withToken(tok), tracesRequest(t)); err != nil {
		t.Fatal(err)
	}
	_, err := client.Export(withToken(tok), tracesRequest(t))
	if d, ok := retryDelay(err); status.Code(err) != codes.ResourceExhausted || !ok || d != time.Second {
		t.Errorf("rate limited: %v (retry %v %v)", err, d, ok)
	}

	big := tracesRequest(t)
	more := tracesRequest(t)
	more.Traces().ResourceSpans().MoveAndAppendTo(big.Traces().ResourceSpans()) // span 8 > burst 6
	_, err = client.Export(withToken(tok), big)
	if _, ok := retryDelay(err); status.Code(err) != codes.ResourceExhausted || ok {
		t.Errorf("over burst: %v (must not carry RetryInfo)", err)
	}

	s2 := newSetup(t)
	s2.prod.err = errors.New("kafka down")
	tok2 := s2.keys.issue(t, allSignals, []string{"production"})
	_, err = ptraceotlp.NewGRPCClient(grpcConn(t, s2)).Export(withToken(tok2), tracesRequest(t))
	if status.Code(err) != codes.Unavailable {
		t.Errorf("produce failure: %v", err)
	}
}

// 압축 해제 후 크기 한도는 gRPC server가 decode 전에 막는다(RESOURCE_EXHAUSTED, RetryInfo 없음).
func TestGRPCMessageSizeLimit(t *testing.T) {
	s := newSetup(t)
	s.h.cfg.Limits = otlp.Limits{MaxWireBytes: 512, MaxDecodedBytes: 512}
	tok := s.keys.issue(t, allSignals, []string{"production"})
	_, err := ptraceotlp.NewGRPCClient(grpcConn(t, s)).Export(withToken(tok), tracesRequest(t))
	if _, ok := retryDelay(err); status.Code(err) != codes.ResourceExhausted || ok {
		t.Errorf("oversized: %v", err)
	}
	// gzip으로 전송 크기는 작아도 압축 해제 후 한도를 넘으면 같은 거절(압축 폭탄 방어)
	_, err = ptraceotlp.NewGRPCClient(grpcConn(t, s)).Export(withToken(tok), tracesRequest(t), grpc.UseCompressor("gzip"))
	if status.Code(err) != codes.ResourceExhausted {
		t.Errorf("oversized after decompression: %v", err)
	}
	if len(s.prod.records) != 0 {
		t.Error("oversized request produced records")
	}
}

// 로그에는 payload·key가 없다(HTTP와 같은 log 경로).
func TestGRPCLogsCarryNoPayloadOrKey(t *testing.T) {
	s := newSetup(t)
	tok := s.keys.issue(t, allSignals, []string{"production"})
	if _, err := ptraceotlp.NewGRPCClient(grpcConn(t, s)).Export(withToken(tok), tracesRequest(t)); err != nil {
		t.Fatal(err)
	}
	logs := s.logBuf.String()
	if !strings.Contains(logs, `"otlp request"`) || strings.Contains(logs, tok) || strings.Contains(logs, "POST /checkout") {
		t.Errorf("log = %s", logs)
	}
}
