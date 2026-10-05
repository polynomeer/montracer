package opsmetrics

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/polynomeer/montracer/internal/ingest"
	"github.com/polynomeer/montracer/internal/pipeline"
	"github.com/polynomeer/montracer/internal/rollup"
)

func TestIngress(t *testing.T) {
	reg := NewRegistry()
	m := NewIngress(reg)
	m.ObserveRequest(ingest.RequestResult{Signal: "traces", Status: 200, Accepted: 4,
		Rejected: map[string]int{"environment_not_allowed": 2}, Duration: 30 * time.Millisecond, ProduceAttempted: true, ProduceDuration: 10 * time.Millisecond})
	m.ObserveRequest(ingest.RequestResult{Signal: "traces", Status: 503, Duration: time.Second, ProduceAttempted: true, ProduceDuration: time.Second, ProduceFailed: true})
	m.ObserveRequest(ingest.RequestResult{Signal: "logs", Status: 401})

	if got := testutil.ToFloat64(m.records.WithLabelValues("traces", "accepted", "")); got != 4 {
		t.Errorf("accepted = %v", got)
	}
	if got := testutil.ToFloat64(m.records.WithLabelValues("traces", "rejected", "environment_not_allowed")); got != 2 {
		t.Errorf("rejected = %v", got)
	}
	if got := testutil.ToFloat64(m.requests.WithLabelValues("traces", "5xx")); got != 1 {
		t.Errorf("5xx = %v", got)
	}
	if got := testutil.ToFloat64(m.requests.WithLabelValues("logs", "4xx")); got != 1 {
		t.Errorf("4xx = %v", got)
	}
	if n := testutil.CollectAndCount(m.produce); n != 2 {
		t.Errorf("produce series = %d, want ok+error", n)
	}
}

func TestWorker(t *testing.T) {
	reg := NewRegistry()
	m := NewWorker(reg)
	m.ObserveBatch(pipeline.BatchResult{Signal: "metrics", Records: 7, Stored: 4, Duplicates: 1, Conflicts: 1,
		Quarantined: map[string]int{"conflicting_point_value": 1, "decode_failed": 1}, InsertDuration: 20 * time.Millisecond, OldestAge: 90 * time.Second})
	m.ObserveSinkError("transient")
	m.now = func() time.Time { return time.Unix(1791158400, 0) }
	m.ObserveCommit(true)
	m.ObserveCommit(false)
	if got := testutil.ToFloat64(m.lastCommit); got != 1791158400 {
		t.Errorf("last commit = %v (실패한 commit은 갱신하지 않는다)", got)
	}
	if got := testutil.ToFloat64(m.records.WithLabelValues("metrics", "stored", "")); got != 4 {
		t.Errorf("stored = %v", got)
	}
	if got := testutil.ToFloat64(m.records.WithLabelValues("metrics", "quarantined", "decode_failed")); got != 1 {
		t.Errorf("quarantined = %v", got)
	}
	if got := testutil.ToFloat64(m.oldestAge.WithLabelValues("metrics")); got != 90 {
		t.Errorf("oldest age = %v", got)
	}
	if got := testutil.ToFloat64(m.commits.WithLabelValues("error")); got != 1 {
		t.Errorf("commit errors = %v", got)
	}
}

// label에 tenant·사용자 식별자 같은 무한 값이 들어가지 않는다 (D04 §10).
func TestNoUnboundedLabels(t *testing.T) {
	reg := NewRegistry()
	NewIngress(reg).ObserveRequest(ingest.RequestResult{Signal: "traces", Status: 200, Accepted: 1})
	NewWorker(reg).ObserveBatch(pipeline.BatchResult{Signal: "traces", Stored: 1})
	NewQuery(reg).Observe("GET /api/v1/traces/{trace_id}", 200, time.Millisecond)
	mfs, err := reg.Gather()
	if err != nil {
		t.Fatal(err)
	}
	allowed := map[string]bool{"signal": true, "status_class": true, "reason": true, "outcome": true, "kind": true, "route": true, "flag": true}
	for _, mf := range mfs {
		if !strings.HasPrefix(mf.GetName(), "montracer_") {
			continue
		}
		for _, m := range mf.GetMetric() {
			for _, l := range m.GetLabel() {
				if !allowed[l.GetName()] {
					t.Errorf("%s has label %q outside the bounded set", mf.GetName(), l.GetName())
				}
			}
		}
	}
}

func TestServer(t *testing.T) {
	reg := NewRegistry()
	NewQuery(reg).Observe("GET /api/v1/traces/{trace_id}", 404, time.Millisecond)
	srv := httptest.NewServer(NewServer("", reg, nil).Handler)
	defer srv.Close()
	resp, err := http.Get(srv.URL + "/metrics") //nolint:noctx // 시험
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if !strings.Contains(string(body), `montracer_query_requests_total{route="GET /api/v1/traces/{trace_id}",status_class="4xx"} 1`) ||
		!strings.Contains(string(body), "go_goroutines") {
		t.Errorf("metrics body missing series:\n%s", body)
	}
}

