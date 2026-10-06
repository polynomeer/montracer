// Package opsmetrics는 플랫폼 자체 운영 지표다 (D04 §10, ADR 0023).
//
// 고객 수집 경로와 독립된 실패 영역에서 운영용 Prometheus가 pull한다(/metrics, 별도 listener).
// 도메인 패키지는 이 패키지를 import하지 않는다. 각 패키지의 Observer interface를 여기서 구현한다.
//
// label 원칙 (D04 §10, Prometheus 명명 규약)
//   - tenant·user·trace ID·key·body 등 무한 값을 label로 쓰지 않는다. tenant별 회계는 usage 원장(D04 §08)이 맡는다.
//   - label 값은 고정 enum이다: signal, status_class, reason, outcome, kind, route(등록 pattern).
//   - 이름은 montracer_<component>_<what>_<unit>, counter는 _total, 시간은 _seconds다.
package opsmetrics

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/polynomeer/montracer/internal/ingest"
	"github.com/polynomeer/montracer/internal/pipeline"
	"github.com/polynomeer/montracer/internal/probe"
	"github.com/polynomeer/montracer/internal/rollup"
)

// NewRegistry는 process·Go runtime collector를 담은 registry다. 전역 DefaultRegisterer는 쓰지 않는다.
func NewRegistry() *prometheus.Registry {
	reg := prometheus.NewRegistry()
	reg.MustRegister(collectors.NewGoCollector(), collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}))
	return reg
}

// Handler는 /metrics handler다. 수집 실패를 숨기지 않도록 오류 시 500을 돌려준다.
func Handler(reg *prometheus.Registry) http.Handler {
	return promhttp.HandlerFor(reg, promhttp.HandlerOpts{ErrorHandling: promhttp.HTTPErrorOnError, Registry: reg})
}

// latencyBuckets는 요청·저장 지연 bucket이다(초). 5ms~30s를 덮는다.
var latencyBuckets = []float64{.005, .01, .025, .05, .1, .25, .5, 1, 2.5, 5, 10, 30}

// StatusClass는 HTTP status를 2xx·4xx·5xx로 묶는다. 0(응답 전 종료)은 "none"이다.
func StatusClass(status int) string {
	if status < 100 {
		return "none"
	}
	return strconv.Itoa(status/100) + "xx"
}

// Ingress는 ingest.Observer 구현이다.
type Ingress struct {
	requests *prometheus.CounterVec   // signal, status_class
	records  *prometheus.CounterVec   // signal, outcome, reason
	duration *prometheus.HistogramVec // signal
	produce  *prometheus.HistogramVec // signal, outcome
	reloads  *prometheus.CounterVec   // outcome
	catalog  *prometheus.CounterVec   // outcome: written | dropped | over_limit | write_error
}

var _ ingest.Observer = (*Ingress)(nil)

// NewIngress는 ingress 지표를 등록한다.
func NewIngress(reg prometheus.Registerer) *Ingress {
	m := &Ingress{
		requests: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "montracer_ingress_requests_total",
			Help: "OTLP requests by signal, transport (http|grpc) and HTTP-equivalent status class.",
		}, []string{"signal", "transport", "status_class"}),
		records: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "montracer_ingress_records_total",
			Help: "OTLP records (span, log record, metric point) by outcome. accepted = durably appended to Kafka; rejected carries a fixed reason.",
		}, []string{"signal", "outcome", "reason"}),
		duration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "montracer_ingress_request_duration_seconds",
			Help:    "OTLP request handling time including the Kafka append wait.",
			Buckets: latencyBuckets,
		}, []string{"signal"}),
		produce: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "montracer_ingress_kafka_append_duration_seconds",
			Help:    "Wait for acks=all Kafka append of one request's records.",
			Buckets: latencyBuckets,
		}, []string{"signal", "outcome"}),
	}
	m.reloads = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "montracer_ingress_quota_overrides_reloads_total",
		Help: "Tenant quota overrides file reloads by outcome. On error the previous overrides stay in effect.",
	}, []string{"outcome"})
	m.catalog = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "montracer_ingress_catalog_services_total",
		Help: "Service catalog registrations: written (services upserted), dropped (queue full, retried by later requests), over_limit (tenant service cap, not registered), write_error (batch failed).",
	}, []string{"outcome"})
	reg.MustRegister(m.requests, m.records, m.duration, m.produce, m.reloads, m.catalog)
	return m
}

