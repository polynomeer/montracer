package apicursor

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

var (
	key     = []byte("0123456789abcdef0123456789abcdef")
	t0      = time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	binding = Binding{Tenant: "11111111-1111-4111-8111-111111111111", Fingerprint: Fingerprint("api_key", "k1", "operations"), QueryHash: Fingerprint("q")}
)

type pos struct {
	At string `json:"at"`
	ID string `json:"id"`
}

func signer(t *testing.T, now *time.Time) *Signer {
	t.Helper()
	s, err := NewSigner(key, time.Hour, func() time.Time { return *now })
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestRoundTrip(t *testing.T) {
	now := t0
	s := signer(t, &now)
	tok, err := s.Encode(binding, t0, pos{At: "x", ID: "y"})
	if err != nil {
		t.Fatal(err)
	}
	c, err := s.Decode(tok, binding)
	if err != nil {
		t.Fatal(err)
	}
	var p pos
	if err := json.Unmarshal(c.Position, &p); err != nil || p.ID != "y" || !c.Snapshot.Equal(t0) || !c.Expires.Equal(t0.Add(time.Hour)) {
		t.Errorf("claims = %+v pos=%+v", c, p)
	}
}

func TestRejects(t *testing.T) {
	now := t0
	s := signer(t, &now)
	tok, _ := s.Encode(binding, t0, pos{ID: "y"})
	other, _ := NewSigner([]byte("ffffffffffffffffffffffffffffffff"), time.Hour, func() time.Time { return now })
	forged, _ := other.Encode(binding, t0, pos{ID: "y"})
	body, sig, _ := strings.Cut(strings.TrimPrefix(tok, prefix), ".")
	tampered := prefix + body[:len(body)-2] + "AA." + sig

	for name, c := range map[string]struct {
		tok  string
		want Binding
	}{
		"other key":         {forged, binding},
		"tampered body":     {tampered, binding},
		"other tenant":      {tok, Binding{Tenant: "22222222-2222-4222-8222-222222222222", Fingerprint: binding.Fingerprint, QueryHash: binding.QueryHash}},
		"other permissions": {tok, Binding{Tenant: binding.Tenant, Fingerprint: Fingerprint("api_key", "k1", "operations", "security"), QueryHash: binding.QueryHash}},
		"other query":       {tok, Binding{Tenant: binding.Tenant, Fingerprint: binding.Fingerprint, QueryHash: Fingerprint("q2")}},
		"no prefix":         {strings.TrimPrefix(tok, prefix), binding},
		"garbage":           {"c1.!!!.???", binding},
		"too long":          {prefix + strings.Repeat("a", MaxTokenLen), binding},
	} {
		if _, err := s.Decode(c.tok, c.want); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: %v, want ErrInvalid", name, err)
		}
	}
	now = t0.Add(time.Hour)
	if _, err := s.Decode(tok, binding); !errors.Is(err, ErrInvalid) {
		t.Errorf("expired: %v", err)
	}
}

func TestFingerprintBoundaries(t *testing.T) {
	if Fingerprint("ab", "c") == Fingerprint("a", "bc") {
		t.Error("boundary ambiguity")
	}
	first, again := Fingerprint("a", "b"), Fingerprint("a", "b")
	if first != again || first == Fingerprint("b", "a") {
		t.Error("fingerprint not deterministic or order-insensitive")
	}
}

func TestNewSignerValidates(t *testing.T) {
	if _, err := NewSigner(key[:31], time.Hour, nil); err == nil {
		t.Error("short key accepted")
	}
	if _, err := NewSigner(key, 0, nil); err == nil {
		t.Error("zero ttl accepted")
	}
}
