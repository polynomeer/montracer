package query

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"testing"
	"time"

	"github.com/polynomeer/montracer/internal/apicursor"
	"github.com/polynomeer/montracer/internal/authz"
	"github.com/polynomeer/montracer/internal/controldb"
)

type fakeServices struct {
	all     []controldb.Service
	queries []controldb.ServiceQuery
	envs    [][]string
}

func (f *fakeServices) ListServices(_ context.Context, p authz.Principal, q controldb.ServiceQuery) ([]controldb.Service, bool, error) {
	f.queries = append(f.queries, q)
	f.envs = append(f.envs, p.Environments())
	var out []controldb.Service
	for _, s := range f.all {
		if q.After != nil && s.NameNormalized <= q.After.NameNormalized {
			continue
		}
		out = append(out, s)
	}
	more := len(out) > q.Limit
	if more {
		out = out[:q.Limit]
	}
	return out, more, nil
}

func servicesHandler(t *testing.T, k *keys, store ServiceStore, clock *time.Time) *Handler {
	t.Helper()
	signer, _ := apicursor.NewSigner([]byte("0123456789abcdef0123456789abcdef"), 15*time.Minute, func() time.Time { return *clock })
	h, err := NewHandler(Config{Authenticate: k.authenticate, Store: &fakeStore{}, Services: store, Cursor: signer,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)), Now: func() time.Time { return *clock }})
	if err != nil {
		t.Fatal(err)
	}
	return h
}

func TestListServices(t *testing.T) {
	k := newKeys(t)
	tok := k.issue(t, authz.KindAPIKey, []authz.Action{authz.TelemetryRead}, nil)
	clock := now
	var all []controldb.Service
	for i := 0; i < 3; i++ {
		lang := "go"
		all = append(all, controldb.Service{ServiceID: fmt.Sprintf("aaaaaaaa-0000-4000-8000-00000000000%d", i), Name: fmt.Sprintf("Svc%d", i),
			NameNormalized: fmt.Sprintf("svc%d", i), Environment: "prod", Namespace: "shop", Language: &lang, Status: "active",
			FirstSeen: now.Add(-time.Hour), LastSeen: now, Tags: []string{}})
	}
	store := &fakeServices{all: all}
	h := servicesHandler(t, k, store, &clock)

	var names []string
	params := url.Values{"limit": {"2"}, "environment": {"prod"}}
	for page := 0; page < 3; page++ {
		rec := get(h, "/api/v1/services?"+params.Encode(), tok)
		if rec.Code != http.StatusOK {
			t.Fatalf("page %d: %d %s", page, rec.Code, rec.Body)
		}
		var body struct {
			Data       []ServiceItem `json:"data"`
			NextCursor *string       `json:"next_cursor"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		for _, s := range body.Data {
			names = append(names, s.Name)
			if s.OwnerTeam != nil || s.Status != "active" || s.Language == nil {
				t.Errorf("item = %+v", s)
			}
		}
		if body.NextCursor == nil {
			break
		}
		params.Set("cursor", *body.NextCursor)
		clock = clock.Add(time.Minute)
	}
	if fmt.Sprint(names) != "[Svc0 Svc1 Svc2]" {
		t.Fatalf("names = %v", names)
	}
	last := store.queries[len(store.queries)-1]
	if last.Environment != "prod" || !last.Now.Equal(now) || last.After == nil {
		t.Errorf("last query = %+v (state time must stay at the first page)", last)
	}
	// 다른 filter로 cursor 재사용 → 400, 잘못된 limit → 400
	params.Set("environment", "staging")
	if rec := get(h, "/api/v1/services?"+params.Encode(), tok); rec.Code != http.StatusBadRequest {
		t.Errorf("cursor with other filter: %d", rec.Code)
	}
	if rec := get(h, "/api/v1/services?limit=1001", tok); rec.Code != http.StatusBadRequest {
		t.Errorf("limit 1001: %d", rec.Code)
	}
}

// environment로 제한된 key의 범위는 저장소로 그대로 간다(저장소가 mandatory predicate로 건다).
func TestListServicesEnvironmentScopedKey(t *testing.T) {
	k := newKeys(t)
	tok := k.issue(t, authz.KindAPIKey, []authz.Action{authz.TelemetryRead}, []string{"prod"})
	clock := now
	store := &fakeServices{}
	h := servicesHandler(t, k, store, &clock)
	if rec := get(h, "/api/v1/services", tok); rec.Code != http.StatusOK {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	if fmt.Sprint(store.envs[0]) != "[prod]" {
		t.Errorf("principal envs = %v", store.envs[0])
	}
}
