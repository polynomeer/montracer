package main

import (
	"math"
	"testing"
	"time"

	"go.opentelemetry.io/collector/pdata/plog"
	"go.opentelemetry.io/collector/pdata/ptrace"
)

var anchor = time.Date(2026, 10, 6, 10, 0, 0, 0, time.UTC)

// 같은 anchor면 같은 trace·span ID와 같은 JSON이다 → 재실행이 dedup된다.
func TestScenarioIsDeterministic(t *testing.T) {
	dt := demoTenants[0]
	a, _ := (&ptrace.JSONMarshaler{}).MarshalTraces(buildTraces(dt, anchor, 0, 10))
	b, _ := (&ptrace.JSONMarshaler{}).MarshalTraces(buildTraces(dt, anchor, 0, 10))
	if string(a) != string(b) {
		t.Error("traces differ between runs")
	}
	la, _ := (&plog.JSONMarshaler{}).MarshalLogs(buildLogs(dt, anchor, 0, 10))
	lb, _ := (&plog.JSONMarshaler{}).MarshalLogs(buildLogs(dt, anchor, 0, 10))
	if string(la) != string(lb) {
		t.Error("logs differ between runs")
	}
	if traceHex(dt.ID, anchor, 0) == traceHex(dt.ID, anchor.Add(time.Hour), 0) || traceHex(dt.ID, anchor, 0) == traceHex(demoTenants[1].ID, anchor, 0) {
		t.Error("trace IDs must differ across anchors and tenants")
	}
}

// D06 §04 oracle: 요청 1,000 중 느림 100, 오류 20(느린 요청의 부분집합), metric 합 980/20.
func TestCheckoutOracle(t *testing.T) {
	dt := demoTenants[0]
	slowN, errN := 0, 0
	for i := 0; i < dt.Requests; i++ {
		if slow(i) {
			slowN++
		}
		if failed(i) {
			errN++
			if !slow(i) {
				t.Fatalf("error request %d is not slow", i)
			}
		}
	}
	if slowN != 100 || errN != 20 {
		t.Fatalf("slow=%d errors=%d", slowN, errN)
	}
	td := buildTraces(dt, anchor, 0, dt.Requests)
	if td.SpanCount() != 3000 {
		t.Errorf("spans = %d", td.SpanCount())
	}
	md := buildMetrics(dt, anchor)
	sums := map[int64]int64{}
	dps := md.ResourceMetrics().At(0).ScopeMetrics().At(0).Metrics().At(0).Sum().DataPoints()
	for i := 0; i < dps.Len(); i++ {
		st, _ := dps.At(i).Attributes().Get(statusAttr)
		sums[st.Int()] += dps.At(i).IntValue()
		if dps.At(i).Timestamp().AsTime().Sub(dps.At(i).StartTimestamp().AsTime()) != time.Minute {
			t.Fatal("metric window is not one minute")
		}
	}
	if sums[200] != 980 || sums[500] != 20 {
		t.Errorf("metric sums = %v", sums)
	}
	// duration histogram: 같은 요청 수, 느린 요청 100개는 2.5초 bucket(≤2.5), 나머지는 0.25초 bucket
	hm := md.ResourceMetrics().At(0).ScopeMetrics().At(0).Metrics().At(1)
	if hm.Name() != durationMetric || hm.Unit() != "s" {
		t.Fatalf("histogram metric = %s %s", hm.Name(), hm.Unit())
	}
	var hcount, slowCount uint64
	var hsum float64
	hps := hm.Histogram().DataPoints()
	for i := 0; i < hps.Len(); i++ {
		hp := hps.At(i)
		hcount += hp.Count()
		hsum += hp.Sum()
		slowCount += hp.BucketCounts().At(bucketIndex(2.5))
		var total uint64
		for j := 0; j < hp.BucketCounts().Len(); j++ {
			total += hp.BucketCounts().At(j)
		}
		if total != hp.Count() || hp.BucketCounts().Len() != hp.ExplicitBounds().Len()+1 {
			t.Fatalf("histogram point %d inconsistent", i)
		}
		if r, _ := hp.Attributes().Get(routeAttr); r.Str() != demoRoute {
			t.Errorf("route = %q", r.Str())
		}
	}
	if hcount != 1000 || slowCount != 100 || math.Abs(hsum-(100*2.5+900*0.12)) > 1e-6 {
		t.Errorf("histogram count=%d slow=%d sum=%v", hcount, slowCount, hsum)
	}
	// 시나리오(span·log·metric window)가 anchor부터 5분 안이다 → anchor가 10분 전이면 모두 과거다
	var last time.Time
	spans := td.ResourceSpans()
	for r := 0; r < spans.Len(); r++ {
		ss := spans.At(r).ScopeSpans().At(0).Spans()
		for i := 0; i < ss.Len(); i++ {
			if e := ss.At(i).EndTimestamp().AsTime(); e.After(last) {
				last = e
			}
		}
	}
	if last.Sub(anchor) > 5*time.Minute {
		t.Errorf("scenario spans %s, must fit in 5 minutes", last.Sub(anchor))
	}
	if dps.Len() != 10 {
		t.Errorf("metric points = %d, want 5 minutes × 2 statuses", dps.Len())
	}
}

func TestRequireLocal(t *testing.T) {
	for raw, ok := range map[string]bool{
		"http://localhost:18318":                    true,
		"http://127.0.0.1:18080":                    true,
		"http://[::1]:18081":                        true,
		"postgres://u:p@localhost:15432/montracer":  true,
		"https://ingest.example.com":                false,
		"postgres://u:p@db.internal:5432/montracer": false,
		"http://10.0.0.5:18318":                     false,
		"not a url":                                 false,
	} {
		if err := requireLocal("X", raw); (err == nil) != ok {
			t.Errorf("%s: err=%v, want ok=%v", raw, err, ok)
		}
	}
}

func TestNewAnchorIsPastAndAligned(t *testing.T) {
	for _, now := range []time.Time{
		time.Date(2026, 10, 6, 10, 0, 0, 0, time.UTC), time.Date(2026, 10, 6, 10, 4, 59, 0, time.UTC), time.Date(2026, 10, 6, 10, 7, 30, 0, time.UTC),
	} {
		a := newAnchor(now)
		if a.Truncate(5*time.Minute) != a || now.Sub(a) < 10*time.Minute || now.Sub(a) >= 15*time.Minute {
			t.Errorf("now %s → anchor %s", now.Format("15:04:05"), a.Format("15:04:05"))
		}
	}
}
