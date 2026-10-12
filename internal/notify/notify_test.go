package notify

import (
	"bytes"
	"context"
	"crypto/x509"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"
	"time"
)

var t0 = time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC)

func TestPublicAddr(t *testing.T) {
	for addr, want := range map[string]bool{
		"8.8.8.8": true, "1.1.1.1": true, "2606:4700:4700::1111": true,
		"127.0.0.1": false, "10.1.2.3": false, "172.16.0.1": false, "192.168.1.1": false, "169.254.169.254": false, // cloud metadata
		"100.64.0.1": false, "0.0.0.0": false, "255.255.255.255": false, "224.0.0.1": false, "198.18.0.1": false,
		"::1": false, "fc00::1": false, "fd00:ec2::254": false, "fe80::1": false, "::": false, "ff02::1": false,
		"::ffff:10.0.0.1": false, "::ffff:8.8.8.8": true, "64:ff9b::a00:1": false, "2002:a00:1::": false, "2001:db8::1": false,
		"::7f00:1": false, "fec0::1": false, "2001::1": false, "3fff::1": false, "192.88.99.1": false, "2001:10::1": false, "4000::1": false,
	} {
		if got := PublicAddr(netip.MustParseAddr(addr)); got != want {
			t.Errorf("PublicAddr(%s) = %v, want %v", addr, got, want)
		}
	}
}

func TestValidateURL(t *testing.T) {
	ok := []string{"https://hooks.example.com/services/T0/B0/xyz", "https://example.com:8443/a?b=c", "https://[2606:4700:4700::1111]/x"}
	bad := []string{"http://example.com/x", "ftp://example.com", "https://", "https://user:pass@example.com/", "https://example.com/#frag",
		"https://localhost/x", "https://api.localhost/x", "https://metadata.google.internal/", "https://printer.local/",
		"https://127.0.0.1/x", "https://169.254.169.254/latest/meta-data", "https://[::1]/x", "https://10.0.0.1/", "https://example.com:99999/",
		"https://" + strings.Repeat("a", 2050) + ".com/",
		"https://127.1/x", "https://2130706433/x", "https://0x7f000001/x", "https://[::7f00:1]/x", "https://[fec0::1]/x"}
	for _, u := range ok {
		if err := ValidateURL(u); err != nil {
			t.Errorf("ValidateURL(%s) = %v", u, err)
		}
	}
	for _, u := range bad {
		if err := ValidateURL(u); err == nil {
			t.Errorf("ValidateURL(%.60s) accepted", u)
		}
	}
	if got := RedactURL("https://hooks.example.com/services/T0/B0/secret?token=x"); got != "https://hooks.example.com/…" {
		t.Errorf("redact = %s", got)
	}
	if got := RedactURL("https://example.com"); got != "https://example.com" {
		t.Errorf("redact bare = %s", got)
	}
}

