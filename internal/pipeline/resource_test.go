package pipeline

import (
	"strings"
	"testing"

	"go.opentelemetry.io/collector/pdata/pcommon"
	"go.opentelemetry.io/collector/pdata/plog"
	"go.opentelemetry.io/collector/pdata/ptrace"

	"github.com/polynomeer/montracer/internal/telemetry/envelope"
	"github.com/polynomeer/montracer/internal/telemetry/redact"
)

// resource 속성 열(ADR 0075 §2): 없음·문자열 아님·255 byte 초과·redaction 표시는 ”와 사유. 자르지 않는다.
func TestResourceColumns(t *testing.T) {
	m := pcommon.NewMap()
	m.PutStr(ResourceServiceVersion, "1.4.2")
	m.PutInt(ResourceServiceInstanceID, 7)
	m.PutStr(ResourceK8sNamespace, strings.Repeat("n", MaxResourceValueBytes+1))
	m.PutStr(ResourceK8sPod, "checkout-[REDACTED:phone]")
	cols, drops := resourceColumns(m)
	if cols != (ResourceColumns{ServiceVersion: "1.4.2"}) {
		t.Fatalf("cols = %+v", cols)
	}
	want := []ResourceDrop{
		{ResourceServiceInstanceID, "not_string"},
		{ResourceK8sNamespace, "too_long"},
		{ResourceK8sPod, "redacted"},
	}
	if len(drops) != len(want) {
		t.Fatalf("drops = %v", drops)
	}
	for i := range want {
		if drops[i] != want[i] {
			t.Errorf("drop %d = %v, want %v", i, drops[i], want[i])
		}
	}
	// 정확히 255 byte는 넣는다. 빈 문자열은 없음이다
	m = pcommon.NewMap()
	m.PutStr(ResourceK8sPod, strings.Repeat("p", MaxResourceValueBytes))
	m.PutStr(ResourceServiceVersion, "")
	cols, drops = resourceColumns(m)
	if len(cols.K8sPod) != MaxResourceValueBytes || cols.ServiceVersion != "" || len(drops) != 3 || drops[0] != (ResourceDrop{ResourceServiceVersion, "missing"}) {
		t.Fatalf("boundary: %+v %v", cols, drops)
	}
}

// 실제 형태의 버전·pod·instance ID는 ingress redaction(기본 정책) 뒤에도 그대로다(ADR 0075 §2).
// 4부분 버전(공인 IPv4처럼 보임)의 축약은 알려진 한계라 여기서 기대하지 않는다(ADR 0019 §5).
func TestResourceValuesSurviveRedaction(t *testing.T) {
	values := map[string][]string{
		ResourceServiceVersion:    {"1.4.2", "v2.10.0-rc.1", "2026.11.02-3f9c2ab", "3f9c2ab1d4e5", "1.2.3+build.45"},
		ResourceK8sPod:            {"checkout-7d9f8b6c4-x2k9p", "redis-0", "payments-api-6c8b9d7f5d-qwz7m"},
		ResourceK8sNamespace:      {"shop", "kube-system"},
		ResourceServiceInstanceID: {"8f14e45f-ceea-467f-a0e6-3a1b2c3d4e5f", "checkout-7d9f8b6c4-x2k9p/app"},
	}
	r := redact.New(redact.DefaultPolicy)
	for key, vs := range values {
		for _, v := range vs {
			td := ptrace.NewTraces()
			rs := td.ResourceSpans().AppendEmpty()
			rs.Resource().Attributes().PutStr("service.name", "checkout")
			rs.Resource().Attributes().PutStr(key, v)
			s := rs.ScopeSpans().AppendEmpty().Spans().AppendEmpty()
			s.SetTraceID(pcommon.TraceID{1})
			s.SetSpanID(pcommon.SpanID{1})
			r.Traces(td)
			got, _ := rs.Resource().Attributes().Get(key)
			cols, _ := resourceColumns(rs.Resource().Attributes())
			stored := map[string]string{ResourceServiceVersion: cols.ServiceVersion, ResourceK8sPod: cols.K8sPod,
				ResourceK8sNamespace: cols.K8sNamespace, ResourceServiceInstanceID: cols.ServiceInstanceID}[key]
			if got.Str() != v || stored != v {
				t.Errorf("%s %q became %q (stored %q)", key, v, got.Str(), stored)
			}
		}
	}
}

// span·log 행에 열 값이 실리고, batch는 처음 들어간 행의 버림 사유를 센다.
func TestResourceColumnsInRows(t *testing.T) {
	td := span(pcommon.TraceID{0xcd, 1}, pcommon.SpanID{1}, "GET /cart")
	res := td.ResourceSpans().At(0).Resource().Attributes()
	res.PutStr(ResourceServiceVersion, "1.4.2")
	res.PutStr(ResourceK8sNamespace, "shop")
	res.PutStr(ResourceK8sPod, "checkout-7d9f8b6c4-x2k9p")
	recs := spanRecords(t, td, envMeta(tenantA, now))
	// 같은 span을 한 번 더(재전송): 버림 사유는 한 번만 센다
	recs = append(recs, recs[0])
	bs := builder.Build(toMsgs(0, 1, recs...))
	if len(bs) != 1 || len(bs[0].Spans) != 1 {
		t.Fatalf("batches = %+v", bs)
	}
	if got := bs[0].Spans[0].Resource; got != (ResourceColumns{ServiceVersion: "1.4.2", K8sNamespace: "shop", K8sPod: "checkout-7d9f8b6c4-x2k9p"}) {
		t.Errorf("span resource = %+v", got)
	}
	if n := bs[0].ResourceDrops[ResourceDrop{ResourceServiceInstanceID, "missing"}]; n != 1 || len(bs[0].ResourceDrops) != 1 {
		t.Errorf("drops = %v", bs[0].ResourceDrops)
	}

	ld := plog.NewLogs()
	rl := ld.ResourceLogs().AppendEmpty()
	rl.Resource().Attributes().PutStr("service.name", "checkout")
	rl.Resource().Attributes().PutStr(ResourceServiceVersion, "2.0.0")
	rl.Resource().Attributes().PutStr(ResourceServiceInstanceID, "i-1")
	lr := rl.ScopeLogs().AppendEmpty().LogRecords().AppendEmpty()
	lr.SetTimestamp(pcommon.NewTimestampFromTime(now))
	lr.Body().SetStr("hello")
	lres, err := envelope.Logs(ld, envMeta(tenantA, now), nil)
	if err != nil {
		t.Fatal(err)
	}
	bs = builder.Build(toMsgs(0, 1, lres.Records...))
	if len(bs) != 1 || len(bs[0].Logs) != 1 || bs[0].Logs[0].Resource != (ResourceColumns{ServiceVersion: "2.0.0", ServiceInstanceID: "i-1"}) {
		t.Fatalf("log rows = %+v", bs)
	}
	if bs[0].ResourceDrops[ResourceDrop{ResourceK8sPod, "missing"}] != 1 {
		t.Errorf("log drops = %v", bs[0].ResourceDrops)
	}
}
