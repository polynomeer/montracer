package main

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"time"

	"go.opentelemetry.io/collector/pdata/pcommon"
	"go.opentelemetry.io/collector/pdata/plog"
	"go.opentelemetry.io/collector/pdata/pmetric"
	"go.opentelemetry.io/collector/pdata/ptrace"
	"go.opentelemetry.io/collector/pdata/ptrace/ptraceotlp"
	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	grpcstatus "google.golang.org/grpc/status"
)

// checkout 시나리오 (D06 §04 통합 시나리오 축소판):
//   - 요청 i는 anchor + 0.3i초에 시작한다(1,000개 = 5분). 서비스 체인은 <root> → payment → database (span 3개).
//     5분에 몰아 두는 이유: rollup은 처음 보는 tenant를 watermark − 10분부터 계산한다(과거 구간은 backfill job 몫, ADR 0026).
//     seed 데이터가 그 안에 들어와야 metric oracle을 확인할 수 있다.
//   - i%10==0이면 느린 요청(2.5초, 100개), 그중 i%50==0이면 오류(20개). 비샘플링 metric oracle: 요청 1,000·오류 20(2%).
//   - 요청마다 root 서비스 info log 1건(같은 trace), 오류면 payment error log 1건. log.record.uid는 결정적이다.
//   - metric http.server.requests(delta sum, 분 단위, status별).
const (
	requestEvery = 300 * time.Millisecond
	batchSize    = 100
	metricName   = "http.server.requests"
	statusAttr   = "http.response.status_code"
)

func slow(i int) bool   { return i%10 == 0 }
func failed(i int) bool { return i%50 == 0 }
func startOf(anchor time.Time, i int) time.Time {
	return anchor.Add(time.Duration(i) * requestEvery)
}

func traceIDOf(tenant string, anchor time.Time, i int) pcommon.TraceID {
	var t pcommon.TraceID
	copy(t[:], id(tenant, anchor, "trace", i, 16))
	return t
}

func spanIDOf(tenant string, anchor time.Time, name string, i int) pcommon.SpanID {
	var s pcommon.SpanID
	copy(s[:], id(tenant, anchor, "span:"+name, i, 8))
	return s
}

// TraceHex는 smoke가 다시 계산하는 알려진 trace ID다.
func traceHex(tenant string, anchor time.Time, i int) string {
	t := traceIDOf(tenant, anchor, i)
	return hex.EncodeToString(t[:])
}

func resource(m pcommon.Map, service string) {
	m.PutStr("service.name", service)
	m.PutStr("service.namespace", "shop")
	m.PutStr("deployment.environment.name", "prod")
}

func services(dt demoTenant) [3]string { return [3]string{dt.Service, "payment", "database"} }

