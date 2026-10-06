package query

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/polynomeer/montracer/internal/apicursor"
	"github.com/polynomeer/montracer/internal/authz"
	"github.com/polynomeer/montracer/internal/telemetrystore"
)

// fakeLogs는 시간 내림차순 log에서 keyset·snapshot·limit을 흉내 낸다(필터 SQL은 telemetrystore 통합 시험이 본다).
type fakeLogs struct {
	mu      sync.Mutex
	rows    []telemetrystore.LogRecord
	queries []telemetrystore.LogQuery
	err     error
	block   chan struct{}
}

func (f *fakeLogs) SearchLogs(_ context.Context, p authz.Principal, q telemetrystore.LogQuery, _ time.Time) ([]telemetrystore.LogRecord, bool, error) {
	if f.block != nil {
		<-f.block
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.queries = append(f.queries, q)
	if f.err != nil {
		return nil, false, f.err
	}
	if p.EnvironmentRestricted() {
		return nil, false, telemetrystore.ErrEnvironmentScoped
	}
	var out []telemetrystore.LogRecord
	for _, r := range f.rows {
		// keyset: (event_time, event_id) < After 인 행만 남긴다
		if q.After != nil && !r.EventTime.Before(q.After.EventTime) && (!r.EventTime.Equal(q.After.EventTime) || r.EventID >= q.After.EventID) {
			continue
		}
		out = append(out, r)
	}
	more := len(out) > q.Limit
	if more {
		out = out[:q.Limit]
	}
	return out, more, nil
}

func logRows(n int) []telemetrystore.LogRecord {
	var out []telemetrystore.LogRecord
	for i := 0; i < n; i++ {
		out = append(out, telemetrystore.LogRecord{
			EventID: fmt.Sprintf("uid:%03d", n-i), ServiceID: "aaaaaaaa-0000-4000-8000-000000000001",
			EventTime: now.Add(-time.Duration(i+1) * time.Minute), Severity: 17, Body: "card declined",
			TraceID: traceID, Attributes: map[string]string{"http.route": "/checkout"},
		})
	}
	return out
}

func searchHandler(t *testing.T, k *keys, logs LogStore, clock *time.Time) *Handler {
	t.Helper()
	signer, err := apicursor.NewSigner([]byte("0123456789abcdef0123456789abcdef"), 15*time.Minute, func() time.Time { return *clock })
	if err != nil {
		t.Fatal(err)
	}
	h, err := NewHandler(Config{Authenticate: k.authenticate, Store: &fakeStore{}, Logs: logs, Cursor: signer,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)), Now: func() time.Time { return *clock }})
	if err != nil {
		t.Fatal(err)
	}
	return h
}

func post(h http.Handler, path, token, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, path, bytes.NewBufferString(body))
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

type searchBody struct {
	Data       []map[string]any `json:"data"`
	NextCursor *string          `json:"next_cursor"`
	Meta       map[string]any   `json:"meta"`
}

func decodeSearch(t *testing.T, rec *httptest.ResponseRecorder) searchBody {
	t.Helper()
	var b searchBody
	if err := json.Unmarshal(rec.Body.Bytes(), &b); err != nil {
		t.Fatalf("decode %d %s: %v", rec.Code, rec.Body, err)
	}
	return b
}