// ObserveCatalog는 서비스 catalog 등록 결과를 센다 (catalog.Observer, ADR 0038).
func (m *Ingress) ObserveCatalog(written, dropped, overLimit int, failed bool) {
	if written > 0 {
		m.catalog.WithLabelValues("written").Add(float64(written))
	}
	if dropped > 0 {
		m.catalog.WithLabelValues("dropped").Add(float64(dropped))
	}
	if overLimit > 0 {
		m.catalog.WithLabelValues("over_limit").Add(float64(overLimit))
	}
	if failed {
		m.catalog.WithLabelValues("write_error").Inc()
	}
}

// ObserveOverridesReload는 quota overrides 파일 reload 결과를 센다.
func (m *Ingress) ObserveOverridesReload(ok bool) {
	outcome := "ok"
	if !ok {
		outcome = "error"
	}
	m.reloads.WithLabelValues(outcome).Inc()
}

// ObserveRequest는 요청 하나를 기록한다.
func (m *Ingress) ObserveRequest(r ingest.RequestResult) {
	transport := r.Transport
	if transport != "grpc" {
		transport = "http" // 고정 enum 밖 값이 label을 늘리지 않게
	}
	m.requests.WithLabelValues(r.Signal, transport, StatusClass(r.Status)).Inc()
	m.duration.WithLabelValues(r.Signal).Observe(r.Duration.Seconds())
	if r.Accepted > 0 {
		m.records.WithLabelValues(r.Signal, "accepted", "").Add(float64(r.Accepted))
	}
	for reason, n := range r.Rejected {
		m.records.WithLabelValues(r.Signal, "rejected", reason).Add(float64(n))
	}
	if r.ProduceAttempted {
		outcome := "ok"
		switch {
		case r.ProduceCanceled:
			outcome = "canceled"
		case r.ProduceFailed:
			outcome = "error"
		}
		m.produce.WithLabelValues(r.Signal, outcome).Observe(r.ProduceDuration.Seconds())
	}
}

// Worker는 pipeline.Observer 구현이다.
type Worker struct {
	records    *prometheus.CounterVec   // signal, outcome, reason
	conflicts  *prometheus.CounterVec   // signal
	insert     *prometheus.HistogramVec // signal
	oldestAge  *prometheus.GaugeVec     // signal
	sinkErrors *prometheus.CounterVec   // kind
	commits    *prometheus.CounterVec   // outcome
	lastCommit prometheus.Gauge
	now        func() time.Time
}

var _ pipeline.Observer = (*Worker)(nil)

// NewWorker는 worker 지표를 등록한다.
func NewWorker(reg prometheus.Registerer) *Worker {
	m := &Worker{
		records: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "montracer_worker_records_total",
			Help: "Kafka records consumed by outcome: stored, duplicate, quarantined (with reason). Same unit as ingress accepted records.",
		}, []string{"signal", "outcome", "reason"}),
		conflicts: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "montracer_worker_conflicts_total",
			Help: "Same identity with different content within a batch (first received value kept).",
		}, []string{"signal"}),
		insert: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "montracer_worker_store_duration_seconds",
			Help:    "Time to durably store one partition batch in ClickHouse, including retries.",
			Buckets: latencyBuckets,
		}, []string{"signal"}),
		oldestAge: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "montracer_worker_oldest_record_age_seconds",
			Help: "Age since ingress receipt of the oldest record in the last committed batch. Only meaningful while commits progress; pair with last_commit_timestamp_seconds.",
		}, []string{"signal"}),
		sinkErrors: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "montracer_worker_sink_errors_total",
			Help: "Store failures: transient (retried with the same token) or row_rejected (row sent to quarantine).",
		}, []string{"kind"}),
		commits: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "montracer_worker_offset_commits_total",
			Help: "Kafka offset commits after durable store, by outcome.",
		}, []string{"outcome"}),
		lastCommit: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "montracer_worker_last_commit_timestamp_seconds",
			Help: "Unix time of the last successful offset commit. time() minus this is how long the pipeline has made no progress.",
		}),
		now: time.Now,
	}
	reg.MustRegister(m.records, m.conflicts, m.insert, m.oldestAge, m.sinkErrors, m.commits, m.lastCommit)
	return m
}

