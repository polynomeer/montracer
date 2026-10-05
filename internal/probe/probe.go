// Package probe는 플랫폼 synthetic probe다 (D04 §10 "1분마다 알려진 trace를 보내 60초 안에 조회·연결·정책 적용 검증",
// ADR 0031).
//
// 고객과 같은 공개 경로(OTLP/HTTP ingress → Kafka → worker → ClickHouse → 조회 API)를 probe 전용 tenant로
// 실제로 지나가 본다. 개별 구성요소 지표가 정상이어도 경로가 끊긴 장애(예: worker는 돌지만 저장이 안 보임)를 잡는다.
//
//	매 주기: 새 trace_id로 3-span trace + 같은 trace_id의 log + 그 trace를 exemplar로 단 heartbeat gauge를 보낸다
//	        → 조회 API로 3 span·구조 완결을 확인한다 (deadline 60초)
//	        → 보낸 PII 표본(이메일)이 응답에 없는지 확인한다 (redaction 정책 적용)
//	        → (선택) 다른 tenant key로 같은 trace가 404인지 확인한다 (tenant 격리)
package probe

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"go.opentelemetry.io/collector/pdata/pcommon"
	"go.opentelemetry.io/collector/pdata/plog"
	"go.opentelemetry.io/collector/pdata/pmetric"
	"go.opentelemetry.io/collector/pdata/ptrace"
)

// 검사 이름. 지표 label 값이다(고정 enum).
const (
	CheckIngest    = "ingest"    // ingress가 trace·log를 200으로 받음
	CheckTrace     = "trace"     // deadline 안에 3 span·complete로 조회됨
	CheckRedaction = "redaction" // PII 표본이 응답에 없음
	CheckIsolation = "isolation" // 다른 tenant key로는 404
)

// Checks는 검사 목록(순서 고정)이다.
var Checks = []string{CheckIngest, CheckTrace, CheckRedaction, CheckIsolation}

// piiSample은 redaction 정책(ADR 0019 패턴)에 걸려야 하는 값이다. 실제 사람의 주소가 아니다(example.com).
const piiSample = "probe-sentinel@example.com"

// redactedMarker는 정책이 이메일을 가린 표식이다 (internal/telemetry/redact).
const redactedMarker = "[REDACTED:email]"

// Config는 Probe 설정이다.
type Config struct {
	IngressURL string // 예: https://ingest.example.com
	QueryURL   string // 예: https://api.example.com
	IngestKey  string // probe tenant의 ingest key (traces·logs)
	APIKey     string // probe tenant의 API key (telemetry.read)
	// OtherAPIKey가 있으면 다른 tenant key로 조회해 404인지 본다(격리 검사).
	OtherAPIKey string
	// Environment는 ingest key의 environment 범위다(기본 synthetic).
	Environment string
	Client      *http.Client
	Logger      *slog.Logger
	Observer    Observer
	Now         func() time.Time
	// Deadline은 전송부터 조회 확인까지 상한이다(기본 60초, D04 §10).
	Deadline time.Duration
	// PollEvery는 조회 재시도 간격이다(기본 2초).
	PollEvery time.Duration
	// Interval은 주기다(기본 1분).
	Interval time.Duration
}

// Result는 검사 하나의 결과다.
type Result struct {
	Check    string
	OK       bool
	Skipped  bool          // 설정이 없어 하지 않은 검사(OtherAPIKey 없음 등)
	Duration time.Duration // trace: 전송부터 조회 성공까지
	Reason   string        // 실패 사유(고정 문구, 응답 본문 없음)
}

// Observer는 검사 결과를 운영 지표로 내보낸다.
type Observer interface {
	ObserveProbe(Result)
}

// Probe는 synthetic probe 실행기다.
type Probe struct{ cfg Config }