// buildTraces는 [from, to) 요청의 span을 서비스별 resource로 묶는다.
func buildTraces(dt demoTenant, anchor time.Time, from, to int) ptrace.Traces {
	td := ptrace.NewTraces()
	svc := services(dt)
	var scopes [3]ptrace.SpanSlice
	for k, name := range svc {
		rs := td.ResourceSpans().AppendEmpty()
		resource(rs.Resource().Attributes(), name)
		ss := rs.ScopeSpans().AppendEmpty()
		ss.Scope().SetName("montracer.demo")
		scopes[k] = ss.Spans()
	}
	for i := from; i < to; i++ {
		start := startOf(anchor, i)
		dur := 120 * time.Millisecond
		if slow(i) {
			dur = 2500 * time.Millisecond
		}
		tid := traceIDOf(dt.ID, anchor, i)
		root, pay, db := spanIDOf(dt.ID, anchor, "root", i), spanIDOf(dt.ID, anchor, "payment", i), spanIDOf(dt.ID, anchor, "db", i)

		r := scopes[0].AppendEmpty()
		r.SetTraceID(tid)
		r.SetSpanID(root)
		r.SetName("POST /checkout")
		r.SetKind(ptrace.SpanKindServer)
		r.SetStartTimestamp(pcommon.NewTimestampFromTime(start))
		r.SetEndTimestamp(pcommon.NewTimestampFromTime(start.Add(dur)))
		r.Attributes().PutStr("http.route", "/checkout")
		r.Attributes().PutInt(statusAttr, status(i))

		p := scopes[1].AppendEmpty()
		p.SetTraceID(tid)
		p.SetSpanID(pay)
		p.SetParentSpanID(root)
		p.SetName("POST /charge")
		p.SetKind(ptrace.SpanKindServer)
		p.SetStartTimestamp(pcommon.NewTimestampFromTime(start.Add(5 * time.Millisecond)))
		p.SetEndTimestamp(pcommon.NewTimestampFromTime(start.Add(dur - 5*time.Millisecond)))

		d := scopes[2].AppendEmpty()
		d.SetTraceID(tid)
		d.SetSpanID(db)
		d.SetParentSpanID(pay)
		d.SetName("SELECT orders")
		d.SetKind(ptrace.SpanKindClient)
		d.SetStartTimestamp(pcommon.NewTimestampFromTime(start.Add(10 * time.Millisecond)))
		d.SetEndTimestamp(pcommon.NewTimestampFromTime(start.Add(dur - 10*time.Millisecond)))
		d.Attributes().PutStr("db.system.name", "postgresql")
		d.Attributes().PutStr("db.query.summary", "SELECT orders")

		if failed(i) {
			r.Status().SetCode(ptrace.StatusCodeError)
			r.Status().SetMessage("payment declined")
			p.Status().SetCode(ptrace.StatusCodeError)
			p.Status().SetMessage("card declined")
		}
	}
	return td
}

func status(i int) int64 {
	if failed(i) {
		return 500
	}
	return 200
}

func buildLogs(dt demoTenant, anchor time.Time, from, to int) plog.Logs {
	ld := plog.NewLogs()
	svc := services(dt)
	rootRL := ld.ResourceLogs().AppendEmpty()
	resource(rootRL.Resource().Attributes(), svc[0])
	rootLogs := rootRL.ScopeLogs().AppendEmpty().LogRecords()
	payRL := ld.ResourceLogs().AppendEmpty()
	resource(payRL.Resource().Attributes(), svc[1])
	payLogs := payRL.ScopeLogs().AppendEmpty().LogRecords()
	for i := from; i < to; i++ {
		start := startOf(anchor, i)
		lr := rootLogs.AppendEmpty()
		lr.SetTimestamp(pcommon.NewTimestampFromTime(start.Add(time.Millisecond)))
		lr.SetTraceID(traceIDOf(dt.ID, anchor, i))
		lr.SetSpanID(spanIDOf(dt.ID, anchor, "root", i))
		lr.SetSeverityNumber(plog.SeverityNumberInfo)
		lr.SetSeverityText("INFO")
		lr.Body().SetStr("order received")
		lr.Attributes().PutStr("log.record.uid", hex.EncodeToString(id(dt.ID, anchor, "log:root", i, 16)))
		if failed(i) {
			e := payLogs.AppendEmpty()
			e.SetTimestamp(pcommon.NewTimestampFromTime(start.Add(20 * time.Millisecond)))
			e.SetTraceID(traceIDOf(dt.ID, anchor, i))
			e.SetSpanID(spanIDOf(dt.ID, anchor, "payment", i))
			e.SetSeverityNumber(plog.SeverityNumberError)
			e.SetSeverityText("ERROR")
			e.Body().SetStr("charge failed: card declined")
			e.Attributes().PutStr("log.record.uid", hex.EncodeToString(id(dt.ID, anchor, "log:payment", i, 16)))
		}
	}
	return ld
}