func TestStatusClass(t *testing.T) {
	for status, want := range map[int]string{0: "none", 200: "2xx", 413: "4xx", 503: "5xx"} {
		if got := StatusClass(status); got != want {
			t.Errorf("StatusClass(%d) = %s", status, got)
		}
	}
}

// 경보 규칙이 참조하는 지표는 실제로 등록된 이름이어야 하고(이름 변경 시 경보가 조용히 죽는 것 방지),
// 모든 경보는 runbook의 같은 이름 절을 가리켜야 한다 (ADR 0023).
func TestAlertRulesMatchMetricsAndRunbooks(t *testing.T) {
	root := filepath.Join("..", "..")
	rules, err := os.ReadFile(filepath.Join(root, "deploy", "prometheus", "rules", "montracer.rules.yml")) //nolint:gosec // 레포 안 고정 경로
	if err != nil {
		t.Fatal(err)
	}
	reg := prometheus.NewRegistry()
	in, wk, q, ro := NewIngress(reg), NewWorker(reg), NewQuery(reg), NewRollup(reg)
	collectors := []prometheus.Collector{in.requests, in.records, in.duration, in.produce, in.reloads,
		wk.records, wk.conflicts, wk.insert, wk.oldestAge, wk.sinkErrors, wk.commits, wk.lastCommit, q.requests, q.duration,
		ro.cycles, ro.written, ro.flags, ro.duration, ro.lastSuccess}
	known := map[string]bool{}
	fqName := regexp.MustCompile(`fqName: "([^"]+)"`)
	for _, c := range collectors {
		ch := make(chan *prometheus.Desc, 4)
		go func() { c.Describe(ch); close(ch) }()
		for d := range ch {
			if m := fqName.FindStringSubmatch(d.String()); m != nil {
				known[m[1]] = true
			}
		}
	}
	suffix := regexp.MustCompile(`_(bucket|count|sum)$`)
	for _, name := range regexp.MustCompile(`montracer_[a-z_]+`).FindAllString(string(rules), -1) {
		if !known[name] && !known[suffix.ReplaceAllString(name, "")] {
			t.Errorf("rule references unknown metric %s", name)
		}
	}
	for _, m := range regexp.MustCompile(`alert: (\S+)[^\n]*\n(?s:.*?)runbook_url: "docs/runbooks/([^"#]+)(?:#([^"]+))?"`).FindAllStringSubmatch(string(rules), -1) {
		body, err := os.ReadFile(filepath.Join(root, "docs", "runbooks", filepath.Base(m[2]))) //nolint:gosec // 레포 runbook 디렉터리 안으로 제한
		if err != nil {
			t.Errorf("%s: runbook %s: %v", m[1], m[2], err)
			continue
		}
		if m[3] == "" {
			continue
		}
		if m[3] != strings.ToLower(m[1]) || !strings.Contains(strings.ToLower(string(body)), "\n### "+m[3]+"\n") {
			t.Errorf("%s: runbook %s has no section #%s", m[1], m[2], m[3])
		}
	}
}

// 지표 port가 이미 쓰이고 있으면 기동 오류다(고객 listener를 열기 전에 실패).
func TestStartFailsFastOnBindError(t *testing.T) {
	busy := httptest.NewServer(http.NotFoundHandler())
	defer busy.Close()
	addr := strings.TrimPrefix(busy.URL, "http://")
	if err := Start(context.Background(), NewServer(addr, NewRegistry(), nil), nil); err == nil {
		t.Fatal("expected bind error")
	}
	srv := NewServer("127.0.0.1:0", NewRegistry(), nil)
	if err := Start(context.Background(), srv, nil); err != nil {
		t.Fatal(err)
	}
	_ = srv.Close()
}

func TestRollup(t *testing.T) {
	m := NewRollup(NewRegistry())
	m.now = func() time.Time { return time.Unix(1791158400, 0) }
	m.ObserveCycle(rollup.CycleResult{OK: true, Written: 3, Flags: map[string]int{"missing_baseline": 2}})
	m.ObserveCycle(rollup.CycleResult{OK: false})
	if testutil.ToFloat64(m.written) != 3 || testutil.ToFloat64(m.flags.WithLabelValues("missing_baseline")) != 2 ||
		testutil.ToFloat64(m.cycles.WithLabelValues("error")) != 1 || testutil.ToFloat64(m.lastSuccess) != 1791158400 {
		t.Error("rollup metrics not recorded")
	}
}
