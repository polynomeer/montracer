//go:build integration

package telemetrystore

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/polynomeer/montracer/internal/authz"
	"github.com/polynomeer/montracer/internal/queryplan"
)

type searchSpan struct {
	tenant   authz.TenantID
	service  string
	traceID  string
	spanID   string
	parentID string // "" = root
	name     string
	start    time.Time
	dur      time.Duration
	status   uint8
	attrs    map[string]string
	version  uint64
	expires  time.Time
	received time.Time
}

func insertSearchSpans(t *testing.T, rows ...searchSpan) {
	t.Helper()
	ctx := context.Background()
	conn := rawConn(t, "MONTRACER_TEST_CH_INGEST_DSN")
	batch, err := conn.PrepareBatch(ctx, `INSERT INTO spans_local
		(tenant_id, service_id, trace_id, span_id, parent_span_id, name, event_time, duration_ns,
		 received_at, status, span_kind, attributes, payload, payload_hash, version, expires_at)`)
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	for _, r := range rows {
		tid, _ := hex.DecodeString(r.traceID)
		sid, _ := hex.DecodeString(r.spanID)
		pid := make([]byte, 8)
		if r.parentID != "" {
			pid, _ = hex.DecodeString(r.parentID)
		}
		if r.attrs == nil {
			r.attrs = map[string]string{}
		}
		if r.received.IsZero() {
			r.received = r.start
		}
		if r.dur < 0 {
			t.Fatalf("negative span duration %v", r.dur)
		}
		if err := batch.Append(r.tenant.String(), r.service, string(tid), string(sid), string(pid), r.name,
			r.start, uint64(r.dur), r.received, //nolint:gosec // 위에서 음수 제외 r.status, uint8(2), r.attrs,
			"{}", string(make([]byte, 32)), r.version, r.expires); err != nil {
			t.Fatalf("append: %v", err)
		}
	}
	if err := batch.Send(); err != nil {
		t.Fatalf("send: %v", err)
	}
}

func parse(t *testing.T, s string) *queryplan.Node {
	t.Helper()
	var n queryplan.Node
	if err := json.Unmarshal([]byte(s), &n); err != nil {
		t.Fatal(err)
	}
	return &n
}

func traceIDs(ts []TraceSummary) string {
	parts := make([]string, len(ts))
	for i, x := range ts {
		parts[i] = x.TraceID[:4]
	}
	return strings.Join(parts, ",")
}