func TestSealer(t *testing.T) {
	key := bytes.Repeat([]byte{7}, 32)
	s, err := NewSealer("v1", key)
	if err != nil {
		t.Fatal(err)
	}
	sec, err := NewSecret()
	if err != nil {
		t.Fatal(err)
	}
	sealed, err := s.Seal("tenant-a", "policy-1", sec)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(sealed, sec.Key) {
		t.Fatal("ciphertext contains the key")
	}
	got, err := s.Open("tenant-a", "policy-1", sec.KeyID, "v1", sealed)
	if err != nil || !bytes.Equal(got.Key, sec.Key) || got.KeyID != sec.KeyID {
		t.Fatalf("open: %v", err)
	}
	// 다른 tenant·policy·key id로 옮긴 암호문, 다른 master key 버전·key는 풀리지 않는다
	for _, c := range []struct{ tenant, policy, keyID, version string }{
		{"tenant-b", "policy-1", sec.KeyID, "v1"}, {"tenant-a", "policy-2", sec.KeyID, "v1"}, {"tenant-a", "policy-1", "wk_other", "v1"}, {"tenant-a", "policy-1", sec.KeyID, "v2"},
	} {
		if _, err := s.Open(c.tenant, c.policy, c.keyID, c.version, sealed); !errors.Is(err, ErrSealing) {
			t.Errorf("open %+v: %v", c, err)
		}
	}
	other, _ := NewSealer("v1", bytes.Repeat([]byte{8}, 32))
	if _, err := other.Open("tenant-a", "policy-1", sec.KeyID, "v1", sealed); !errors.Is(err, ErrSealing) {
		t.Errorf("other key: %v", err)
	}
	if _, err := NewSealer("v1", key[:16]); err == nil {
		t.Error("short key accepted")
	}
	// URL 암호문: 같은 tenant·policy로만 풀리고, secret 암호문과 용도가 섞이지 않는다
	su, err := s.SealURL("tenant-a", "policy-1", "https://hooks.example.com/t0k3n")
	if err != nil {
		t.Fatal(err)
	}
	if u, err := s.OpenURL("tenant-a", "policy-1", "v1", su); err != nil || u != "https://hooks.example.com/t0k3n" {
		t.Errorf("open url: %q %v", u, err)
	}
	if _, err := s.OpenURL("tenant-b", "policy-1", "v1", su); !errors.Is(err, ErrSealing) {
		t.Errorf("url under another tenant: %v", err)
	}
	if _, err := s.Open("tenant-a", "policy-1", sec.KeyID, "v1", su); !errors.Is(err, ErrSealing) {
		t.Errorf("url opened as a secret: %v", err)
	}
	// 회전: 새 key(v2)로 암호화하고 이전 key(v1)로 된 암호문도 푼다
	rotated, err := NewSealer("v2", bytes.Repeat([]byte{9}, 32))
	if err != nil {
		t.Fatal(err)
	}
	if err := rotated.AddPrevious("v1", key); err != nil {
		t.Fatal(err)
	}
	if got, err := rotated.Open("tenant-a", "policy-1", sec.KeyID, "v1", sealed); err != nil || !bytes.Equal(got.Key, sec.Key) || rotated.Version() != "v2" {
		t.Errorf("open with previous key: %v", err)
	}
	if env, err := SealerFromEnv(hex.EncodeToString(bytes.Repeat([]byte{9}, 32)), "v2", "v1:"+hex.EncodeToString(key)); err != nil || env.Version() != "v2" {
		t.Errorf("from env: %v", err)
	} else if _, err := env.Open("tenant-a", "policy-1", sec.KeyID, "v1", sealed); err != nil {
		t.Errorf("env previous key: %v", err)
	}
	if off, err := SealerFromEnv("", "", ""); off != nil || err != nil {
		t.Errorf("empty key: %v %v", off, err)
	}
	if _, err := SealerFromEnv("zz", "", ""); err == nil {
		t.Error("non-hex key accepted")
	}
	if !strings.HasPrefix(sec.Plain(), "whsec_") || !strings.HasPrefix(sec.KeyID, "wk_") {
		t.Errorf("secret format %s %s", sec.Plain(), sec.KeyID)
	}
}

func TestSignVerify(t *testing.T) {
	key := []byte("secret-key-32-bytes-for-testing!")
	body := []byte(`{"event_id":"e1"}`)
	sig := Sign(key, t0, "e1", body)
	if !strings.HasPrefix(sig, "v1=") || !Verify(key, "1791633600", "e1", body, sig, t0.Add(time.Minute)) {
		t.Fatalf("verify failed: %s", sig)
	}
	for name, ok := range map[string]bool{
		"body changed":     Verify(key, "1791633600", "e1", []byte(`{"event_id":"e2"}`), sig, t0),
		"event id changed": Verify(key, "1791633600", "e2", body, sig, t0),
		"other key":        Verify([]byte("another-key-32-bytes-for-testing"), "1791633600", "e1", body, sig, t0),
		"too old":          Verify(key, "1791633600", "e1", body, sig, t0.Add(6*time.Minute)),
		"bad timestamp":    Verify(key, "x", "e1", body, sig, t0),
	} {
		if ok {
			t.Errorf("%s: verified", name)
		}
	}
}

func TestNextAttempt(t *testing.T) {
	created := t0
	want := []time.Duration{30 * time.Second, time.Minute, 2 * time.Minute, 4 * time.Minute}
	for i, d := range want {
		next, ok := NextAttempt(created, t0, i+1)
		if !ok || next.Sub(t0) != d {
			t.Errorf("attempt %d: %v %v", i+1, next.Sub(t0), ok)
		}
	}
	if next, ok := NextAttempt(created, t0, 20); !ok || next.Sub(t0) != MaxBackoff {
		t.Errorf("cap: %v", next.Sub(t0))
	}
	// 24시간을 넘기는 다음 시도는 없다(포기)
	if _, ok := NextAttempt(created, t0.Add(23*time.Hour+30*time.Minute), 20); ok {
		t.Error("retry beyond 24h")
	}
}

func tlsClient(srv *httptest.Server, allowLoopback bool) *Client {
	pool := x509.NewCertPool()
	pool.AddCert(srv.Certificate())
	o := ClientOptions{Timeout: 2 * time.Second, rootCAs: pool}
	if allowLoopback {
		o.allow = func(netip.Addr) bool { return true }
	}
	return NewClient(o)
}