func TestSearchLogsPagesWithCursor(t *testing.T) {
	k := newKeys(t)
	tok := k.issue(t, authz.KindAPIKey, []authz.Action{authz.TelemetryRead}, nil)
	clock := now
	logs := &fakeLogs{rows: logRows(5)}
	h := searchHandler(t, k, logs, &clock)

	req := map[string]any{
		"signal": "logs", "range": map[string]string{"from": "2026-10-05T11:00:00Z", "to": "2026-10-05T12:00:00Z"},
		"filter": json.RawMessage(`{"op":"and","args":[{"field":"severity_number","op":"gte","value":17},{"field":"body","op":"contains","value":"declined"}]}`),
		"limit":  2,
	}
	var seen []string
	for page := 0; page < 5; page++ {
		body, _ := json.Marshal(req)
		rec := post(h, "/api/v1/query", tok, string(body))
		if rec.Code != http.StatusOK {
			t.Fatalf("page %d: %d %s", page, rec.Code, rec.Body)
		}
		b := decodeSearch(t, rec)
		for _, d := range b.Data {
			seen = append(seen, d["event_id"].(string))
		}
		if b.NextCursor == nil {
			break
		}
		req["cursor"] = *b.NextCursor
		clock = clock.Add(time.Minute) // 다음 page 사이에 시간이 지나도 snapshot은 첫 page 시각
	}
	if len(seen) != 5 || seen[0] != "uid:005" || seen[4] != "uid:001" {
		t.Fatalf("seen = %v", seen)
	}
	// 첫 page의 수신 snapshot이 이후 page에도 그대로 쓰인다(D02 §15)
	first, last := logs.queries[0], logs.queries[len(logs.queries)-1]
	if !first.ReceivedBefore.Equal(now) || !last.ReceivedBefore.Equal(now) || last.After == nil {
		t.Errorf("snapshot first=%s last=%s after=%v", first.ReceivedBefore, last.ReceivedBefore, last.After)
	}
	// filter는 컴파일된 SQL·parameter로만 저장소에 간다
	if !strings.Contains(first.Filter.SQL, "positionCaseInsensitiveUTF8(body, {f1:String})") || first.Filter.Params["f1"] != "declined" {
		t.Errorf("filter = %+v", first.Filter)
	}
}