// ObserveBatch는 저장을 마친 batch를 기록한다.
func (m *Worker) ObserveBatch(b pipeline.BatchResult) {
	if b.Stored > 0 {
		m.records.WithLabelValues(b.Signal, "stored", "").Add(float64(b.Stored))
	}
	if b.Duplicates > 0 {
		m.records.WithLabelValues(b.Signal, "duplicate", "").Add(float64(b.Duplicates))
	}
	for reason, n := range b.Quarantined {
		m.records.WithLabelValues(b.Signal, "quarantined", reason).Add(float64(n))
	}
	if b.Conflicts > 0 {
		m.conflicts.WithLabelValues(b.Signal).Add(float64(b.Conflicts))
	}
	m.insert.WithLabelValues(b.Signal).Observe(b.InsertDuration.Seconds())
	if b.OldestAge > 0 {
		m.oldestAge.WithLabelValues(b.Signal).Set(b.OldestAge.Seconds())
	}
}

// ObserveSinkError는 저장 실패 한 번을 센다.
func (m *Worker) ObserveSinkError(kind string) { m.sinkErrors.WithLabelValues(kind).Inc() }

// ObserveCommit은 offset commit 결과를 센다.
func (m *Worker) ObserveCommit(ok bool) {
	outcome := "ok"
	if !ok {
		outcome = "error"
	}
	m.commits.WithLabelValues(outcome).Inc()
	if ok {
		m.lastCommit.Set(float64(m.now().UnixNano()) / 1e9)
	}
}

// Query는 조회 API 지표다. httpapi.Observe로 넘긴다.
type Query struct {
	requests *prometheus.CounterVec   // route, status_class
	duration *prometheus.HistogramVec // route
}

// NewQuery는 조회 API 지표를 등록한다.
func NewQuery(reg prometheus.Registerer) *Query {
	m := &Query{
		requests: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "montracer_query_requests_total",
			Help: "Query API requests by route pattern and HTTP status class.",
		}, []string{"route", "status_class"}),
		duration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "montracer_query_request_duration_seconds",
			Help:    "Query API request handling time.",
			Buckets: latencyBuckets,
		}, []string{"route"}),
	}
	reg.MustRegister(m.requests, m.duration)
	return m
}

// Observe는 httpapi.Observe 형태다.
func (m *Query) Observe(route string, status int, d time.Duration) {
	m.requests.WithLabelValues(route, StatusClass(status)).Inc()
	m.duration.WithLabelValues(route).Observe(d.Seconds())
}

// NewControl은 관리 API(control-api) 지표를 등록한다. 형태는 조회 API와 같고 이름만 다르다.
func NewControl(reg prometheus.Registerer) *Query {
	m := &Query{
		requests: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "montracer_control_requests_total",
			Help: "Control API requests by route pattern and HTTP status class.",
		}, []string{"route", "status_class"}),
		duration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "montracer_control_request_duration_seconds",
			Help:    "Control API request handling time.",
			Buckets: latencyBuckets,
		}, []string{"route"}),
	}
	reg.MustRegister(m.requests, m.duration)
	return m
}

// DefaultAddr는 운영 지표 listener 기본 주소다(OTel Prometheus exporter 관례 포트).
// 고객 트래픽 listener와 분리해 외부에 노출하지 않는다.
const DefaultAddr = ":9464"

