package probe

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"go.opentelemetry.io/collector/pdata/pcommon"
	"go.opentelemetry.io/collector/pdata/plog"
	"go.opentelemetry.io/collector/pdata/pmetric"
	"go.opentelemetry.io/collector/pdata/ptrace"
)

// fakeStack은 ingress와 query API를 흉내 낸다. 받은 trace를 저장하고 정책을 흉내 내 조회에 돌려준다.
type fakeStack struct {
	mu          sync.Mutex
	traces      map[string]ptrace.Traces
	logTraceIDs []string
	exTraceIDs  []string
	paths       []string
	polls       int

	ingestStatus int    // 0이면 200
	ingestBody   string // 응답 본문
	failPath     string // 비어 있지 않으면 이 경로에만 ingestStatus·ingestBody를 적용한다
	metaPartial  bool   // 조회 응답 meta.partial
	visibleAfter int    // 이 횟수만큼 조회해야 보인다
	spans        int    // 돌려줄 span 수(0이면 받은 대로)
	noRedact     bool   // 정책 미적용 흉내
	dropAttr     bool   // 속성째 삭제 흉내
	leakOther    bool   // 다른 tenant에도 보임
	otherStatus  int    // 다른 tenant key 응답 status 강제(0이면 정상 동작)
}

func newFake() *fakeStack { return &fakeStack{traces: map[string]ptrace.Traces{}} }

func (f *fakeStack) ingress(t *testing.T) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer ingest-key" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		body, _ := io.ReadAll(r.Body)
		f.mu.Lock()
		defer f.mu.Unlock()
		f.paths = append(f.paths, r.URL.Path)
		switch r.URL.Path {
		case "/v1/traces":
			td, err := (&ptrace.JSONUnmarshaler{}).UnmarshalTraces(body)
			if err != nil {
				t.Errorf("bad traces: %v", err)
			}
			id := td.ResourceSpans().At(0).ScopeSpans().At(0).Spans().At(0).TraceID()
			f.traces[fmt.Sprintf("%x", id[:])] = td
		case "/v1/logs":
			ld, err := (&plog.JSONUnmarshaler{}).UnmarshalLogs(body)
			if err != nil {
				t.Errorf("bad logs: %v", err)
			}
			id := ld.ResourceLogs().At(0).ScopeLogs().At(0).LogRecords().At(0).TraceID()
			f.logTraceIDs = append(f.logTraceIDs, fmt.Sprintf("%x", id[:]))
		case "/v1/metrics":
			md, err := (&pmetric.JSONUnmarshaler{}).UnmarshalMetrics(body)
			if err != nil {
				t.Errorf("bad metrics: %v", err)
			}
			ex := md.ResourceMetrics().At(0).ScopeMetrics().At(0).Metrics().At(0).Gauge().DataPoints().At(0).Exemplars()
			if ex.Len() == 1 {
				id := ex.At(0).TraceID()
				f.exTraceIDs = append(f.exTraceIDs, fmt.Sprintf("%x", id[:]))
			}
		}
		if f.failPath != "" && r.URL.Path != f.failPath {
			return
		}
		if f.ingestStatus != 0 {
			w.WriteHeader(f.ingestStatus)
			return
		}
		_, _ = io.WriteString(w, f.ingestBody)
	})
}

func (f *fakeStack) query() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		id := strings.TrimPrefix(r.URL.Path, "/api/v1/traces/")
		if r.URL.Query().Get("from") == "" || r.URL.Query().Get("to") == "" {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		switch r.Header.Get("Authorization") {
		case "Bearer api-key":
		case "Bearer other-key":
			if f.otherStatus != 0 {
				w.WriteHeader(f.otherStatus)
				return
			}
			if !f.leakOther {
				w.WriteHeader(http.StatusNotFound)
				return
			}
		default:
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		f.polls++
		td, ok := f.traces[id]
		if !ok || f.polls <= f.visibleAfter {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		ss := td.ResourceSpans().At(0).ScopeSpans().At(0).Spans()
		spans := []map[string]any{}
		for i := 0; i < ss.Len(); i++ {
			attrs := map[string]any{}
			ss.At(i).Attributes().Range(func(k string, v pcommon.Value) bool {
				val := v.AsString()
				switch {
				case f.dropAttr:
					return true
				case !f.noRedact && strings.Contains(val, "@"):
					val = "contact [REDACTED:email]"
				}
				attrs[k] = val
				return true
			})
			spans = append(spans, map[string]any{"name": ss.At(i).Name(), "attributes": attrs})
		}
		n := len(spans)
		if f.spans != 0 {
			n = f.spans
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"data": map[string]any{"trace_id": id, "spans": spans, "span_count": n, "complete": n == 3},
			"meta": map[string]any{"partial": f.metaPartial},
		})
	})
}