func TestSearchLogsResponseShape(t *testing.T) {
	k := newKeys(t)
	tok := k.issue(t, authz.KindAPIKey, []authz.Action{authz.TelemetryRead}, nil)
	clock := now
	rows := logRows(1)
	rows[0].SpanID = "" // 없는 span ID
	h := searchHandler(t, k, &fakeLogs{rows: rows}, &clock)
	rec := post(h, "/api/v1/query/logs", tok, `{"projection":["time","span_id","body"]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	b := decodeSearch(t, rec)
	if len(b.Data) != 1 || len(b.Data[0]) != 3 || b.Data[0]["span_id"] != nil || b.Data[0]["body"] != "card declined" {
		t.Errorf("row = %v", b.Data)
	}
	if !strings.Contains(rec.Body.String(), `"span_id":null`) || !strings.Contains(rec.Body.String(), `"next_cursor":null`) ||
		b.Meta["partial"] != false || b.Meta["coverage"] != nil {
		t.Errorf("body = %s", rec.Body)
	}
	if rec.Header().Get("Cache-Control") != "no-store" {
		t.Error("search response cacheable")
	}
}

func TestSearchRejects(t *testing.T) {
	k := newKeys(t)
	tok := k.issue(t, authz.KindAPIKey, []authz.Action{authz.TelemetryRead}, nil)
	clock := now
	logs := &fakeLogs{}
	h := searchHandler(t, k, logs, &clock)
	for name, c := range map[string]struct {
		path, body string
		status     int
		field      string
	}{
		"unknown field in body": {"/api/v1/query", `{"signal":"logs","sql":"select 1"}`, 400, "body"},
		"missing signal":        {"/api/v1/query", `{}`, 400, "signal"},
		"traces not yet":        {"/api/v1/query", `{"signal":"traces"}`, 422, "signal"},
		"metrics elsewhere":     {"/api/v1/query", `{"signal":"metrics"}`, 422, "signal"},
		"path signal mismatch":  {"/api/v1/query/logs", `{"signal":"traces"}`, 400, "signal"},
		"bad filter field":      {"/api/v1/query/logs", `{"filter":{"field":"tenant_id","op":"eq","value":"x"}}`, 400, "filter.field"},
		"bad order":             {"/api/v1/query/logs", `{"order":[{"field":"severity_number","direction":"asc"}]}`, 422, "order"},
		"unknown projection":    {"/api/v1/query/logs", `{"projection":["payload"]}`, 400, "projection"},
		"limit too big":         {"/api/v1/query/logs", `{"limit":1001}`, 400, "limit"},
		"bad cursor":            {"/api/v1/query/logs", `{"cursor":"c1.x.y"}`, 400, "cursor"},
	} {
		rec := post(h, c.path, tok, c.body)
		var e errorBody
		_ = json.Unmarshal(rec.Body.Bytes(), &e)
		fv, _ := e.Error.Details["field_violations"].([]any)
		var got any
		if len(fv) > 0 {
			got = fv[0].(map[string]any)["field"]
		}
		if rec.Code != c.status || got != c.field {
			t.Errorf("%s: %d %s", name, rec.Code, rec.Body)
		}
	}
	if len(logs.queries) != 0 {
		t.Errorf("invalid requests reached the store: %d", len(logs.queries))
	}
}

// cursor는 다른 query(filter·범위)나 다른 principal에 쓸 수 없다.
func TestSearchCursorBinding(t *testing.T) {
	k := newKeys(t)
	a := k.issue(t, authz.KindAPIKey, []authz.Action{authz.TelemetryRead}, nil)
	b := k.issue(t, authz.KindAPIKey, []authz.Action{authz.TelemetryRead}, nil)
	clock := now
	h := searchHandler(t, k, &fakeLogs{rows: logRows(3)}, &clock)
	first := decodeSearch(t, post(h, "/api/v1/query/logs", a, `{"limit":1}`))
	if first.NextCursor == nil {
		t.Fatal("no cursor")
	}
	cur := *first.NextCursor
	for name, c := range map[string]struct{ token, body string }{
		"other key":    {b, `{"limit":1,"cursor":"` + cur + `"}`},
		"other filter": {a, `{"limit":1,"cursor":"` + cur + `","filter":{"field":"body","op":"contains","value":"x"}}`},
	} {
		if rec := post(h, "/api/v1/query/logs", c.token, c.body); rec.Code != http.StatusBadRequest {
			t.Errorf("%s: %d %s", name, rec.Code, rec.Body)
		}
	}
	if rec := post(h, "/api/v1/query/logs", a, `{"limit":2,"cursor":"`+cur+`"}`); rec.Code != http.StatusOK {
		t.Errorf("same query, other page size: %d %s", rec.Code, rec.Body)
	}
}

// environment로 제한된 key는 log를 검색할 수 없다(범위를 증명할 수 없다) — 403.
func TestSearchEnvironmentScopedKey(t *testing.T) {
	k := newKeys(t)
	tok := k.issue(t, authz.KindAPIKey, []authz.Action{authz.TelemetryRead}, []string{"prod"})
	clock := now
	h := searchHandler(t, k, &fakeLogs{rows: logRows(1)}, &clock)
	if rec := post(h, "/api/v1/query/logs", tok, `{}`); rec.Code != http.StatusForbidden || strings.Contains(rec.Body.String(), "card declined") {
		t.Errorf("%d %s", rec.Code, rec.Body)
	}
}

// tenant별 동시 실행 상한: 5개 실행 + 20개 대기를 넘으면 429 + Retry-After.
func TestSearchConcurrencyGate(t *testing.T) {
	k := newKeys(t)
	tok := k.issue(t, authz.KindAPIKey, []authz.Action{authz.TelemetryRead}, nil)
	clock := now
	logs := &fakeLogs{block: make(chan struct{})}
	h := searchHandler(t, k, logs, &clock)
	var wg sync.WaitGroup
	for i := 0; i < 25; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); post(h, "/api/v1/query/logs", tok, `{}`) }()
	}
	deadline := time.Now().Add(2 * time.Second)
	for {
		s := h.gate.slot(tenantA.String())
		if len(s.running) == 5 && s.waiting.Load() == 20 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("running=%d waiting=%d", len(s.running), s.waiting.Load())
		}
		time.Sleep(5 * time.Millisecond)
	}
	rec := post(h, "/api/v1/query/logs", tok, `{}`)
	if rec.Code != http.StatusTooManyRequests || rec.Header().Get("Retry-After") != "1" {
		t.Errorf("26th: %d %v", rec.Code, rec.Header())
	}
	close(logs.block)
	wg.Wait()
}

// range를 생략해도 다음 page cursor가 쓰인다: 기본 15분은 첫 page snapshot 기준이다(리뷰에서 발견, P1 회귀).
func TestSearchDefaultRangeCursorSurvivesClock(t *testing.T) {
	k := newKeys(t)
	tok := k.issue(t, authz.KindAPIKey, []authz.Action{authz.TelemetryRead}, nil)
	clock := now
	logs := &fakeLogs{rows: logRows(3)}
	h := searchHandler(t, k, logs, &clock)
	first := decodeSearch(t, post(h, "/api/v1/query/logs", tok, `{"limit":1}`))
	if first.NextCursor == nil {
		t.Fatal("no cursor")
	}
	clock = clock.Add(2 * time.Second)
	rec := post(h, "/api/v1/query/logs", tok, `{"limit":1,"cursor":"`+*first.NextCursor+`"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("second page after clock advance: %d %s", rec.Code, rec.Body)
	}
	// 두 번째 page의 범위는 첫 page와 같은 [snapshot−15분, snapshot)이다
	q := logs.queries[len(logs.queries)-1]
	if !q.Range.To.Equal(now) || !q.Range.From.Equal(now.Add(-15*time.Minute)) {
		t.Errorf("second page range = %s..%s", q.Range.From, q.Range.To)
	}
}

// interactive 누적 10,000행에서 멈추고 경고를 남긴다(D02 §13).
func TestSearchInteractiveRowLimit(t *testing.T) {
	k := newKeys(t)
	tok := k.issue(t, authz.KindAPIKey, []authz.Action{authz.TelemetryRead}, nil)
	clock := now
	rows := make([]telemetrystore.LogRecord, 0, MaxInteractiveRows+5)
	for i := 0; i < MaxInteractiveRows+5; i++ {
		rows = append(rows, telemetrystore.LogRecord{EventID: fmt.Sprintf("uid:%05d", MaxInteractiveRows+5-i), EventTime: now.Add(-time.Duration(i+1) * time.Millisecond)})
	}
	h := searchHandler(t, k, &fakeLogs{rows: rows}, &clock)
	req := map[string]any{"limit": 1000, "range": map[string]string{"from": "2026-10-05T11:00:00Z", "to": "2026-10-05T12:00:00Z"}}
	total := 0
	var last searchBody
	for page := 0; page < 20; page++ {
		body, _ := json.Marshal(req)
		last = decodeSearch(t, post(h, "/api/v1/query/logs", tok, string(body)))
		total += len(last.Data)
		if last.NextCursor == nil {
			break
		}
		req["cursor"] = *last.NextCursor
	}
	warnings, _ := last.Meta["warnings"].([]any)
	if total != MaxInteractiveRows || len(warnings) != 1 || warnings[0] != WarningInteractiveRowLimit {
		t.Errorf("total=%d warnings=%v", total, warnings)
	}
}

// 대기는 조회 시간 상한 안에서만 한다(무기한 대기 없음).
func TestGateWaitIsBounded(t *testing.T) {
	g := newTenantGate(1, 5)
	release, err := g.acquire(context.Background(), "t")
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, err := g.acquire(ctx, "t"); err == nil {
		t.Error("waiting acquire did not stop at the deadline")
	}
	if n := g.slot("t").waiting.Load(); n != 0 {
		t.Errorf("waiting counter leaked: %d", n)
	}
}
