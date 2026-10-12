// Package notify는 경보 webhook 전송의 순수 부분이다 (D02 §17·§20, D04 §03, ADR 0053):
// 서명 secret 생성·암호화, envelope, HMAC 서명, 목적지 검증, SSRF를 막는 HTTP client, 재시도 간격.
// 저장(notification_deliveries)과 전송 루프는 controldb·alertworker가 한다.
package notify

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// 서명 header (receiver 문서: docs/adr/0053).
const (
	HeaderEventID   = "Montracer-Event-Id"
	HeaderTimestamp = "Montracer-Timestamp"
	HeaderKeyID     = "Montracer-Key-Id"
	HeaderSignature = "Montracer-Signature"
	HeaderAttempt   = "Montracer-Delivery-Attempt"
	// SignatureTolerance는 receiver가 받아들일 timestamp 허용 범위다(D02 §17 "5분 초과 서명은 거절").
	SignatureTolerance = 5 * time.Minute
)

// 알림 종류(event_type).
const (
	EventOpened   = "alert.opened"
	EventResolved = "alert.resolved"
)

// Secret은 서명 secret이다. Plain은 만들 때 한 번만 응답한다.
type Secret struct {
	KeyID string // 공개 id(header)
	Key   []byte // HMAC key 32 byte
}

// Plain은 사용자에게 보여주는 형태다.
func (s Secret) Plain() string { return "whsec_" + base64.RawURLEncoding.EncodeToString(s.Key) }

// NewSecret은 새 서명 secret을 만든다.
func NewSecret() (Secret, error) {
	key := make([]byte, 32)
	id := make([]byte, 8)
	if _, err := rand.Read(key); err != nil {
		return Secret{}, fmt.Errorf("notify: secret: %w", err)
	}
	if _, err := rand.Read(id); err != nil {
		return Secret{}, fmt.Errorf("notify: secret id: %w", err)
	}
	return Secret{KeyID: "wk_" + hex.EncodeToString(id), Key: key}, nil
}

// Sealer는 policy의 서명 secret과 webhook URL을 master key(AES-256-GCM)로 암호화한다.
// AAD는 용도·tenant·policy id(secret은 key id까지)라 다른 행·다른 용도로 옮긴 암호문은 풀리지 않는다.
// 새로 암호화할 때는 현재 버전을 쓰고, 풀 때는 저장된 버전의 key를 쓴다(회전 중 이전 key를 함께 둔다).
type Sealer struct {
	current string
	aeads   map[string]cipher.AEAD
}