// trace 검색(ADR 0043): 요약 값, span·요약 filter, keyset, 재전송 dedup, 만료, 범위 경계, tenant·environment 범위.
func TestSearchTraces(t *testing.T) {
	s := openQuery(t)
	ctx := context.Background()
	tenantA, viewerA := newTenant(t)
	tenantB, viewerB := newTenant(t)
	now := time.Now().UTC().Truncate(time.Millisecond)
	exp := now.Add(7 * 24 * time.Hour)
	base := now.Add(-30 * time.Minute)
	const (
		svcB = "bbbbbbbb-0000-4000-8000-000000000002"
		t1   = "1111aaaaaaaaaaaaaaaaaaaaaaaaaaaa"
		t2   = "2222aaaaaaaaaaaaaaaaaaaaaaaaaaaa"
		t3   = "3333aaaaaaaaaaaaaaaaaaaaaaaaaaaa"
		t4   = "4444aaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	)
	sp := func(trace, span, parent, svc, name string, startOff, dur time.Duration, status uint8) searchSpan {
		return searchSpan{tenant: tenantA, service: svc, traceID: trace, spanID: span, parentID: parent, name: name,
			start: base.Add(startOff), dur: dur, status: status, version: 10, expires: exp,
			attrs: map[string]string{"http.route": "/" + name}}
	}
	insertSearchSpans(t,
		// t1: 3 span, payment(svcB)에서 오류, root 0~840ms
		sp(t1, "0000000000000001", "", svcA, "checkout", 0, 840*time.Millisecond, 1),
		sp(t1, "0000000000000002", "0000000000000001", svcB, "charge", 60*time.Millisecond, 500*time.Millisecond, 2),
		sp(t1, "0000000000000003", "0000000000000002", svcB, "select", 70*time.Millisecond, 400*time.Millisecond, 0),
		// t2: 1 span, 정상, t1보다 늦게 시작
		sp(t2, "0000000000000004", "", svcA, "cart", time.Minute, 30*time.Millisecond, 0),
		// t3: root 없음(부모가 수신되지 않음)
		sp(t3, "0000000000000005", "00000000000000ff", svcB, "orphan", 2*time.Minute, 10*time.Millisecond, 0),
		// t4: 범위 시작 전에 시작한 trace — span 하나는 범위 안이어도 목록에는 다른 범위에서 한 번만 나온다
		sp(t4, "0000000000000006", "", svcA, "early", -20*time.Minute, 15*time.Minute, 0),
		sp(t4, "0000000000000007", "0000000000000006", svcA, "late-child", -6*time.Minute, time.Minute, 0),
	)
	// 재전송(같은 span, 뒤 수신 = 작은 version)과 만료된 trace
	dup := sp(t1, "0000000000000002", "0000000000000001", svcB, "charge", 60*time.Millisecond, 500*time.Millisecond, 2)
	dup.version = 5
	expired := sp("5555aaaaaaaaaaaaaaaaaaaaaaaaaaaa", "0000000000000008", "", svcA, "gone", 3*time.Minute, time.Millisecond, 0)
	expired.expires = now.Add(-time.Minute)
	other := sp(t1, "0000000000000009", "", svcA, "tenant-b-secret", 0, time.Second, 2)
	other.tenant = tenantB
	insertSearchSpans(t, dup, expired, other)

	compile := func(f string) (queryplan.Compiled, queryplan.Compiled) {
		t.Helper()
		var n *queryplan.Node
		if f != "" {
			n = parse(t, f)
		}
		spanF, sumF, err := queryplan.Split(n, queryplan.IsTraceSummaryField)
		if err != nil {
			t.Fatal(err)
		}
		a, err := queryplan.CompileWith(spanF, queryplan.TraceSpanCatalog, queryplan.Options{ParamPrefix: "s", ServiceIDs: map[string][]string{}})
		if err != nil {
			t.Fatal(err)
		}
		b, err := queryplan.CompileWith(sumF, queryplan.TraceSummaryCatalog, queryplan.Options{ParamPrefix: "t"})
		if err != nil {
			t.Fatal(err)
		}
		return a, b
	}
	rng := TimeRange{From: base.Add(-5 * time.Minute), To: now}
	run := func(p authz.Principal, f string, limit int, after *TracePosition, envs []string) ([]TraceSummary, bool) {
		t.Helper()
		a, b := compile(f)
		got, more, err := s.SearchTraces(ctx, p, TraceSearchQuery{Range: rng, SpanFilter: a, TraceFilter: b, Limit: limit, After: after, EnvironmentServices: envs}, now)
		if err != nil {
			t.Fatalf("search %s: %v", f, err)
		}
		return got, more
	}

	all, more := run(viewerA, "", 10, nil, nil)
	if traceIDs(all) != "3333,2222,1111" || more {
		t.Fatalf("all = %s more=%v", traceIDs(all), more)
	}
	byID := map[string]TraceSummary{}
	for _, x := range all {
		byID[x.TraceID[:4]] = x
	}
	if x := byID["1111"]; x.SpanCount != 3 || !x.HasError || x.Roots != 1 || x.RootServiceID != svcA || x.RootName != "checkout" ||
		x.MissingParent || x.DurationMs != 840 || !x.Start.Equal(base) || x.LastReceived.Before(base) {
		t.Errorf("t1 summary = %+v (재전송은 한 span으로, 오류 span이 있으면 has_error)", x)
	}
	if x := byID["3333"]; x.Roots != 0 || x.RootServiceID != "" || !x.MissingParent || x.HasError {
		t.Errorf("t3 summary = %+v (root 없음·부모 누락)", x)
	}
	// span filter: payment 서비스의 span이 있는 trace / route 속성 / status=error
	if got, _ := run(viewerA, fmt.Sprintf(`{"field":"service_id","op":"eq","value":%q}`, svcB), 10, nil, nil); traceIDs(got) != "3333,1111" {
		t.Errorf("service_id = %s", traceIDs(got))
	}
	if got, _ := run(viewerA, `{"field":"attributes.http.route","op":"eq","value":"/cart"}`, 10, nil, nil); traceIDs(got) != "2222" {
		t.Errorf("route = %s", traceIDs(got))
	}
	// 요약 filter: 오류 trace, 500ms 이상, 그리고 span·요약 조건의 and
	if got, _ := run(viewerA, `{"field":"has_error","op":"eq","value":true}`, 10, nil, nil); traceIDs(got) != "1111" {
		t.Errorf("has_error = %s", traceIDs(got))
	}
	if got, _ := run(viewerA, `{"field":"duration_ms","op":"gte","value":500}`, 10, nil, nil); traceIDs(got) != "1111" {
		t.Errorf("duration = %s", traceIDs(got))
	}
	if got, _ := run(viewerA, `{"op":"and","args":[{"field":"name","op":"eq","value":"cart"},{"field":"has_error","op":"eq","value":true}]}`, 10, nil, nil); traceIDs(got) != "" {
		t.Errorf("cart and error = %s", traceIDs(got))
	}
	// keyset: 1개씩 끝까지, 같은 trace가 두 번 나오지 않는다
	var pages []string
	var after *TracePosition
	for i := 0; i < 5; i++ {
		got, more := run(viewerA, "", 1, after, nil)
		if len(got) != 1 {
			t.Fatalf("page %d = %s", i, traceIDs(got))
		}
		pages = append(pages, traceIDs(got))
		after = &TracePosition{Start: got[0].Start, TraceID: got[0].TraceID}
		if !more {
			break
		}
	}
	if strings.Join(pages, ",") != "3333,2222,1111" {
		t.Errorf("pages = %v", pages)
	}
	// 범위 시작 전에 시작한 t4는 그 시작이 든 범위에서 나온다(범위 안 span이 있어도 다른 범위에 중복으로 나오지 않는다)
	early := rng
	early.From, early.To = base.Add(-25*time.Minute), base.Add(-10*time.Minute)
	a, b := compile("")
	if got, _, err := s.SearchTraces(ctx, viewerA, TraceSearchQuery{Range: early, SpanFilter: a, TraceFilter: b, Limit: 10}, now); err != nil || traceIDs(got) != "4444" || got[0].SpanCount != 2 {
		t.Errorf("t4 in its own range = %v %v", got, err)
	}
	// tenant B는 같은 trace_id의 자기 span만 본다(A의 요약이 섞이지 않는다)
	if got, _ := run(viewerB, "", 10, nil, nil); traceIDs(got) != "1111" || got[0].RootName != "tenant-b-secret" || got[0].SpanCount != 1 {
		t.Errorf("tenant B = %+v", got)
	}
	// environment 제한 key: 범위가 없으면 403, svcA만이면 svcB span은 보이지 않고 그 자식은 부모 누락으로 보인다
	prodKey := keyPrincipal(t, tenantA, []string{"prod"})
	if _, _, err := s.SearchTraces(ctx, prodKey, TraceSearchQuery{Range: rng, SpanFilter: a, TraceFilter: b, Limit: 1}, now); !errors.Is(err, authz.ErrForbidden) {
		t.Errorf("environment-scoped without scope = %v", err)
	}
	scoped, _ := run(prodKey, "", 10, nil, []string{svcA})
	if traceIDs(scoped) != "2222,1111" || scoped[1].SpanCount != 1 || scoped[1].HasError {
		t.Errorf("svcA scope = %+v (svcB span·오류가 보이면 안 된다)", scoped)
	}
	// 입력 검증: 범위 상한, limit, 풀지 않은 service.name, 잘못된 keyset
	unresolved, _ := queryplan.Compile(parse(t, `{"field":"service.name","op":"eq","value":"checkout"}`), queryplan.TraceSpanCatalog)
	for name, q := range map[string]TraceSearchQuery{
		"25h":        {Range: TimeRange{From: base, To: base.Add(25 * time.Hour)}, SpanFilter: a, TraceFilter: b, Limit: 1},
		"limit":      {Range: rng, SpanFilter: a, TraceFilter: b, Limit: 1001},
		"unresolved": {Range: rng, SpanFilter: unresolved, TraceFilter: b, Limit: 1},
		"after":      {Range: rng, SpanFilter: a, TraceFilter: b, Limit: 1, After: &TracePosition{Start: base, TraceID: "' OR 1=1 --"}},
	} {
		if _, _, err := s.SearchTraces(ctx, viewerA, q, now); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
}

// span이 요약 한도(10,000)를 넘는 trace: span_limit_reached로 알리고, 잘린 목록 때문에 부모 누락을 잘못 세지 않는다(D02 §22).
func TestSearchTracesSpanLimit(t *testing.T) {
	s := openQuery(t)
	ctx := context.Background()
	tenant, viewer := newTenant(t)
	now := time.Now().UTC().Truncate(time.Millisecond)
	base := now.Add(-10 * time.Minute)
	const big = "6666aaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	rows := []searchSpan{{tenant: tenant, service: svcA, traceID: big, spanID: "00000000000f0000", name: "root", start: base,
		dur: time.Second, version: 1, expires: now.Add(time.Hour)}}
	for i := 1; i <= TraceSummarySpanLimit; i++ {
		rows = append(rows, searchSpan{tenant: tenant, service: svcA, traceID: big, spanID: fmt.Sprintf("%016x", i), parentID: "00000000000f0000",
			name: "child", start: base.Add(time.Duration(i) * time.Microsecond), dur: time.Millisecond, version: 1, expires: now.Add(time.Hour)})
	}
	insertSearchSpans(t, rows...)
	a, err := queryplan.CompileWith(nil, queryplan.TraceSpanCatalog, queryplan.Options{ParamPrefix: "s"})
	if err != nil {
		t.Fatal(err)
	}
	got, _, err := s.SearchTraces(ctx, viewer, TraceSearchQuery{Range: TimeRange{From: base.Add(-time.Minute), To: now}, SpanFilter: a, TraceFilter: a, Limit: 10}, now)
	if err != nil || len(got) != 1 {
		t.Fatalf("search = %+v %v", got, err)
	}
	if x := got[0]; x.SpanCount != TraceSummarySpanLimit+1 || !x.SpanLimitHit || x.MissingParent || x.Roots != 1 {
		t.Errorf("big trace = %+v", x)
	}
}