// New는 Probe를 만든다.
func New(cfg Config) (*Probe, error) {
	if cfg.IngressURL == "" || cfg.QueryURL == "" || cfg.IngestKey == "" || cfg.APIKey == "" {
		return nil, fmt.Errorf("probe: ingress url, query url, ingest key and api key are required")
	}
	if cfg.Environment == "" {
		cfg.Environment = "synthetic"
	}
	if cfg.Client == nil {
		cfg.Client = &http.Client{Timeout: 10 * time.Second}
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.Deadline <= 0 {
		cfg.Deadline = 60 * time.Second
	}
	if cfg.PollEvery <= 0 {
		cfg.PollEvery = 2 * time.Second
	}
	if cfg.Interval <= 0 {
		cfg.Interval = time.Minute
	}
	cfg.IngressURL = strings.TrimRight(cfg.IngressURL, "/")
	cfg.QueryURL = strings.TrimRight(cfg.QueryURL, "/")
	return &Probe{cfg: cfg}, nil
}

// Run은 ctx가 끝날 때까지 주기를 돈다.
func (p *Probe) Run(ctx context.Context) {
	t := time.NewTicker(p.cfg.Interval)
	defer t.Stop()
	for {
		for _, r := range p.RunOnce(ctx) {
			if p.cfg.Observer != nil && ctx.Err() == nil {
				p.cfg.Observer.ObserveProbe(r)
			}
			if !r.OK && !r.Skipped && ctx.Err() == nil {
				p.cfg.Logger.Warn("synthetic probe check failed", slog.String("check", r.Check), slog.String("reason", r.Reason))
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// RunOnce는 주기 하나를 실행하고 검사별 결과를 돌려준다. 앞 검사가 실패하면 뒤 검사는 실패(의존)로 남긴다.
func (p *Probe) RunOnce(ctx context.Context) []Result {
	var traceID [16]byte
	_, _ = rand.Read(traceID[:])
	tid := hex.EncodeToString(traceID[:])
	start := p.cfg.Now()

	results := make([]Result, 0, len(Checks))
	fail := func(check, reason string) Result { return Result{Check: check, Reason: reason} }

	if err := p.send(ctx, traceID, start); err != nil {
		results = append(results, fail(CheckIngest, err.Error()))
		for _, c := range Checks[1:] {
			results = append(results, fail(c, "ingest failed"))
		}
		return results
	}
	results = append(results, Result{Check: CheckIngest, OK: true})

	body, took, err := p.waitTrace(ctx, tid, start)
	if err != nil {
		results = append(results, fail(CheckTrace, err.Error()), fail(CheckRedaction, "trace not visible"))
	} else {
		results = append(results, Result{Check: CheckTrace, OK: true, Duration: took})
		switch {
		case bytes.Contains(body, []byte(piiSample)):
			results = append(results, fail(CheckRedaction, "pii sample returned unredacted"))
		case !bytes.Contains(body, []byte(redactedMarker)):
			// 속성째 사라진 것도 정책 적용이 아니다(값을 가린 표식이 보여야 한다, ADR 0019)
			results = append(results, fail(CheckRedaction, "redaction marker missing"))
		default:
			results = append(results, Result{Check: CheckRedaction, OK: true})
		}
	}

	switch {
	case p.cfg.OtherAPIKey == "":
		results = append(results, Result{Check: CheckIsolation, Skipped: true})
	case err != nil:
		results = append(results, fail(CheckIsolation, "trace not visible"))
	default:
		status, _, qerr := p.getTrace(ctx, tid, start, p.cfg.OtherAPIKey)
		switch {
		case qerr != nil:
			results = append(results, fail(CheckIsolation, qerr.Error()))
		case status != http.StatusNotFound:
			results = append(results, fail(CheckIsolation, fmt.Sprintf("other tenant got status %d, want 404", status)))
		default:
			results = append(results, Result{Check: CheckIsolation, OK: true})
		}
	}
	return results
}

// send는 3-span trace(root → 2 children)와 같은 trace의 log 하나를 보낸다.
func (p *Probe) send(ctx context.Context, traceID [16]byte, now time.Time) error {
	td := ptrace.NewTraces()
	rs := td.ResourceSpans().AppendEmpty()
	p.resource(rs.Resource().Attributes())
	spans := rs.ScopeSpans().AppendEmpty()
	spans.Scope().SetName("montracer.probe")
	var root pcommon.SpanID
	for i, name := range []string{"probe.root", "probe.child.ingest", "probe.child.query"} {
		s := spans.Spans().AppendEmpty()
		var sid pcommon.SpanID
		_, _ = rand.Read(sid[:])
		s.SetTraceID(traceID)
		s.SetSpanID(sid)
		if i == 0 {
			root = sid
			s.SetKind(ptrace.SpanKindServer)
			// redaction 검사용 표본: 중립 key에 이메일 패턴을 담는다(ADR 0019 패턴 규칙이 가려야 한다)
			s.Attributes().PutStr("probe.note", "contact "+piiSample)
		} else {
			s.SetParentSpanID(root)
			s.SetKind(ptrace.SpanKindInternal)
		}
		s.SetName(name)
		s.SetStartTimestamp(pcommon.NewTimestampFromTime(now.Add(time.Duration(i) * time.Millisecond)))
		s.SetEndTimestamp(pcommon.NewTimestampFromTime(now.Add(time.Duration(i+5) * time.Millisecond)))
	}
	traces, err := (&ptrace.JSONMarshaler{}).MarshalTraces(td)
	if err != nil {
		return fmt.Errorf("encode traces")
	}
	ld := plog.NewLogs()
	rl := ld.ResourceLogs().AppendEmpty()
	p.resource(rl.Resource().Attributes())
	lr := rl.ScopeLogs().AppendEmpty().LogRecords().AppendEmpty()
	lr.SetTimestamp(pcommon.NewTimestampFromTime(now))
	lr.SetTraceID(traceID)
	lr.SetSpanID(root)
	lr.Body().SetStr("synthetic probe")
	logs, err := (&plog.JSONMarshaler{}).MarshalLogs(ld)
	if err != nil {
		return fmt.Errorf("encode logs")
	}
	// metric(exemplar 포함)은 수집 경로만 검사한다. 조회는 rollup watermark(ADR 0026, 2분 지연) 때문에 60초 안에 볼 수 없다(ADR 0031).
	md := pmetric.NewMetrics()
	rm := md.ResourceMetrics().AppendEmpty()
	p.resource(rm.Resource().Attributes())
	m := rm.ScopeMetrics().AppendEmpty().Metrics().AppendEmpty()
	m.SetName("montracer.probe.heartbeat")
	m.SetUnit("1")
	dp := m.SetEmptyGauge().DataPoints().AppendEmpty()
	dp.SetTimestamp(pcommon.NewTimestampFromTime(now))
	dp.SetIntValue(1)
	// D04 §10 "연결 log·metric exemplar": 같은 trace의 root span을 exemplar로 단다
	ex := dp.Exemplars().AppendEmpty()
	ex.SetTimestamp(pcommon.NewTimestampFromTime(now))
	ex.SetIntValue(1)
	ex.SetTraceID(traceID)
	ex.SetSpanID(root)
	metrics, err := (&pmetric.JSONMarshaler{}).MarshalMetrics(md)
	if err != nil {
		return fmt.Errorf("encode metrics")
	}
	for _, s := range []struct {
		path string
		body []byte
	}{{"/v1/traces", traces}, {"/v1/logs", logs}, {"/v1/metrics", metrics}} {
		if err := p.post(ctx, s.path, s.body); err != nil {
			return err
		}
	}
	return nil
}

func (p *Probe) resource(m pcommon.Map) {
	m.PutStr("service.name", "montracer-probe")
	m.PutStr("service.namespace", "montracer")
	m.PutStr("deployment.environment.name", p.cfg.Environment)
}

func (p *Probe) post(ctx context.Context, path string, body []byte) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.cfg.IngressURL+path, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("ingest request")
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+p.cfg.IngestKey)
	resp, err := p.cfg.Client.Do(req)
	if err != nil {
		return fmt.Errorf("ingest %s unreachable", path)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err = io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if err != nil {
		return fmt.Errorf("ingest %s read", path)
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("ingest %s status %d", path, resp.StatusCode)
	}
	if partiallyRejected(body) {
		// quota·cardinality·정책 거절. 응답 문구는 남기지 않는다(고정 사유만).
		return fmt.Errorf("ingest %s partially rejected", path)
	}
	return nil
}

// partiallyRejected는 OTLP JSON 응답의 partialSuccess.rejected*가 0이 아닌지 본다.
// int64는 OTLP JSON에서 문자열이다("1").
func partiallyRejected(body []byte) bool {
	var r struct {
		PartialSuccess map[string]any `json:"partialSuccess"`
	}
	if len(bytes.TrimSpace(body)) == 0 || json.Unmarshal(body, &r) != nil {
		return false
	}
	for k, v := range r.PartialSuccess {
		if !strings.HasPrefix(k, "rejected") {
			continue
		}
		switch n := v.(type) {
		case string:
			if n != "" && n != "0" {
				return true
			}
		case float64:
			if n != 0 {
				return true
			}
		}
	}
	return false
}

// waitTrace는 deadline 안에 3 span·complete 응답이 올 때까지 조회한다.
func (p *Probe) waitTrace(ctx context.Context, tid string, start time.Time) ([]byte, time.Duration, error) {
	deadline := start.Add(p.cfg.Deadline)
	lastReason := "not visible"
	for {
		status, body, err := p.getTrace(ctx, tid, start, p.cfg.APIKey)
		switch {
		case err != nil:
			lastReason = err.Error()
		case status == http.StatusOK:
			var resp struct {
				Data struct {
					SpanCount int  `json:"span_count"`
					Complete  bool `json:"complete"`
				} `json:"data"`
			}
			if json.Unmarshal(body, &resp) != nil {
				lastReason = "unreadable query response"
			} else if resp.Data.SpanCount == 3 && resp.Data.Complete {
				return body, p.cfg.Now().Sub(start), nil
			} else {
				lastReason = fmt.Sprintf("partial trace: %d spans", resp.Data.SpanCount)
			}
		case status == http.StatusNotFound:
			lastReason = "not visible"
		default:
			lastReason = fmt.Sprintf("query status %d", status)
		}
		if !p.cfg.Now().Add(p.cfg.PollEvery).Before(deadline) {
			return nil, 0, fmt.Errorf("trace %s within %s", lastReason, p.cfg.Deadline)
		}
		select {
		case <-ctx.Done():
			return nil, 0, ctx.Err()
		case <-time.After(p.cfg.PollEvery):
		}
	}
}

func (p *Probe) getTrace(ctx context.Context, tid string, start time.Time, key string) (int, []byte, error) {
	from := start.Add(-time.Minute).UTC().Format(time.RFC3339)
	to := start.Add(10 * time.Minute).UTC().Format(time.RFC3339)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, p.cfg.QueryURL+"/api/v1/traces/"+tid+"?from="+from+"&to="+to, nil)
	if err != nil {
		return 0, nil, fmt.Errorf("query request")
	}
	req.Header.Set("Authorization", "Bearer "+key)
	resp, err := p.cfg.Client.Do(req)
	if err != nil {
		return 0, nil, fmt.Errorf("query unreachable")
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return 0, nil, fmt.Errorf("query read")
	}
	return resp.StatusCode, body, nil
}