func newAEAD(key []byte) (cipher.AEAD, error) {
	if len(key) < 32 {
		return nil, errors.New("notify: sealing key must be at least 32 bytes")
	}
	derived := sha256.Sum256(key) // 길이가 다른 key material도 같은 강도의 AES-256 key로
	block, err := aes.NewCipher(derived[:])
	if err != nil {
		return nil, fmt.Errorf("notify: sealing key: %w", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("notify: sealing key: %w", err)
	}
	return aead, nil
}

func validVersion(v string) bool { return v != "" && len(v) <= 32 && !strings.ContainsAny(v, ":,") }

// NewSealer는 현재 master key(32 byte 이상, secret manager에서 주입)와 그 버전 이름으로 Sealer를 만든다.
func NewSealer(version string, key []byte) (*Sealer, error) {
	if !validVersion(version) {
		return nil, errors.New("notify: sealing key version must be 1..32 characters without ':' or ','")
	}
	aead, err := newAEAD(key)
	if err != nil {
		return nil, err
	}
	return &Sealer{current: version, aeads: map[string]cipher.AEAD{version: aead}}, nil
}

// AddPrevious는 이전 버전 key를 풀기 전용으로 더한다(master key 회전 중).
func (s *Sealer) AddPrevious(version string, key []byte) error {
	if !validVersion(version) || version == s.current {
		return errors.New("notify: previous key version must differ from the current one")
	}
	aead, err := newAEAD(key)
	if err != nil {
		return err
	}
	s.aeads[version] = aead
	return nil
}

// SealerFromEnv는 hex master key와 버전 이름으로 Sealer를 만든다. key가 비면 nil(알림 꺼짐)이다. 버전 기본값은 v1.
// previous는 회전 중인 이전 key 목록이다: "v0:<hex>,v00:<hex>".
func SealerFromEnv(keyHex, version, previous string) (*Sealer, error) {
	if keyHex == "" {
		return nil, nil //nolint:nilnil // key가 없으면 알림 기능이 꺼진다(설정 오류 아님)
	}
	key, err := hex.DecodeString(keyHex)
	if err != nil {
		return nil, errors.New("notify: MONTRACER_NOTIFY_KEY_HEX must be hex") // key 내용은 오류에 싣지 않는다
	}
	if version == "" {
		version = "v1"
	}
	s, err := NewSealer(version, key)
	if err != nil {
		return nil, err
	}
	for _, item := range strings.Split(previous, ",") {
		if item = strings.TrimSpace(item); item == "" {
			continue
		}
		v, h, ok := strings.Cut(item, ":")
		k, err := hex.DecodeString(h)
		if !ok || err != nil {
			return nil, errors.New("notify: MONTRACER_NOTIFY_PREVIOUS_KEYS must be version:hex pairs")
		}
		if err := s.AddPrevious(v, k); err != nil {
			return nil, err
		}
	}
	return s, nil
}

// Version은 새로 암호화할 때 쓰는 master key 버전이다(저장한다).
func (s *Sealer) Version() string { return s.current }

func (s *Sealer) seal(plain, ad []byte) ([]byte, error) {
	aead := s.aeads[s.current]
	nonce := make([]byte, aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, fmt.Errorf("notify: nonce: %w", err)
	}
	return aead.Seal(nonce, nonce, plain, ad), nil
}

func (s *Sealer) open(version string, sealed, ad []byte) ([]byte, error) {
	aead, ok := s.aeads[version]
	if !ok || len(sealed) < aead.NonceSize() {
		return nil, ErrSealing
	}
	n := aead.NonceSize()
	plain, err := aead.Open(nil, sealed[:n], sealed[n:], ad)
	if err != nil {
		return nil, ErrSealing
	}
	return plain, nil
}

func secretAAD(tenant, policyID, keyID string) []byte {
	return []byte("secret|" + tenant + "|" + policyID + "|" + keyID)
}
func urlAAD(tenant, policyID string) []byte { return []byte("url|" + tenant + "|" + policyID) }

// Seal은 서명 secret key를 현재 버전으로 암호화한다(nonce || 암호문).
func (s *Sealer) Seal(tenant, policyID string, sec Secret) ([]byte, error) {
	return s.seal(sec.Key, secretAAD(tenant, policyID, sec.KeyID))
}

// ErrSealing은 암호문을 풀 수 없다는 뜻이다(모르는 master key 버전, 다른 행·용도로 옮긴 암호문, 손상).
var ErrSealing = errors.New("notify: cannot open sealed value")

// Open은 서명 secret 암호문을 저장된 버전의 key로 푼다.
func (s *Sealer) Open(tenant, policyID, keyID, version string, sealed []byte) (Secret, error) {
	key, err := s.open(version, sealed, secretAAD(tenant, policyID, keyID))
	if err != nil {
		return Secret{}, err
	}
	return Secret{KeyID: keyID, Key: key}, nil
}

// SealURL은 webhook URL을 현재 버전으로 암호화한다.
func (s *Sealer) SealURL(tenant, policyID, rawURL string) ([]byte, error) {
	return s.seal([]byte(rawURL), urlAAD(tenant, policyID))
}

// OpenURL은 webhook URL 암호문을 푼다.
func (s *Sealer) OpenURL(tenant, policyID, version string, sealed []byte) (string, error) {
	b, err := s.open(version, sealed, urlAAD(tenant, policyID))
	return string(b), err
}

// Sign은 "timestamp.event_id.body"의 HMAC-SHA256(hex)이다(D02 §17 timestamp·event_id·본문, §20 원문 body bytes).
func Sign(key []byte, timestamp time.Time, eventID string, body []byte) string {
	m := hmac.New(sha256.New, key)
	m.Write([]byte(strconv.FormatInt(timestamp.Unix(), 10)))
	m.Write([]byte("."))
	m.Write([]byte(eventID))
	m.Write([]byte("."))
	m.Write(body)
	return "v1=" + hex.EncodeToString(m.Sum(nil))
}

// Verify는 receiver 쪽 검증이다(문서·시험용 참조 구현): 서명이 맞고 timestamp가 now ± SignatureTolerance 안이어야 한다.
func Verify(key []byte, timestampHeader, eventID string, body []byte, signature string, now time.Time) bool {
	sec, err := strconv.ParseInt(timestampHeader, 10, 64)
	if err != nil {
		return false
	}
	ts := time.Unix(sec, 0)
	if d := now.Sub(ts); d > SignatureTolerance || d < -SignatureTolerance {
		return false
	}
	return hmac.Equal([]byte(Sign(key, ts, eventID, body)), []byte(signature))
}

// Event는 webhook envelope다(D02 §20: event_id, event_type, tenant_public_id, occurred_at, resource_id, revision, payload).
// raw telemetry는 싣지 않는다. tenant_public_id는 tenant id다(별도 공개 id가 아직 없다).
type Event struct {
	EventID        string       `json:"event_id"`
	EventType      string       `json:"event_type"`
	SchemaVersion  int          `json:"schema_version"`
	TenantPublicID string       `json:"tenant_public_id"`
	OccurredAt     time.Time    `json:"occurred_at"`
	ResourceID     string       `json:"resource_id"` // monitor id
	Revision       int64        `json:"revision"`    // monitor revision
	Payload        AlertPayload `json:"payload"`
}

// AlertPayload는 경보 사건 하나의 열림·닫힘이다.
type AlertPayload struct {
	MonitorID   string            `json:"monitor_id"`
	MonitorName string            `json:"monitor_name"`
	GroupHash   string            `json:"group_hash"`
	Labels      map[string]string `json:"labels"`
	// LabelsTruncated는 labels가 커서 본문 상한(64KiB)을 넘어 뺐다는 뜻이다(group_hash로 찾는다).
	LabelsTruncated bool       `json:"labels_truncated"`
	EpisodeID       string     `json:"episode_id"`
	EpisodeReason   *string    `json:"episode_reason"` // violation | no_data
	State           *string    `json:"state"`          // 이 알림 시점의 상태. 정의 변경으로 닫히면 null
	Value           *float64   `json:"value"`          // null이면 reason이 있다(0으로 대체하지 않는다)
	Reason          *string    `json:"reason"`
	WindowEnd       *time.Time `json:"window_end"`
	// CloseReason은 닫힌 이유다: recovered(복구) | monitor_revised | monitor_disabled | monitor_deleted. 열림에는 null
	CloseReason *string `json:"close_reason"`
}

// MaxBody는 envelope 크기 상한이다(labels가 큰 경우 저장 CHECK와 같다).
const MaxBody = 64 << 10

// Marshal은 envelope bytes다.
func (e Event) Marshal() ([]byte, error) {
	if e.Payload.Labels == nil {
		e.Payload.Labels = map[string]string{}
	}
	b, err := json.Marshal(e)
	if err != nil {
		return nil, fmt.Errorf("notify: envelope: %w", err)
	}
	if len(b) > MaxBody {
		return nil, fmt.Errorf("notify: envelope is %d bytes (max %d)", len(b), MaxBody)
	}
	return b, nil
}

// 재시도 (D02 §17 "지수 backoff, 최대 24시간").
const (
	FirstRetry = 30 * time.Second
	MaxBackoff = time.Hour
	GiveUp     = 24 * time.Hour
)

// NextAttempt는 attempts번 실패한 뒤의 다음 시도 시각이다. created로부터 GiveUp이 지나면 ok=false(포기, DLQ).
func NextAttempt(created, now time.Time, attempts int) (time.Time, bool) {
	d := FirstRetry
	for i := 1; i < attempts && d < MaxBackoff; i++ {
		d *= 2
	}
	d = min(d, MaxBackoff)
	next := now.Add(d)
	if next.Sub(created) > GiveUp {
		return time.Time{}, false
	}
	return next, true
}
