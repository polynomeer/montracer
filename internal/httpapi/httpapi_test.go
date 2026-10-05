package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/polynomeer/montracer/internal/apierr"
	"github.com/polynomeer/montracer/internal/authz"
)

type logLine struct {
	Level     string `json:"level"`
	Msg       string `json:"msg"`
	RequestID string `json:"request_id"`
	Code      string `json:"code"`
	Route     string `json:"route"`
	Error     string `json:"error"`
	Stack     string `json:"stack"`
}

// serve는 RequestID → mux(route pattern) → Boundary 순서로 실제 조립과 같게 요청을 처리한다.
func serve(t *testing.T, req *http.Request, h HandlerFunc) (*httptest.ResponseRecorder, []logLine) {
	t.Helper()
	var buf bytes.Buffer
	b := Boundary{Logger: slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))}
	mux := http.NewServeMux()
	mux.Handle("/api/v1/things/{id}", b.Handle(h))
	rec := httptest.NewRecorder()
	RequestID(mux).ServeHTTP(rec, req)

	var lines []logLine
	for _, raw := range bytes.Split(bytes.TrimSpace(buf.Bytes()), []byte("\n")) {
		if len(raw) == 0 {
			continue
		}
		var l logLine
		if err := json.Unmarshal(raw, &l); err != nil {
			t.Fatalf("bad log line %q: %v", raw, err)
		}
		lines = append(lines, l)
	}
	return rec, lines
}

func newRequest(method, target string, body io.Reader) *http.Request {
	return httptest.NewRequestWithContext(context.Background(), method, target, body)
}

var requestIDPattern = regexp.MustCompile(`^req_[0-9a-f]{32}$`)

func TestSuccessPassesThrough(t *testing.T) {
	req := newRequest(http.MethodGet, "/api/v1/things/1", nil)
	req.Header.Set(RequestIDHeader, "client-chosen-id")
	rec, logs := serve(t, req, func(w http.ResponseWriter, r *http.Request) error {
		if !requestIDPattern.MatchString(RequestIDFrom(r.Context())) {
			t.Errorf("request id in context = %q", RequestIDFrom(r.Context()))
		}
		w.WriteHeader(http.StatusNoContent)
		return nil
	})
	if rec.Code != http.StatusNoContent || len(logs) != 0 {
		t.Fatalf("status=%d logs=%v", rec.Code, logs)
	}
	// client가 보낸 request ID는 쓰지 않는다.
	if id := rec.Header().Get(RequestIDHeader); !requestIDPattern.MatchString(id) {
		t.Errorf("response request id = %q", id)
	}
}

func TestInternalErrorLoggedOnceNotLeaked(t *testing.T) {
	req := newRequest(http.MethodPost, "/api/v1/things/42?q=secret-search", strings.NewReader(`{"password":"hunter2"}`))
	rec, logs := serve(t, req, func(http.ResponseWriter, *http.Request) error {
		return errors.New("pq: duplicate key at 10.0.0.9")
	})
	if rec.Code != 500 {
		t.Fatalf("status = %d", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "10.0.0.9") {
		t.Errorf("response leaks cause: %s", rec.Body.String())
	}
	if len(logs) != 1 || logs[0].Level != "ERROR" || logs[0].Code != "INTERNAL" {
		t.Fatalf("logs = %+v", logs)
	}
	l := logs[0]
	if !strings.Contains(l.Error, "10.0.0.9") {
		t.Errorf("cause missing from server log: %+v", l)
	}
	if l.Route != "/api/v1/things/{id}" || l.RequestID != rec.Header().Get(RequestIDHeader) {
		t.Errorf("route/request id = %q/%q", l.Route, l.RequestID)
	}
	var body struct {
		Error struct {
			RequestID string `json:"request_id"`
		} `json:"error"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	if body.Error.RequestID != l.RequestID {
		t.Errorf("envelope request id %q != log %q", body.Error.RequestID, l.RequestID)
	}
}

func TestLogNeverContainsRequestData(t *testing.T) {
	req := newRequest(http.MethodPost, "/api/v1/things/user@example.com?q=secret-search", strings.NewReader(`{"password":"hunter2"}`))
	req.Header.Set("Authorization", "Bearer mta_secret")
	var buf bytes.Buffer
	b := Boundary{Logger: slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))}
	mux := http.NewServeMux()
	mux.Handle("/api/v1/things/{id}", b.Handle(func(http.ResponseWriter, *http.Request) error { return errors.New("boom") }))
	RequestID(mux).ServeHTTP(httptest.NewRecorder(), req)
	for _, frag := range []string{"user@example.com", "secret-search", "hunter2", "mta_secret"} {
		if strings.Contains(buf.String(), frag) {
			t.Errorf("log contains request data %q: %s", frag, buf.String())
		}
	}
}

func TestLogLevelPolicy(t *testing.T) {
	cases := []struct {
		err   error
		level string
	}{
		{authz.ErrUnauthenticated, "INFO"},
		{authz.ErrNotFound, "DEBUG"},
		{apierr.NewInvalidArgument("x"), "DEBUG"},
		{apierr.New(apierr.Unavailable, "x"), "ERROR"},
	}
	for _, tc := range cases {
		_, logs := serve(t, newRequest(http.MethodGet, "/api/v1/things/1", nil),
			func(http.ResponseWriter, *http.Request) error { return tc.err })
		if len(logs) != 1 || logs[0].Level != tc.level {
			t.Errorf("%v: logs = %+v, want one %s", tc.err, logs, tc.level)
		}
		if tc.level != "ERROR" && logs[0].Error != "" {
			t.Errorf("%v: non-5xx log must not carry error text", tc.err)
		}
	}
}

func TestRetryableDependsOnRequestSafety(t *testing.T) {
	unavailable := func(http.ResponseWriter, *http.Request) error { return apierr.New(apierr.Unavailable, "x") }
	retryable := func(rec *httptest.ResponseRecorder) bool {
		var b struct {
			Error struct {
				Retryable bool `json:"retryable"`
			} `json:"error"`
		}
		_ = json.Unmarshal(rec.Body.Bytes(), &b)
		return b.Error.Retryable
	}
	get, _ := serve(t, newRequest(http.MethodGet, "/api/v1/things/1", nil), unavailable)
	post, _ := serve(t, newRequest(http.MethodPost, "/api/v1/things/1", nil), unavailable)
	idem := newRequest(http.MethodPost, "/api/v1/things/1", nil)
	idem.Header.Set("Idempotency-Key", "k1")
	postIdem, _ := serve(t, idem, unavailable)
	if !retryable(get) || retryable(post) || !retryable(postIdem) {
		t.Errorf("GET=%v POST=%v POST+Idempotency-Key=%v", retryable(get), retryable(post), retryable(postIdem))
	}
}

func TestPanicRecovered(t *testing.T) {
	rec, logs := serve(t, newRequest(http.MethodGet, "/api/v1/things/1", nil),
		func(http.ResponseWriter, *http.Request) error { panic("nil map at tenant 2222") })
	if rec.Code != 500 || strings.Contains(rec.Body.String(), "2222") {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if len(logs) != 1 || logs[0].Msg != "panic recovered" || logs[0].Stack == "" {
		t.Fatalf("logs = %+v", logs)
	}
}

func TestPanicAfterResponseStarted(t *testing.T) {
	rec, logs := serve(t, newRequest(http.MethodGet, "/api/v1/things/1", nil),
		func(w http.ResponseWriter, _ *http.Request) error {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"data":[`))
			panic("mid-stream")
		})
	if rec.Code != 200 || strings.Contains(rec.Body.String(), "INTERNAL") {
		t.Fatalf("must not append error envelope to started response: %d %s", rec.Code, rec.Body.String())
	}
	if len(logs) != 1 || logs[0].Level != "ERROR" {
		t.Fatalf("logs = %+v", logs)
	}
}