func run(t *testing.T, f *fakeStack, other bool) map[string]Result {
	t.Helper()
	ing := httptest.NewServer(f.ingress(t))
	t.Cleanup(ing.Close)
	q := httptest.NewServer(f.query())
	t.Cleanup(q.Close)
	cfg := Config{
		IngressURL: ing.URL + "/", QueryURL: q.URL,
		IngestKey: "ingest-key", APIKey: "api-key",
		Deadline: 300 * time.Millisecond, PollEvery: 10 * time.Millisecond,
	}
	if other {
		cfg.OtherAPIKey = "other-key"
	}
	p, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]Result{}
	for _, r := range p.RunOnce(context.Background()) {
		out[r.Check] = r
	}
	if len(out) != len(Checks) {
		t.Fatalf("results = %v, want every check", out)
	}
	return out
}

func TestRunOnceAllChecksPass(t *testing.T) {
	f := newFake()
	f.visibleAfter = 3
	res := run(t, f, true)
	for _, c := range Checks {
		if !res[c].OK {
			t.Errorf("%s failed: %q", c, res[c].Reason)
		}
	}
	if res[CheckTrace].Duration <= 0 {
		t.Error("trace duration not measured")
	}
	if got := strings.Join(f.paths, ","); got != "/v1/traces,/v1/logs,/v1/metrics" {
		t.Errorf("paths = %s", got)
	}
	// log는 같은 trace에 연결된다
	if len(f.logTraceIDs) != 1 || f.traces[f.logTraceIDs[0]].SpanCount() != 3 {
		t.Errorf("log not linked to the 3-span trace: %v", f.logTraceIDs)
	}
	if len(f.exTraceIDs) != 1 || f.exTraceIDs[0] != f.logTraceIDs[0] {
		t.Errorf("metric exemplar not linked to the trace: %v", f.exTraceIDs)
	}
}

func TestRunOnceTraceIDsDifferPerRun(t *testing.T) {
	f := newFake()
	run(t, f, false)
	run(t, f, false)
	if len(f.traces) != 2 {
		t.Errorf("traces = %d, want a fresh trace_id per run", len(f.traces))
	}
}

// 자기 tenant에서 안 보이면 trace만 실패다. redaction·isolation은 평가하지 못했으므로 blocked(보안 경보로 번지지 않음).
func TestRunOnceTraceNotVisibleWithinDeadline(t *testing.T) {
	f := newFake()
	f.visibleAfter = 1 << 30
	res := run(t, f, true)
	if !res[CheckIngestTraces].OK || res[CheckTrace].OK || res[CheckTrace].Blocked {
		t.Fatalf("results = %+v", res)
	}
	for _, c := range []string{CheckRedaction, CheckIsolation} {
		if r := res[c]; !r.Blocked || r.OK {
			t.Errorf("%s = %+v, want blocked", c, r)
		}
	}
	if !strings.Contains(res[CheckTrace].Reason, "not visible") {
		t.Errorf("reason = %q", res[CheckTrace].Reason)
	}
}

func TestRunOncePartialQueryResponseFails(t *testing.T) {
	f := newFake()
	f.metaPartial = true
	res := run(t, f, false)
	if res[CheckTrace].OK || !strings.Contains(res[CheckTrace].Reason, "partial query response") {
		t.Fatalf("trace = %+v", res[CheckTrace])
	}
}

