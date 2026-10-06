package authz

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func mustGrant(t *testing.T, actions ...BreakGlassAction) BreakGlassGrant {
	t.Helper()
	g, err := NewBreakGlassGrant(tenantA, "op-kim", "sec-lee", "INC-1042", "probe isolation failed, revoke leaked key", actions, testNow, 30*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	return g
}

func TestBreakGlassGrantChecksActionAndExpiry(t *testing.T) {
	g := mustGrant(t, BreakGlassKeysRevoke)
	if err := g.Check(BreakGlassKeysRevoke, testNow.Add(29*time.Minute)); err != nil {
		t.Errorf("within ttl: %v", err)
	}
	if err := g.Check(BreakGlassKeysRevoke, testNow.Add(30*time.Minute)); !errors.Is(err, ErrBreakGlassExpired) {
		t.Errorf("at expiry: %v, want ErrBreakGlassExpired", err)
	}
	if err := g.Check(BreakGlassKeysList, testNow); !errors.Is(err, ErrForbidden) {
		t.Errorf("action outside grant: %v, want ErrForbidden", err)
	}
	if err := (BreakGlassGrant{}).Check(BreakGlassKeysList, testNow); !errors.Is(err, ErrUnauthenticated) {
		t.Errorf("zero grant: %v, want ErrUnauthenticated", err)
	}
	if !g.ExpiresAt().Equal(testNow.Add(30*time.Minute)) || g.Operator() != "op-kim" || g.Approver() != "sec-lee" || g.Tenant() != tenantA {
		t.Errorf("grant fields = %+v", g)
	}
}

func TestNewBreakGlassGrantRejects(t *testing.T) {
	reason := "probe isolation failed, revoke leaked key"
	all := []BreakGlassAction{BreakGlassKeysList}
	for name, f := range map[string]func() error{
		"zero tenant": func() error {
			_, err := NewBreakGlassGrant(TenantID{}, "op", "sec", "INC-1", reason, all, testNow, time.Minute)
			return err
		},
		"self approval": func() error {
			_, err := NewBreakGlassGrant(tenantA, "op-kim", " OP-KIM ", "INC-1", reason, all, testNow, time.Minute)
			return err
		},
		"missing approver": func() error {
			_, err := NewBreakGlassGrant(tenantA, "op", "", "INC-1", reason, all, testNow, time.Minute)
			return err
		},
		"missing ticket": func() error {
			_, err := NewBreakGlassGrant(tenantA, "op", "sec", " ", reason, all, testNow, time.Minute)
			return err
		},
		"short reason": func() error {
			_, err := NewBreakGlassGrant(tenantA, "op", "sec", "INC-1", "fix", all, testNow, time.Minute)
			return err
		},
		"long reason": func() error {
			_, err := NewBreakGlassGrant(tenantA, "op", "sec", "INC-1", strings.Repeat("가", 501), all, testNow, time.Minute)
			return err
		},
		"control chars": func() error {
			_, err := NewBreakGlassGrant(tenantA, "op", "sec", "INC-1", "line one\nline two here", all, testNow, time.Minute)
			return err
		},
		"ttl over 30m": func() error {
			_, err := NewBreakGlassGrant(tenantA, "op", "sec", "INC-1", reason, all, testNow, 31*time.Minute)
			return err
		},
		"zero ttl": func() error {
			_, err := NewBreakGlassGrant(tenantA, "op", "sec", "INC-1", reason, all, testNow, 0)
			return err
		},
		"no action": func() error {
			_, err := NewBreakGlassGrant(tenantA, "op", "sec", "INC-1", reason, nil, testNow, time.Minute)
			return err
		},
		"unknown action": func() error {
			_, err := NewBreakGlassGrant(tenantA, "op", "sec", "INC-1", reason, []BreakGlassAction{"break_glass.telemetry.read"}, testNow, time.Minute)
			return err
		},
	} {
		if f() == nil {
			t.Errorf("%s accepted", name)
		}
	}
}

func TestValidKeyID(t *testing.T) {
	for id, want := range map[string]bool{
		"0123456789abcdef": true, "0123456789ABCDEF": false, "0123456789abcde": false,
		"0123456789abcdef0": false, "0123456789abcdeg": false, "": false,
	} {
		if got := ValidKeyID(id); got != want {
			t.Errorf("ValidKeyID(%q) = %v, want %v", id, got, want)
		}
	}
}

// break-glass는 운영자 도구에서만 쓴다. 서비스(ingress·query-api·control-api 등)가 grant를 만들거나
// break-glass 저장소 작업을 부르면 고객 경로에 운영자 권한이 섞인다(ADR 0033 §1).
func TestBreakGlassConfinedToAdminTool(t *testing.T) {
	allowed := map[string]bool{
		"internal/authz/breakglass.go":     true, // 정의
		"internal/controldb/breakglass.go": true, // 저장소 작업(정의)
		"cmd/montracer-admin/main.go":      true, // 유일한 사용처
	}
	fsys := os.DirFS(filepath.Join("..", ".."))
	err := fs.WalkDir(fsys, ".", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() && path != "." && (d.Name() == "node_modules" || strings.HasPrefix(d.Name(), ".")) {
			return fs.SkipDir
		}
		if d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") || allowed[path] {
			return nil
		}
		b, err := fs.ReadFile(fsys, path)
		if err != nil {
			return err
		}
		for _, sym := range []string{"NewBreakGlassGrant(", "BreakGlassRevokeKey(", "BreakGlassListKeys("} {
			if strings.Contains(string(b), sym) {
				t.Errorf("%s uses %s outside the admin tool", path, sym)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