// Start는 운영 listener를 bind한 뒤 백그라운드에서 serve한다.
// bind 실패(port 충돌 등)는 기동 오류로 돌려준다 — 고객 listener를 열기 전에 실패해야 지표 없이 조용히 돌지 않는다.
// bind 이후 serve 오류는 onError로만 알리고 고객 경로를 내리지 않는다(별도 실패 영역, D04 §10).
func Start(ctx context.Context, srv *http.Server, onError func(error)) error {
	ln, err := (&net.ListenConfig{}).Listen(ctx, "tcp", srv.Addr)
	if err != nil {
		return fmt.Errorf("opsmetrics: listen %s: %w", srv.Addr, err)
	}
	go func() {
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) && onError != nil {
			onError(err)
		}
	}()
	return nil
}

// NewServer는 /metrics와 /healthz만 둔 운영 listener다. extra는 /readyz 등 추가 경로다.
func NewServer(addr string, reg *prometheus.Registry, extra map[string]http.Handler) *http.Server {
	if addr == "" {
		addr = DefaultAddr
	}
	mux := http.NewServeMux()
	mux.Handle("GET /metrics", Handler(reg))
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })
	for pattern, h := range extra {
		mux.Handle(pattern, h)
	}
	return &http.Server{
		Addr:              addr,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       120 * time.Second,
		MaxHeaderBytes:    8 << 10,
	}
}

// Rollup은 rollup job 지표다 (ADR 0026, 0028). resolution(1m·1h)별로 For가 돌려주는 observer를 쓴다.
type Rollup struct {
	cycles      *prometheus.CounterVec // resolution, outcome
	written     *prometheus.CounterVec // resolution
	flags       *prometheus.CounterVec // resolution, flag
	duration    *prometheus.HistogramVec
	lastSuccess *prometheus.GaugeVec
	now         func() time.Time
}

// NewRollup은 rollup 지표를 등록한다.
func NewRollup(reg prometheus.Registerer) *Rollup {
	m := &Rollup{
		cycles: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "montracer_rollup_cycles_total",
			Help: "Metric rollup cycles by resolution and outcome.",
		}, []string{"resolution", "outcome"}),
		written: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "montracer_rollup_windows_written_total",
			Help: "Rollup window rows written (only windows whose content changed).",
		}, []string{"resolution"}),
		flags: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "montracer_rollup_window_flags_total",
			Help: "Written windows by quality flag (missing_baseline, reset, nan_value, negative_delta, ...).",
		}, []string{"resolution", "flag"}),
		duration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "montracer_rollup_cycle_duration_seconds",
			Help:    "Duration of one rollup cycle (read raw points, compute, write changed windows).",
			Buckets: latencyBuckets,
		}, []string{"resolution"}),
		lastSuccess: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "montracer_rollup_last_success_timestamp_seconds",
			Help: "Unix time of the last successful rollup cycle.",
		}, []string{"resolution"}),
		now: time.Now,
	}
	reg.MustRegister(m.cycles, m.written, m.flags, m.duration, m.lastSuccess)
	return m
}

// For는 resolution("1m"·"1h") 하나의 observer다.
// 마지막 성공 시각 series를 0으로 미리 만든다 — 기동부터 계속 실패해도 정체 경보가 울리게(series 부재로 침묵하지 않게).
func (m *Rollup) For(resolution string) rollup.Observer {
	m.lastSuccess.WithLabelValues(resolution)
	return rollupObserver{m: m, res: resolution}
}

type rollupObserver struct {
	m   *Rollup
	res string
}

// ObserveCycle은 주기 하나를 기록한다.
func (o rollupObserver) ObserveCycle(r rollup.CycleResult) {
	m := o.m
	m.duration.WithLabelValues(o.res).Observe(r.Duration.Seconds())
	if !r.OK {
		m.cycles.WithLabelValues(o.res, "error").Inc()
		return
	}
	m.cycles.WithLabelValues(o.res, "ok").Inc()
	m.written.WithLabelValues(o.res).Add(float64(r.Written))
	for f, n := range r.Flags {
		m.flags.WithLabelValues(o.res, f).Add(float64(n))
	}
	m.lastSuccess.WithLabelValues(o.res).Set(float64(m.now().UnixNano()) / 1e9)
}