// 조회가 deadline을 넘겨 끝나면 성공으로 세지 않는다(진행 중 요청도 deadline에서 끊는다).
func TestRunOnceSlowQueryCutAtDeadline(t *testing.T) {
	f := newFake()
	slow := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-time.After(time.Second):
		case <-r.Context().Done():
			return
		}
		f.query().ServeHTTP(w, r)
	})
	ing := httptest.NewServer(f.ingress(t))
	defer ing.Close()
	q := httptest.NewServer(slow)
	defer q.Close()
	p, err := New(Config{IngressURL: ing.URL, QueryURL: q.URL, IngestKey: "ingest-key", APIKey: "api-key",
		Deadline: 200 * time.Millisecond, PollEvery: 10 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	began := time.Now()
	for _, r := range p.RunOnce(context.Background()) {
		if r.Check == CheckTrace && r.OK {
			t.Fatal("trace ok after deadline")
		}
	}
	if d := time.Since(began); d > 800*time.Millisecond {
		t.Errorf("RunOnce took %s, want the poll cut at the deadline", d)
	}
}

func TestRunOncePartialTraceFails(t *testing.T) {
	f := newFake()
	f.spans = 2
	res := run(t, f, false)
	if res[CheckTrace].OK || !strings.Contains(res[CheckTrace].Reason, "partial trace: 2 spans") {
		t.Fatalf("trace = %+v", res[CheckTrace])
	}
}

func TestRunOnceRedactionChecks(t *testing.T) {
	for name, set := range map[string]func(*fakeStack){
		"raw email returned": func(f *fakeStack) { f.noRedact = true },
		"attribute dropped":  func(f *fakeStack) { f.dropAttr = true },
	} {
		t.Run(name, func(t *testing.T) {
			f := newFake()
			set(f)
			res := run(t, f, false)
			if !res[CheckTrace].OK || res[CheckRedaction].OK {
				t.Fatalf("results = %+v", res)
			}
			if strings.Contains(res[CheckRedaction].Reason, piiSample) {
				t.Error("reason leaks the sample")
			}
		})
	}
}

func TestRunOnceIsolation(t *testing.T) {
	f := newFake()
	f.leakOther = true
	res := run(t, f, true)
	if res[CheckIsolation].OK || !strings.Contains(res[CheckIsolation].Reason, "status 200") {
		t.Fatalf("isolation = %+v", res[CheckIsolation])
	}
	if r := run(t, newFake(), false)[CheckIsolation]; !r.Skipped || r.OK {
		t.Errorf("without other key = %+v, want skipped", r)
	}
	// 다른 key가 거절되면(401) 격리를 판정하지 못한 것: blocked, 실패 아님
	f = newFake()
	f.otherStatus = http.StatusUnauthorized
	if r := run(t, f, true)[CheckIsolation]; !r.Blocked || r.OK || !strings.Contains(r.Reason, "status 401") {
		t.Errorf("other key rejected = %+v, want blocked", r)
	}
}

func TestRunOnceIngestFailures(t *testing.T) {
	// trace 수집 실패: trace·redaction·isolation은 blocked, 실패로 세지 않는다
	for name, set := range map[string]func(*fakeStack){
		"status":  func(f *fakeStack) { f.ingestStatus = http.StatusServiceUnavailable },
		"partial": func(f *fakeStack) { f.ingestBody = `{"partialSuccess":{"rejectedSpans":"1","errorMessage":"quota"}}` },
	} {
		t.Run(name, func(t *testing.T) {
			f := newFake()
			set(f)
			f.failPath = "/v1/traces"
			res := run(t, f, true)
			if res[CheckIngestTraces].OK || !res[CheckIngestLogs].OK || !res[CheckIngestMetrics].OK {
				t.Fatalf("ingest = %+v", res)
			}
			for _, c := range []string{CheckTrace, CheckRedaction, CheckIsolation} {
				if r := res[c]; !r.Blocked || r.OK {
					t.Errorf("%s = %+v, want blocked", c, r)
				}
			}
			if strings.Contains(res[CheckIngestTraces].Reason, "quota") {
				t.Errorf("reason echoes response text: %q", res[CheckIngestTraces].Reason)
			}
		})
	}
}

// metric 거절(cardinality·quota)은 ingest_metrics만 실패시키고 trace 검사는 계속한다.
func TestRunOnceMetricIngestFailureDoesNotBlockTrace(t *testing.T) {
	f := newFake()
	f.failPath = "/v1/metrics"
	f.ingestBody = `{"partialSuccess":{"rejectedDataPoints":"1"}}`
	res := run(t, f, true)
	if res[CheckIngestMetrics].OK || !strings.Contains(res[CheckIngestMetrics].Reason, "partially rejected") {
		t.Fatalf("ingest_metrics = %+v", res[CheckIngestMetrics])
	}
	for _, c := range []string{CheckIngestTraces, CheckIngestLogs, CheckTrace, CheckRedaction, CheckIsolation} {
		if !res[c].OK {
			t.Errorf("%s = %+v, want ok", c, res[c])
		}
	}
}

func TestPartiallyRejected(t *testing.T) {
	for body, want := range map[string]bool{
		``:                      false,
		`{}`:                    false,
		`{"partialSuccess":{}}`: false,
		`{"partialSuccess":{"rejectedSpans":"0"}}`:           false,
		`{"partialSuccess":{"rejectedLogRecords":"2"}}`:      true,
		`{"partialSuccess":{"rejectedDataPoints":3}}`:        true,
		`{"partialSuccess":{"errorMessage":"warning only"}}`: false,
	} {
		if got := partiallyRejected([]byte(body)); got != want {
			t.Errorf("%s = %v, want %v", body, got, want)
		}
	}
}

func TestNewRequiresEndpointsAndKeys(t *testing.T) {
	if _, err := New(Config{IngressURL: "x", QueryURL: "y", IngestKey: "k"}); err == nil {
		t.Error("missing api key accepted")
	}
}

type recorder struct {
	mu  sync.Mutex
	got []Result
}

func (r *recorder) ObserveProbe(res Result) { r.mu.Lock(); r.got = append(r.got, res); r.mu.Unlock() }

func TestRunObservesEveryCheck(t *testing.T) {
	f := newFake()
	ing := httptest.NewServer(f.ingress(t))
	defer ing.Close()
	q := httptest.NewServer(f.query())
	defer q.Close()
	rec := &recorder{}
	p, err := New(Config{IngressURL: ing.URL, QueryURL: q.URL, IngestKey: "ingest-key", APIKey: "api-key",
		Deadline: 200 * time.Millisecond, PollEvery: 5 * time.Millisecond, Interval: time.Hour, Observer: rec})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { p.Run(ctx); close(done) }()
	deadline := time.Now().Add(2 * time.Second)
	for {
		rec.mu.Lock()
		n := len(rec.got)
		rec.mu.Unlock()
		if n == len(Checks) || time.Now().After(deadline) {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	<-done
	if len(rec.got) != len(Checks) {
		t.Fatalf("observed %d results, want %d", len(rec.got), len(Checks))
	}
}
