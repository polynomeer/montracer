package authz

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"time"
)

// Key 형식 (D04 §02):
//
//	<prefix><key_id>_<secret>
//	prefix  "mti_"(ingest) | "mta_"(api)
//	key_id  8 byte 무작위, hex 16자. 저장소 조회용, 비밀이 아니다.
//	secret  32 byte(256-bit) 무작위, base64url(raw) 43자.
//
// 저장소에는 key_id와 keyed hash(HMAC-SHA256(pepper, key_id || 0x00 || secret))만 둔다.
// 원문 token은 생성 응답에서 한 번만 반환한다.
const (
	ingestKeyPrefix = "mti_"
	apiKeyPrefix    = "mta_"
	keyIDBytes      = 8
	keySecretBytes  = 32
	minPepperBytes  = 32
	keyIDLen        = keyIDBytes * 2
	keySecretLen    = 43 // base64.RawURLEncoding.EncodedLen(32)
	tokenLen        = len(ingestKeyPrefix) + keyIDLen + 1 + keySecretLen
)

var errMalformedToken = errors.New("authz: malformed key token")

// KeyHasher는 server-side pepper로 key secret의 keyed hash를 만든다.
type KeyHasher struct {
	pepper []byte
}

// NewKeyHasher는 최소 32 byte pepper를 요구한다. pepper는 secret manager에서 주입한다.
func NewKeyHasher(pepper []byte) (KeyHasher, error) {
	if len(pepper) < minPepperBytes {
		return KeyHasher{}, fmt.Errorf("authz: pepper must be at least %d bytes", minPepperBytes)
	}
	return KeyHasher{pepper: append([]byte(nil), pepper...)}, nil
}

func (h KeyHasher) hash(keyID, secret string) []byte {
	m := hmac.New(sha256.New, h.pepper)
	m.Write([]byte(keyID))
	m.Write([]byte{0})
	m.Write([]byte(secret))
	return m.Sum(nil)
}

// GeneratedKey는 발급 결과다. Token은 응답으로 한 번만 보여주고 저장하지 않는다.
// 로그·fmt 출력에서는 Token과 Hash를 가린다.
type GeneratedKey struct {
	Token string
	KeyID string
	Hash  []byte
}

// String은 secret을 노출하지 않는다.
func (g GeneratedKey) String() string { return "GeneratedKey{KeyID:" + g.KeyID + " Token:[REDACTED]}" }

// GoString은 %#v 출력에서도 secret을 노출하지 않는다.
func (g GeneratedKey) GoString() string { return g.String() }

// LogValue는 slog 출력에서 secret을 노출하지 않는다.
func (g GeneratedKey) LogValue() slog.Value {
	return slog.GroupValue(slog.String("key_id", g.KeyID), slog.String("token", "[REDACTED]"))
}

// Generate는 새 key를 만든다. random은 보통 crypto/rand.Reader다(nil이면 crypto/rand 사용).
func (h KeyHasher) Generate(kind Kind, random io.Reader) (GeneratedKey, error) {
	prefix, err := prefixFor(kind)
	if err != nil {
		return GeneratedKey{}, err
	}
	if random == nil {
		random = rand.Reader
	}
	buf := make([]byte, keyIDBytes+keySecretBytes)
	if _, err := io.ReadFull(random, buf); err != nil {
		return GeneratedKey{}, fmt.Errorf("authz: generate key: %w", err)
	}
	keyID := hex.EncodeToString(buf[:keyIDBytes])
	secret := base64.RawURLEncoding.EncodeToString(buf[keyIDBytes:])
	return GeneratedKey{
		Token: prefix + keyID + "_" + secret,
		KeyID: keyID,
		Hash:  h.hash(keyID, secret),
	}, nil
}

func prefixFor(kind Kind) (string, error) {
	switch kind {
	case KindIngestKey:
		return ingestKeyPrefix, nil
	case KindAPIKey:
		return apiKeyPrefix, nil
	default:
		return "", errors.New("authz: not a key kind")
	}
}