// Probe는 synthetic probe 지표다 (D04 §10, ADR 0031). label은 check(고정 enum)와 outcome뿐이다.
type Probe struct {
	runs        *prometheus.CounterVec // check, outcome(ok|fail|blocked|skipped)
	failures    *prometheus.GaugeVec   // check: 연속 실패 횟수(성공하면 0)
	successes   *prometheus.GaugeVec   // check: 연속 성공 횟수(실패하면 0) — 복구 확인 "3회 연속 성공"(D04 §11)
	e2e         prometheus.Histogram   // 전송부터 trace 조회 성공까지
	lastRun     prometheus.Gauge       // 마지막 주기 완료 시각
	lastSuccess *prometheus.GaugeVec   // check
	now         func() time.Time
}

// NewProbe는 probe 지표를 등록한다. 검사마다 series를 미리 만든다 — 기동부터 계속 실패해도 경보가 침묵하지 않게.
func NewProbe(reg prometheus.Registerer) *Probe {
	m := &Probe{
		runs: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "montracer_probe_checks_total",
			Help: "Synthetic probe check results by check and outcome.",
		}, []string{"check", "outcome"}),
		failures: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "montracer_probe_consecutive_failures",
			Help: "Consecutive failed runs of a synthetic probe check (0 after a success; blocked runs do not count).",
		}, []string{"check"}),
		successes: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "montracer_probe_consecutive_successes",
			Help: "Consecutive successful runs of a synthetic probe check (0 after a failure or a blocked run).",
		}, []string{"check"}),
		e2e: prometheus.NewHistogram(prometheus.HistogramOpts{
			Name:    "montracer_probe_trace_visible_seconds",
			Help:    "Time from sending the probe trace to seeing all spans through the query API.",
			Buckets: []float64{1, 2, 5, 10, 15, 20, 30, 45, 60},
		}),
		lastRun: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "montracer_probe_last_run_timestamp_seconds",
			Help: "Unix time the synthetic probe last completed a check.",
		}),
		lastSuccess: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "montracer_probe_last_success_timestamp_seconds",
			Help: "Unix time of the last successful run of a synthetic probe check.",
		}, []string{"check"}),
		now: time.Now,
	}
	for _, c := range probe.Checks {
		m.failures.WithLabelValues(c)
		m.successes.WithLabelValues(c)
		m.lastSuccess.WithLabelValues(c)
	}
	// lastRun은 0에서 시작한다(기동 시각을 넣지 않는다). 첫 주기를 끝내기 전에 죽는 crash loop도 "결과 없음"으로 보이게.
	reg.MustRegister(m.runs, m.failures, m.successes, m.e2e, m.lastRun, m.lastSuccess)
	return m
}

// ObserveProbe는 검사 하나의 결과를 기록한다.
func (m *Probe) ObserveProbe(r probe.Result) {
	now := float64(m.now().UnixNano()) / 1e9
	m.lastRun.Set(now)
	switch {
	case r.Skipped:
		m.runs.WithLabelValues(r.Check, "skipped").Inc()
	case r.Blocked:
		// 평가하지 못함: 실패로 세지 않고(보안 check 오경보 방지) 연속 성공도 끊는다(성공도 아니다)
		m.runs.WithLabelValues(r.Check, "blocked").Inc()
		m.successes.WithLabelValues(r.Check).Set(0)
	case r.OK:
		m.runs.WithLabelValues(r.Check, "ok").Inc()
		m.failures.WithLabelValues(r.Check).Set(0)
		m.successes.WithLabelValues(r.Check).Inc()
		m.lastSuccess.WithLabelValues(r.Check).Set(now)
		if r.Check == probe.CheckTrace {
			m.e2e.Observe(r.Duration.Seconds())
		}
	default:
		m.runs.WithLabelValues(r.Check, "fail").Inc()
		m.failures.WithLabelValues(r.Check).Inc()
		m.successes.WithLabelValues(r.Check).Set(0)
	}
}
