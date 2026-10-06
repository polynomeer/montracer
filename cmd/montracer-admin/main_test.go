package main

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/polynomeer/montracer/internal/authz"
	"github.com/polynomeer/montracer/internal/controldb"
)

type fakeStore struct {
	grants  []authz.BreakGlassGrant
	keyIDs  []string
	reqIDs  []string
	outcome controldb.RevokeOutcome
	err     error
	keys    []controldb.KeyMetadata
}

func (f *fakeStore) BreakGlassListKeys(_ context.Context, g authz.BreakGlassGrant, req string) ([]controldb.KeyMetadata, error) {
	f.grants, f.reqIDs = append(f.grants, g), append(f.reqIDs, req)
	return f.keys, f.err
}

func (f *fakeStore) BreakGlassRevokeKey(_ context.Context, g authz.BreakGlassGrant, keyID, req string) (controldb.RevokeOutcome, error) {
	f.grants, f.keyIDs, f.reqIDs = append(f.grants, g), append(f.keyIDs, keyID), append(f.reqIDs, req)
	return f.outcome, f.err
}

const (
	tenantS = "11111111-1111-4111-8111-111111111111"
	keyID   = "0123456789abcdef"
)

func runWith(t *testing.T, store *fakeStore, envOp string, args ...string) (string, string, error, bool) {
	t.Helper()
	var out, errOut bytes.Buffer
	opened := false
	e := env{
		getenv: func(k string) string {
			if k == "MONTRACER_OPERATOR_ID" {
				return envOp
			}
			return ""
		},
		stdout: &out, stderr: &errOut, now: func() time.Time { return time.Date(2026, 10, 6, 9, 0, 0, 0, time.UTC) },
		open: func(context.Context) (keyStore, func(), error) {
			opened = true
			return store, func() {}, nil
		},
	}
	err := run(context.Background(), args, e)
	return out.String(), errOut.String(), err, opened
}

var common = []string{"--tenant", tenantS, "--approver", "sec-lee", "--ticket", "INC-1042", "--reason", "leaked key suspected, revoke now"}

func TestRevokeRequiresConfirmation(t *testing.T) {
	s := &fakeStore{outcome: controldb.RevokeApplied}
	out, _, err, opened := runWith(t, s, "op-kim", append([]string{"keys", "revoke", "--key-id", keyID}, common...)...)
	if err != nil || opened || len(s.keyIDs) != 0 || !strings.Contains(out, "--yes") {
		t.Fatalf("without --yes: out=%q err=%v opened=%v", out, err, opened)
	}
	out, logs, err, _ := runWith(t, s, "op-kim", append([]string{"keys", "revoke", "--key-id", keyID, "--yes"}, common...)...)
	if err != nil || len(s.keyIDs) != 1 || s.keyIDs[0] != keyID || !strings.Contains(out, "revoked") {
		t.Fatalf("revoke: out=%q err=%v", out, err)
	}
	g := s.grants[0]
	if g.Operator() != "op-kim" || g.Approver() != "sec-lee" || g.Ticket() != "INC-1042" ||
		g.Check(authz.BreakGlassKeysRevoke, g.IssuedAt()) != nil || g.Check(authz.BreakGlassKeysList, g.IssuedAt()) == nil {
		t.Errorf("grant = %+v, want revoke-only for op-kim", g)
	}
	if !strings.HasPrefix(s.reqIDs[0], "bg-") || !strings.Contains(logs, s.reqIDs[0]) || strings.Contains(logs, "leaked key suspected") {
		t.Errorf("logs = %s (want request id, no free-text reason)", logs)
	}
}

// 운영자 신원은 session env에서만 온다. 인자로 줄 수 없고, env가 없으면 실행하지 않는다.
func TestOperatorIdentityComesFromSession(t *testing.T) {
	s := &fakeStore{}
	if _, _, err, opened := runWith(t, s, "op-kim", append([]string{"keys", "list", "--operator", "someone-else"}, common...)...); err == nil || opened {
		t.Fatalf("--operator accepted: %v", err)
	}
	_, logs, err, opened := runWith(t, s, "", append([]string{"keys", "list"}, common...)...)
	if err == nil || opened || !strings.Contains(logs, "break-glass rejected") {
		t.Fatalf("missing session operator: err=%v opened=%v logs=%s", err, opened, logs)
	}
}

func TestRejectsBeforeConnecting(t *testing.T) {
	for name, args := range map[string][]string{
		"no subcommand":  {"keys"},
		"bad tenant":     {"keys", "list", "--tenant", "x", "--approver", "a", "--ticket", "t", "--reason", "long enough reason"},
		"self approval":  {"keys", "list", "--tenant", tenantS, "--approver", "op-kim", "--ticket", "t", "--reason", "long enough reason"},
		"short reason":   {"keys", "list", "--tenant", tenantS, "--approver", "a", "--ticket", "t", "--reason", "x"},
		"missing key id": append([]string{"keys", "revoke", "--yes"}, common...),
		"bad key id":     append([]string{"keys", "revoke", "--yes", "--key-id", "k_1"}, common...),
		"upper key id":   append([]string{"keys", "revoke", "--yes", "--key-id", "0123456789ABCDEF"}, common...),
		"extra args":     append(append([]string{"keys", "list"}, common...), "stray"),
	} {
		s := &fakeStore{}
		_, logs, err, opened := runWith(t, s, "op-kim", args...)
		if err == nil || opened {
			t.Errorf("%s: err=%v opened=%v", name, err, opened)
		} else if !errors.Is(err, errUsage) && !strings.Contains(logs, "break-glass rejected") {
			t.Errorf("%s: rejection not logged: %s", name, logs)
		}
	}
}

func TestListPrintsMetadataAndNotFoundIsExplained(t *testing.T) {
	exp := time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC)
	s := &fakeStore{keys: []controldb.KeyMetadata{{KeyID: "k_1", Kind: authz.KindIngestKey, Scopes: []authz.Action{authz.IngestTraces},
		Environments: []string{"prod"}, ExpiresAt: exp, CreatedAt: exp.AddDate(0, -1, 0)}}}
	out, _, err, _ := runWith(t, s, "op-kim", append([]string{"keys", "list"}, common...)...)
	if err != nil || !strings.Contains(out, "k_1") || !strings.Contains(out, "ingest.traces") {
		t.Fatalf("list: out=%q err=%v", out, err)
	}
	s = &fakeStore{err: authz.ErrNotFound}
	_, _, err, _ = runWith(t, s, "op-kim", append([]string{"keys", "revoke", "--key-id", keyID, "--yes"}, common...)...)
	if err == nil || !strings.Contains(err.Error(), "attempt audited") || errors.Is(err, errUsage) {
		t.Errorf("not found = %v", err)
	}
}