// parseToken은 길이·문자 집합을 엄격히 검사한다. 오류에 token 내용을 넣지 않는다.
func parseToken(token string) (kind Kind, keyID, secret string, err error) {
	if len(token) != tokenLen {
		return kindUnknown, "", "", errMalformedToken
	}
	switch {
	case strings.HasPrefix(token, ingestKeyPrefix):
		kind = KindIngestKey
	case strings.HasPrefix(token, apiKeyPrefix):
		kind = KindAPIKey
	default:
		return kindUnknown, "", "", errMalformedToken
	}
	rest := token[len(ingestKeyPrefix):]
	if rest[keyIDLen] != '_' {
		return kindUnknown, "", "", errMalformedToken
	}
	keyID, secret = rest[:keyIDLen], rest[keyIDLen+1:]
	if _, err := hex.DecodeString(keyID); err != nil || strings.ToLower(keyID) != keyID {
		return kindUnknown, "", "", errMalformedToken
	}
	if b, err := base64.RawURLEncoding.DecodeString(secret); err != nil || len(b) != keySecretBytes {
		return kindUnknown, "", "", errMalformedToken
	}
	return kind, keyID, secret, nil
}

// KeyRecord는 저장소의 key metadata다. 원문 secret은 없다.
type KeyRecord struct {
	KeyID        string
	Tenant       TenantID
	Kind         Kind
	Hash         []byte
	Scopes       []Action
	Environments []string // ingest key는 필수 (D04 §02)
	// IssuerRole은 발급자의 현재 membership role이다. 저장소가 조회 시 함께 채운다.
	// 발급자가 조직에서 제거되었으면 비워 둔다. API key에만 쓰인다 (ADR 0015 §1).
	IssuerRole Role
	ExpiresAt  time.Time  // 필수. zero이면 유효하지 않은 기록으로 본다.
	RevokedAt  *time.Time // revoke 시각. 이 시각 이후 거절한다. 교체 시 미래 시각으로 겹침 기간을 둔다.
}

// ErrKeyNotFound는 KeyLookup이 key_id를 찾지 못했을 때 반환해야 하는 오류다.
var ErrKeyNotFound = errors.New("authz: key not found")

// KeyLookup은 key_id로 기록을 찾는다. 미존재는 ErrKeyNotFound, 저장소 장애는 그 외 오류로 반환한다.
type KeyLookup func(ctx context.Context, keyID string) (KeyRecord, error)

// dummyKeyID는 미존재 key에서도 hash 계산을 수행해 응답 시간 차이를 줄이는 데 쓴다.
const dummyKeyID = "0000000000000000"

// Authenticate는 bearer token을 검증해 principal을 만든다.
//
// want는 이 endpoint가 받는 key 종류다. ingest key로 query API를 호출하는 식의 혼용은 거절한다 (D02 §12).
// 미존재·hash 불일치·폐기·만료·종류 불일치는 모두 ErrUnauthenticated로 같게 보고한다.
// 저장소 장애는 ErrBackendUnavailable로 반환하며 호출자는 fail closed 해야 한다 (D02 §02).
func (h KeyHasher) Authenticate(ctx context.Context, token string, want Kind, lookup KeyLookup, now time.Time) (Principal, error) {
	kind, keyID, secret, err := parseToken(token)
	if err != nil || kind != want {
		return Principal{}, ErrUnauthenticated
	}
	rec, err := lookup(ctx, keyID)
	if errors.Is(err, ErrKeyNotFound) {
		_ = hmac.Equal(h.hash(dummyKeyID, secret), make([]byte, sha256.Size))
		return Principal{}, ErrUnauthenticated
	}
	if err != nil {
		return Principal{}, fmt.Errorf("%w: key lookup: %w", ErrBackendUnavailable, err)
	}
	if !hmac.Equal(h.hash(keyID, secret), rec.Hash) {
		return Principal{}, ErrUnauthenticated
	}
	if rec.KeyID != keyID || rec.Kind != kind {
		return Principal{}, ErrUnauthenticated
	}
	if rec.ExpiresAt.IsZero() || !now.Before(rec.ExpiresAt) {
		return Principal{}, ErrUnauthenticated
	}
	if rec.RevokedAt != nil && !now.Before(*rec.RevokedAt) {
		return Principal{}, ErrUnauthenticated
	}
	p, err := newKeyPrincipal(kind, rec.Tenant, rec.KeyID, rec.Scopes, rec.Environments, rec.IssuerRole)
	if err != nil {
		// 저장된 기록이 계약을 위반한다(scope 조합 등). 인증을 거절하고 원인은 호출자 로그로 남긴다.
		return Principal{}, fmt.Errorf("%w: invalid key record: %w", ErrUnauthenticated, err)
	}
	return p, nil
}