func TestErrorAfterResponseStarted(t *testing.T) {
	rec, logs := serve(t, newRequest(http.MethodGet, "/api/v1/things/1", nil),
		func(w http.ResponseWriter, _ *http.Request) error {
			w.WriteHeader(http.StatusOK)
			return errors.New("stream broke")
		})
	if rec.Code != 200 || rec.Body.Len() != 0 {
		t.Fatalf("status=%d body=%q", rec.Code, rec.Body.String())
	}
	if len(logs) != 1 || logs[0].Msg != "error after response started" {
		t.Fatalf("logs = %+v", logs)
	}
}

func TestAbortHandlerPropagates(t *testing.T) {
	defer func() {
		if p := recover(); p != http.ErrAbortHandler { //nolint:errorlint // sentinel 값 비교
			t.Fatalf("recover = %v, want http.ErrAbortHandler", p)
		}
	}()
	serve(t, newRequest(http.MethodGet, "/api/v1/things/1", nil),
		func(http.ResponseWriter, *http.Request) error { panic(http.ErrAbortHandler) })
}

func TestClientDisconnectNotAnError(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	req := newRequest(http.MethodGet, "/api/v1/things/1", nil).WithContext(ctx)
	rec, logs := serve(t, req, func(_ http.ResponseWriter, r *http.Request) error { return r.Context().Err() })
	if rec.Body.Len() != 0 {
		t.Errorf("wrote response to gone client: %s", rec.Body.String())
	}
	if len(logs) != 1 || logs[0].Level != "DEBUG" {
		t.Errorf("logs = %+v", logs)
	}
}

// 요청 context는 살아 있는데 내부 작업이 Canceled를 반환하면 서버 오류다.
func TestInternalCancellationIsServerError(t *testing.T) {
	rec, logs := serve(t, newRequest(http.MethodGet, "/api/v1/things/1", nil),
		func(http.ResponseWriter, *http.Request) error { return context.Canceled })
	if rec.Code != 500 || len(logs) != 1 || logs[0].Level != "ERROR" {
		t.Fatalf("status=%d logs=%+v", rec.Code, logs)
	}
}

func TestInstrumentUsesRoutePattern(t *testing.T) {
	type obs struct {
		route  string
		status int
	}
	var got []obs
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/traces/{trace_id}", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNotFound) })
	h := RequestID(Instrument(mux, func(route string, status int, _ time.Duration) { got = append(got, obs{route, status}) }))
	for _, path := range []string{"/api/v1/traces/4bf92f3577b34da6a3ce929d0e0e4736?from=x", "/nope/secret-id"} {
		h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequestWithContext(context.Background(), http.MethodGet, path, nil))
	}
	// 원 URL·ID가 아니라 등록 pattern만, 미등록 경로는 하나로 묶는다(label cardinality)
	if len(got) != 2 || got[0] != (obs{"GET /api/v1/traces/{trace_id}", 404}) || got[1] != (obs{"unmatched", 404}) {
		t.Errorf("observed = %+v", got)
	}
}