// buildMetrics는 분 단위 delta sum을 status별로 만든다(비샘플링 SDK metric 역할, 계약 5).
func buildMetrics(dt demoTenant, anchor time.Time) pmetric.Metrics {
	type key struct {
		minute int
		status int64
	}
	counts := map[key]int64{}
	maxMinute := 0
	for i := 0; i < dt.Requests; i++ {
		m := int(startOf(anchor, i).Sub(anchor) / time.Minute)
		counts[key{m, status(i)}]++
		if m > maxMinute {
			maxMinute = m
		}
	}
	md := pmetric.NewMetrics()
	rm := md.ResourceMetrics().AppendEmpty()
	resource(rm.Resource().Attributes(), dt.Service)
	met := rm.ScopeMetrics().AppendEmpty().Metrics().AppendEmpty()
	met.SetName(metricName)
	met.SetUnit("{request}")
	sum := met.SetEmptySum()
	sum.SetIsMonotonic(true)
	sum.SetAggregationTemporality(pmetric.AggregationTemporalityDelta)
	for m := 0; m <= maxMinute; m++ {
		for _, st := range []int64{200, 500} {
			n, ok := counts[key{m, st}]
			if !ok {
				continue
			}
			dp := sum.DataPoints().AppendEmpty()
			ws := anchor.Add(time.Duration(m) * time.Minute)
			dp.SetStartTimestamp(pcommon.NewTimestampFromTime(ws))
			dp.SetTimestamp(pcommon.NewTimestampFromTime(ws.Add(time.Minute)))
			dp.SetIntValue(n)
			dp.Attributes().PutInt(statusAttr, st)
		}
	}
	return md
}

// post는 OTLP/HTTP JSON을 ingress로 보낸다. 429·503은 Retry-After만큼 기다려 다시 보낸다(최대 10회).
// 200이어도 partial success로 일부가 거절되면 오류다(seed가 조용히 덜 들어가지 않게).
func post(ctx context.Context, ingressURL, path, token string, body []byte) error {
	for attempt := 0; ; attempt++ {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, ingressURL+path, bytes.NewReader(body))
		if err != nil {
			return err
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+token)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return fmt.Errorf("ingress %s unreachable (make dev 실행 중인가?): %w", path, err)
		}
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
		_ = resp.Body.Close()
		switch {
		case resp.StatusCode == http.StatusOK:
			var r struct {
				PartialSuccess map[string]any `json:"partialSuccess"`
			}
			_ = json.Unmarshal(b, &r)
			for k, v := range r.PartialSuccess {
				if s, ok := v.(string); ok && len(k) > 8 && k[:8] == "rejected" && s != "0" {
					return fmt.Errorf("ingress %s rejected %s records: %v", path, s, r.PartialSuccess["errorMessage"])
				}
			}
			return nil
		case (resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode == http.StatusServiceUnavailable) && attempt < 10:
			wait := time.Second
			if s, err := strconv.Atoi(resp.Header.Get("Retry-After")); err == nil && s > 0 {
				wait = time.Duration(s) * time.Second
			}
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(wait):
			}
		default:
			return fmt.Errorf("ingress %s: status %d", path, resp.StatusCode)
		}
	}
}

// exportGRPC는 trace를 OTLP/gRPC로 보낸다. UNAVAILABLE과 RetryInfo 붙은 RESOURCE_EXHAUSTED만 다시 보낸다(OTLP 규격).
// partial success로 일부가 거절되면 오류다.
func exportGRPC(ctx context.Context, addr, token string, td ptrace.Traces) error {
	conn, err := grpc.NewClient(addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return fmt.Errorf("grpc client: %w", err)
	}
	defer func() { _ = conn.Close() }()
	client := ptraceotlp.NewGRPCClient(conn)
	octx := metadata.AppendToOutgoingContext(ctx, "authorization", "Bearer "+token)
	for attempt := 0; ; attempt++ {
		resp, err := client.Export(octx, ptraceotlp.NewExportRequestFromTraces(td))
		if err == nil {
			if n := resp.PartialSuccess().RejectedSpans(); n != 0 {
				return fmt.Errorf("ingress grpc rejected %d spans: %s", n, resp.PartialSuccess().ErrorMessage())
			}
			return nil
		}
		st := grpcstatus.Convert(err)
		wait := time.Duration(0)
		for _, d := range st.Details() {
			if ri, ok := d.(*errdetails.RetryInfo); ok {
				wait = ri.GetRetryDelay().AsDuration()
			}
		}
		retryable := st.Code() == codes.Unavailable || (st.Code() == codes.ResourceExhausted && wait > 0)
		if !retryable || attempt >= 10 {
			return fmt.Errorf("ingress grpc export (make dev 실행 중인가?): %w", err)
		}
		if wait <= 0 {
			wait = time.Second
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(wait):
		}
	}
}
