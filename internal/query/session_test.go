package query

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/polynomeer/montracer/internal/authz"
	"github.com/polynomeer/montracer/internal/controldb"
	"github.com/polynomeer/montracer/internal/session"
	"github.com/polynomeer/montracer/tests/contract"
)

// oneSession은 사용자 하나·session 하나의 시험 저장소다.
type oneSession struct {
	rec  map[[32]byte]controldb.SessionRecord
	role authz.Role
}

func (s *oneSession) UserMemberships(context.Context, string) ([]controldb.Membership, error) {
	return []controldb.Membership{{Tenant: tenantA, Role: s.role}}, nil
}

func (s *oneSession) CreateSession(_ context.Context, n controldb.NewSession) error {
	s.rec[n.IDHash] = controldb.SessionRecord{Tenant: n.Tenant, UserID: n.UserID, LastSeenAt: n.Now, IdleExpiresAt: n.IdleExpiresAt, ExpiresAt: n.ExpiresAt}
	return nil
}

func (s *oneSession) LookupSession(_ context.Context, h [32]byte) (controldb.SessionRecord, error) {
	r, ok := s.rec[h]
	if !ok {
		return controldb.SessionRecord{}, controldb.ErrSessionNotFound
	}
	r.Role = s.role
	return r, nil
}

func (s *oneSession) TouchSession(context.Context, authz.TenantID, [32]byte, time.Time, time.Time) error {
	return nil
}

func (s *oneSession) MarkStepUp(context.Context, authz.TenantID, [32]byte, string, string, string, time.Time) error {
	return nil
}

func (s *oneSession) RevokeSession(context.Context, authz.TenantID, [32]byte, string, string, string, time.Time) error {
	return nil
}

// query-api도 browser session을 받는다: GET은 cookie만, POST 조회는 CSRF token도 있어야 한다(ADR 0064).
func TestSessionCookieAuth(t *testing.T) {
	store := &oneSession{rec: map[[32]byte]controldb.SessionRecord{}, role: authz.RoleViewer}
	m, err := session.New(session.Config{Store: store, Key: bytes.Repeat([]byte{5}, 32), Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	s, err := m.Issue(context.Background(), tenantA, "oidc:000000000000:viewer", "", "", time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	k := newKeys(t)
	h, err := NewHandler(Config{Authenticate: k.authenticate, Store: &fakeStore{}, Services: mapCatalog(), ServiceMap: &fakeEdges{},
		Sessions: m, Logger: slog.New(slog.NewTextHandler(io.Discard, nil)), Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	call := func(method, path, body string, csrf bool) int {
		var r *http.Request
		if body == "" {
			r = httptest.NewRequestWithContext(context.Background(), method, path, nil)
		} else {
			r = httptest.NewRequestWithContext(context.Background(), method, path, strings.NewReader(body))
			r.Header.Set("Content-Type", "application/json")
		}
		r.AddCookie(&http.Cookie{Name: session.CookieName, Value: s.Token, Path: "/", Secure: true, HttpOnly: true, SameSite: http.SameSiteLaxMode})
		if csrf {
			r.Header.Set(session.CSRFHeader, s.CSRFToken)
		}
		rec := httptest.NewRecorder()
		contract.Serve(h, rec, r)
		return rec.Code
	}
	if code := call(http.MethodGet, "/api/v1/services", "", false); code != http.StatusOK && code != http.StatusNotFound {
		t.Fatalf("GET with session cookie: %d", code)
	}
	body := `{` + mapRange + `}`
	if code := call(http.MethodPost, "/api/v1/service-map/query", body, false); code != http.StatusForbidden {
		t.Fatalf("POST without csrf: %d", code)
	}
	if code := call(http.MethodPost, "/api/v1/service-map/query", body, true); code != http.StatusOK {
		t.Fatalf("POST with csrf: %d", code)
	}
	// session이 없으면(쿠키만 남은 경우) 미인증
	store.rec = map[[32]byte]controldb.SessionRecord{}
	if code := call(http.MethodPost, "/api/v1/service-map/query", body, true); code != http.StatusUnauthorized {
		t.Fatalf("unknown session: %d", code)
	}
}