// 받는 쪽에서 서명이 검증되고, header에 event id·key id·시도 번호가 있다
func TestDeliverSignsAndReports(t *testing.T) {
	sec, _ := NewSecret()
	body := []byte(`{"event_id":"e1","event_type":"alert.opened"}`)
	var got *http.Request
	var gotBody []byte
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got, gotBody = r, mustRead(r)
		w.WriteHeader(http.StatusAccepted)
	}))
	defer srv.Close()
	now := time.Now()
	r := tlsClient(srv, true).Deliver(context.Background(), srv.URL+"/hook", sec, "e1", body, 2, now)
	if r.ErrCode != "" || r.StatusCode != http.StatusAccepted {
		t.Fatalf("result = %+v", r)
	}
	if got.Header.Get(HeaderEventID) != "e1" || got.Header.Get(HeaderKeyID) != sec.KeyID || got.Header.Get(HeaderAttempt) != "2" ||
		!Verify(sec.Key, got.Header.Get(HeaderTimestamp), "e1", gotBody, got.Header.Get(HeaderSignature), now) || !bytes.Equal(gotBody, body) {
		t.Errorf("request headers %v body %s", got.Header, gotBody)
	}
}

func mustRead(r *http.Request) []byte {
	b, _ := io.ReadAll(r.Body)
	return b
}

// 기본 client는 loopback(사설) 연결을 dialer에서 막는다: URL 검사를 통과한 이름이 사설 주소로 풀려도(DNS rebinding) 막힌다
func TestDeliverBlocksPrivateAddress(t *testing.T) {
	hits := 0
	srv := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { hits++ }))
	defer srv.Close()
	sec, _ := NewSecret()
	// 127.0.0.1 literal은 URL 검사에서 막힌다
	c := tlsClient(srv, false)
	if r := c.Deliver(context.Background(), srv.URL, sec, "e1", []byte(`{}`), 1, time.Now()); r.ErrCode != ErrCodeInvalid || !r.Permanent {
		t.Errorf("literal loopback: %+v", r)
	}
	// 공개처럼 보이는 이름이 loopback으로 풀리는 경우(DNS rebinding)를 외부 DNS 없이 보려고 연결 단계 검사를 직접 부른다
	port := srv.URL[strings.LastIndex(srv.URL, ":")+1:]
	conn, err := c.http.Transport.(*http.Transport).DialContext(context.Background(), "tcp", "127.0.0.1:"+port)
	if err == nil {
		_ = conn.Close()
		t.Error("dialer connected to loopback")
	}
	if r := classifyError(err); r.ErrCode != ErrCodeBlocked || !r.Permanent {
		t.Errorf("dial error classified %+v (%v)", r, err)
	}
	if hits != 0 {
		t.Errorf("server reached %d times", hits)
	}
}

// redirect는 따라가지 않고(목적지 바꾸기로 사설 주소에 닿지 못하게) 실패로 본다. 5xx는 재시도 대상 실패다.
func TestDeliverRedirectAndStatus(t *testing.T) {
	followed := false
	internal := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { followed = true }))
	defer internal.Close()
	status := http.StatusFound
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Location", internal.URL)
		w.WriteHeader(status)
	}))
	defer srv.Close()
	sec, _ := NewSecret()
	c := tlsClient(srv, true)
	if r := c.Deliver(context.Background(), srv.URL, sec, "e1", []byte(`{}`), 1, time.Now()); r.ErrCode != ErrCodeRedirect || r.Permanent || followed {
		t.Errorf("redirect: %+v followed=%v", r, followed)
	}
	status = http.StatusServiceUnavailable
	if r := c.Deliver(context.Background(), srv.URL, sec, "e1", []byte(`{}`), 1, time.Now()); r.ErrCode != ErrCodeStatus || r.StatusCode != 503 || r.Permanent {
		t.Errorf("503: %+v", r)
	}
}

func TestDeliverTimeoutAndTLS(t *testing.T) {
	block := make(chan struct{})
	srv := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { <-block }))
	defer srv.Close()
	defer close(block)
	sec, _ := NewSecret()
	c := tlsClient(srv, true)
	c.http.Timeout = 200 * time.Millisecond
	if r := c.Deliver(context.Background(), srv.URL, sec, "e1", []byte(`{}`), 1, time.Now()); r.ErrCode != ErrCodeTimeout || r.Permanent {
		t.Errorf("timeout: %+v", r)
	}
	// 신뢰하지 않는 인증서
	untrusted := NewClient(ClientOptions{Timeout: time.Second, allow: func(netip.Addr) bool { return true }})
	if r := untrusted.Deliver(context.Background(), srv.URL, sec, "e1", []byte(`{}`), 1, time.Now()); r.ErrCode != ErrCodeTLS {
		t.Errorf("untrusted cert: %+v", r)
	}
}
