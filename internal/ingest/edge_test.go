package ingest

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"go.opentelemetry.io/collector/pdata/pmetric/pmetricotlp"
	"go.opentelemetry.io/collector/pdata/ptrace"

	"github.com/polynomeer/montracer/internal/telemetry/envelope"
)

// 8MiB를 넘는 일반(identity) 본문은 400이 아니라 413이어야 client가 배치를 나눈다 (D02 §04).
func TestOversizedIdentityBodyIs413(t *testing.T) {
	s := newSetup(t)
	tok := s.keys.issue(t, allSignals, []string{"production"})
	big := bytes.Repeat([]byte{0x0a, 0x00}, (8<<20)/2+16) // 8MiB+32B
	// Content-Length 사전 검사 경로
	if rec := post(s, "/v1/traces", tok, "application/x-protobuf", "", big); rec.Code != 413 {
		t.Fatalf("with content-length: %d", rec.Code)
	}
	// chunked(길이 미상) 경로: MaxBytesReader가 한도를 넘김 → 413
	req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/v1/traces", struct{ *bytes.Reader }{bytes.NewReader(big)})
	req.ContentLength = -1
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("Content-Type", "application/x-protobuf")
	rec := httptest.NewRecorder()
	s.h.ServeHTTP(rec, req)
	if rec.Code != 413 {
		t.Fatalf("chunked: %d", rec.Code)
	}
}

type brokenBody struct{}

func (brokenBody) Read([]byte) (int, error) { return 0, errors.New("connection reset") }

// 본문 읽기 실패는 데이터 오류가 아니므로 재시도 가능(503).
func TestBodyReadFailureIsRetryable(t *testing.T) {
	s := newSetup(t)
	tok := s.keys.issue(t, allSignals, []string{"production"})
	req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/v1/traces", brokenBody{})
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("Content-Type", "application/x-protobuf")
	rec := httptest.NewRecorder()
	s.h.ServeHTTP(rec, req)
	if rec.Code != 503 || rec.Header().Get("Retry-After") == "" {
		t.Fatalf("status = %d", rec.Code)
	}
}

// 일부 record만 append된 뒤 실패해도 ACK(200)하지 않는다 (ADR 0002).
type partialProducer struct{ written []envelope.Record }

func (p *partialProducer) ProduceSync(_ context.Context, rs []envelope.Record) error {
	p.written = append(p.written, rs[:len(rs)/2]...)
	return errors.New("broker went away after partial append")
}

type blockingProducer struct{}

func (blockingProducer) ProduceSync(ctx context.Context, _ []envelope.Record) error {
	<-ctx.Done()
	return ctx.Err()
}

func TestNoAckOnPartialAppendOrTimeout(t *testing.T) {
	for name, p := range map[string]Producer{"partial append": &partialProducer{}, "produce timeout": blockingProducer{}} {
		t.Run(name, func(t *testing.T) {
			s := newSetup(t)
			s.h.cfg.Producer = p
			s.h.cfg.ProduceTimeout = 50 * time.Millisecond
			tok := s.keys.issue(t, allSignals, []string{"production"})
			rec := post(s, "/v1/traces", tok, "application/x-protobuf", "", mustProto(t))
			if rec.Code != 503 || rec.Header().Get("Retry-After") == "" {
				t.Fatalf("status = %d, want 503", rec.Code)
			}
		})
	}
}

// 오류 본문은 요청과 같은 encoding의 google.rpc.Status다 (OTLP/HTTP).
func TestErrorBodyEncodingFollowsRequest(t *testing.T) {
	s := newSetup(t)
	rec := post(s, "/v1/traces", "", "application/json", "", []byte(`{}`))
	if rec.Code != 401 || rec.Header().Get("Content-Type") != "application/json" {
		t.Fatalf("status=%d ct=%q", rec.Code, rec.Header().Get("Content-Type"))
	}
	var st struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &st); err != nil || st.Code != 16 {
		t.Fatalf("body = %s", rec.Body.String())
	}
}

