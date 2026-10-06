// Package apicursor는 조회 API의 서명된 page cursor다 (D02 §12, ADR 0034).
//
// cursor는 "tenant·권한 fingerprint·query hash·snapshot 시각·만료를 포함한 서명 토큰"이다.
// 서버만 만들 수 있고(HMAC-SHA256), 다른 tenant·권한·query로 재사용하면 거절한다.
// 내용은 서명만 하고 암호화하지 않는다 — 위치 값(시각, ID)은 응답에 이미 있는 값만 담는다.
//
//	c1.<base64url(JSON claims)>.<base64url(HMAC)>
package apicursor

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"time"
)

const (
	prefix = "c1."
	// MinKeyBytes는 서명 key 최소 길이다.
	MinKeyBytes = 32
	// MaxTokenLen은 받아들이는 cursor 길이 상한이다(외부 입력 검증).
	MaxTokenLen = 2048
)

// ErrInvalid는 cursor가 위조·손상·만료됐거나 이 요청에 묶인 것이 아니라는 뜻이다.
// 원인을 구분해 알리지 않는다(어떤 검사에서 실패했는지가 다른 tenant·권한의 단서가 되지 않게).
var ErrInvalid = errors.New("apicursor: invalid cursor")

// Binding은 cursor를 묶는 요청 문맥이다. 다음 page 요청의 문맥과 같아야 한다.
type Binding struct {
	Tenant      string
	Fingerprint string // 권한 fingerprint (Fingerprint로 만든다)
	QueryHash   string // query 조건 hash (Fingerprint로 만든다). page 크기는 넣지 않는다
}

// Claims는 cursor 내용이다.
type Claims struct {
	Binding
	// Snapshot은 첫 page 요청 시각이다. 이후 page는 이 시각을 넘는 데이터를 읽지 않는다.
	Snapshot time.Time
	Expires  time.Time
	// Position은 호출자가 정한 위치 값(JSON)이다.
	Position json.RawMessage
}

type wire struct {
	T string          `json:"t"`
	F string          `json:"f"`
	Q string          `json:"q"`
	S int64           `json:"s"`
	E int64           `json:"e"`
	P json.RawMessage `json:"p"`
}

// Signer는 cursor를 만들고 검증한다.
type Signer struct {
	key []byte
	ttl time.Duration
	now func() time.Time
}

// NewSigner는 Signer를 만든다. key는 MinKeyBytes 이상, ttl은 양수다.
func NewSigner(key []byte, ttl time.Duration, now func() time.Time) (*Signer, error) {
	if len(key) < MinKeyBytes {
		return nil, errors.New("apicursor: key must be at least 32 bytes")
	}
	if ttl <= 0 {
		return nil, errors.New("apicursor: ttl must be positive")
	}
	if now == nil {
		now = time.Now
	}
	return &Signer{key: append([]byte(nil), key...), ttl: ttl, now: now}, nil
}

// Encode는 claims에 만료(now+ttl)를 넣어 서명한다.
func (s *Signer) Encode(b Binding, snapshot time.Time, position any) (string, error) {
	p, err := json.Marshal(position)
	if err != nil {
		return "", errors.New("apicursor: encode position")
	}
	body, err := json.Marshal(wire{T: b.Tenant, F: b.Fingerprint, Q: b.QueryHash,
		S: snapshot.UnixNano(), E: s.now().Add(s.ttl).UnixNano(), P: p})
	if err != nil {
		return "", errors.New("apicursor: encode claims")
	}
	enc := base64.RawURLEncoding.EncodeToString(body)
	return prefix + enc + "." + base64.RawURLEncoding.EncodeToString(s.mac(enc)), nil
}

// Decode는 서명·만료·binding을 검사하고 claims를 돌려준다. 실패는 모두 ErrInvalid다.
func (s *Signer) Decode(token string, want Binding) (Claims, error) {
	if len(token) > MaxTokenLen || !strings.HasPrefix(token, prefix) {
		return Claims{}, ErrInvalid
	}
	enc, sig, ok := strings.Cut(token[len(prefix):], ".")
	if !ok {
		return Claims{}, ErrInvalid
	}
	got, err := base64.RawURLEncoding.DecodeString(sig)
	if err != nil || !hmac.Equal(got, s.mac(enc)) {
		return Claims{}, ErrInvalid
	}
	body, err := base64.RawURLEncoding.DecodeString(enc)
	if err != nil {
		return Claims{}, ErrInvalid
	}
	var w wire
	if err := json.Unmarshal(body, &w); err != nil {
		return Claims{}, ErrInvalid
	}
	if !s.now().Before(time.Unix(0, w.E)) {
		return Claims{}, ErrInvalid
	}
	// binding 비교는 서명 검증 뒤라 timing 누출 걱정이 없지만, 값 자체는 상수 시간으로 비교한다.
	if !equal(w.T, want.Tenant) || !equal(w.F, want.Fingerprint) || !equal(w.Q, want.QueryHash) {
		return Claims{}, ErrInvalid
	}
	return Claims{
		Binding:  Binding{Tenant: w.T, Fingerprint: w.F, QueryHash: w.Q},
		Snapshot: time.Unix(0, w.S).UTC(), Expires: time.Unix(0, w.E).UTC(), Position: w.P,
	}, nil
}

func (s *Signer) mac(enc string) []byte {
	m := hmac.New(sha256.New, s.key)
	m.Write([]byte(prefix + enc))
	return m.Sum(nil)
}

func equal(a, b string) bool { return hmac.Equal([]byte(a), []byte(b)) }

// Fingerprint는 값 목록의 hash다(권한 fingerprint, query hash). 순서가 의미를 가진다.
// 각 값 앞에 길이를 붙여 경계 모호성("ab","c" vs "a","bc")을 없앤다.
func Fingerprint(parts ...string) string {
	h := sha256.New()
	for _, p := range parts {
		var n [8]byte
		for i, l := 0, uint64(len(p)); i < 8; i++ {
			n[i] = byte(l >> (8 * i))
		}
		h.Write(n[:])
		h.Write([]byte(p))
	}
	return hex.EncodeToString(h.Sum(nil)[:16])
}
