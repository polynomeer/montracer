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
	visibleAfter int    // 이 횟수만큼 조회해야 보인다
	spans        int    // 돌려줄 span 수(0이면 받은 대로)
	noRedact     bool   // 정책 미적용 흉내
	dropAttr     bool   // 속성째 삭제 흉내
	leakOther    bool   // 다른 tenant에도 보임
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
			"meta": map[string]any{},
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

func TestRunOnceTraceNotVisibleWithinDeadline(t *testing.T) {
	f := newFake()
	f.visibleAfter = 1 << 30
	res := run(t, f, true)
	if !res[CheckIngest].OK || res[CheckTrace].OK || res[CheckRedaction].OK || res[CheckIsolation].OK {
		t.Fatalf("results = %+v", res)
	}
	if !strings.Contains(res[CheckTrace].Reason, "not visible") {
		t.Errorf("reason = %q", res[CheckTrace].Reason)
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
}

func TestRunOnceIngestFailures(t *testing.T) {
	for name, set := range map[string]func(*fakeStack){
		"status":  func(f *fakeStack) { f.ingestStatus = http.StatusServiceUnavailable },
		"partial": func(f *fakeStack) { f.ingestBody = `{"partialSuccess":{"rejectedSpans":"1","errorMessage":"quota"}}` },
	} {
		t.Run(name, func(t *testing.T) {
			res := run(t, func() *fakeStack { f := newFake(); set(f); return f }(), true)
			for _, c := range Checks {
				if res[c].OK {
					t.Errorf("%s ok after ingest failure", c)
				}
			}
			if strings.Contains(res[CheckIngest].Reason, "quota") {
				t.Errorf("reason echoes response text: %q", res[CheckIngest].Reason)
			}
		})
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