func TestMetricsPath(t *testing.T) {
	s := newSetup(t)
	tok := s.keys.issue(t, allSignals, []string{"production"})
	rec := post(s, "/v1/metrics", tok, "application/json", "", fixture(t, "otlp", "metrics_checkout.json"))
	if rec.Code != 200 {
		t.Fatalf("status = %d", rec.Code)
	}
	resp := pmetricotlp.NewExportResponse()
	if err := resp.UnmarshalJSON(rec.Body.Bytes()); err != nil || resp.PartialSuccess().RejectedDataPoints() != 0 {
		t.Fatalf("response = %s", rec.Body.String())
	}
	if len(s.prod.records) != 3 {
		t.Fatalf("produced = %d", len(s.prod.records))
	}
	for _, r := range s.prod.records {
		if r.Topic != envelope.TopicMetrics {
			t.Fatalf("topic = %s", r.Topic)
		}
	}
}

// 1MiB를 넘는 record는 partial success로 거절되고 produce되지 않는다.
func TestRecordTooLargeRejected(t *testing.T) {
	s := newSetup(t)
	tok := s.keys.issue(t, allSignals, []string{"production"})
	td, _ := (&ptrace.JSONUnmarshaler{}).UnmarshalTraces(fixture(t, "otlp", "traces_checkout.json"))
	// 속성 수·값 크기는 각각 한도 안이지만, resource+scope+span 합이 envelope 1MiB를 넘게 만든다.
	rs := td.ResourceSpans().At(0)
	for i := range 120 {
		rs.Resource().Attributes().PutStr(fmt.Sprintf("pad.r%03d", i), strings.Repeat("v", 4<<10))
		rs.ScopeSpans().At(0).Scope().Attributes().PutStr(fmt.Sprintf("pad.s%03d", i), strings.Repeat("v", 4<<10))
	}
	spans := rs.ScopeSpans().At(0).Spans()
	for j := 0; j < spans.Len(); j++ {
		for i := range 15 {
			spans.At(j).Attributes().PutStr(fmt.Sprintf("pad.p%02d", i), strings.Repeat("v", 4<<10))
		}
	}
	body, _ := (&ptrace.ProtoMarshaler{}).MarshalTraces(td)
	rec := post(s, "/v1/traces", tok, "application/x-protobuf", "", body)
	if rec.Code != 200 {
		t.Fatalf("status = %d", rec.Code)
	}
	resp := ptraceOTLPResponse(t, rec.Body.Bytes())
	if resp.rejected != 2 || !strings.Contains(resp.msg, "record_too_large=2") {
		t.Fatalf("partial = %+v", resp)
	}
	for _, r := range s.prod.records {
		if len(r.Value) > envelope.MaxMessageBytes {
			t.Fatal("oversized record produced")
		}
	}
}

// payload가 흉내 낸 tenant·플랫폼 예약 속성은 Kafka에 쓰기 전에 지운다.
func TestReservedAttributesStripped(t *testing.T) {
	s := newSetup(t)
	tok := s.keys.issue(t, allSignals, []string{"production"})
	td, _ := (&ptrace.JSONUnmarshaler{}).UnmarshalTraces(fixture(t, "otlp", "traces_checkout.json"))
	a := td.ResourceSpans().At(0).Resource().Attributes()
	a.PutStr("tenant_id", "22222222-2222-4222-8222-222222222222")
	a.PutStr("mt.tenant", "x")
	a.PutStr("Montracer.Routing", "x")
	body, _ := (&ptrace.ProtoMarshaler{}).MarshalTraces(td)
	if rec := post(s, "/v1/traces", tok, "application/x-protobuf", "", body); rec.Code != 200 {
		t.Fatalf("status = %d", rec.Code)
	}
	for _, r := range s.prod.records {
		if bytes.Contains(r.Value, []byte("22222222-2222")) || bytes.Contains(r.Value, []byte("mt.tenant")) || bytes.Contains(r.Value, []byte("Montracer.Routing")) {
			t.Fatal("reserved attribute reached Kafka")
		}
	}
}

type partial struct {
	rejected uint64
	msg      string
}

func ptraceOTLPResponse(t *testing.T, b []byte) partial {
	t.Helper()
	ps := field(t, b, 1)
	return partial{rejected: varintField(t, ps, 1), msg: string(field(t, ps, 2))}
}
