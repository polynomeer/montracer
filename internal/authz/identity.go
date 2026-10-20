package authz

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"unicode/utf8"
)

// MaxOIDCSubject는 받는 OIDC sub claim의 최대 길이다. OIDC Core는 255 ASCII까지 허용하지만 user_id 상한(256)에
// issuer 구분자를 더해야 해서 230으로 둔다. 넘으면 로그인을 거절한다(자르면 다른 사람과 같아질 수 있다).
const MaxOIDCSubject = 230

// OIDCUserID는 OIDC 사용자의 안정적인 user_id다: "oidc:" + hex(SHA-256(issuer))[:12] + ":" + sub (ADR 0064 §2).
//   - sub는 issuer 안에서만 고유하다(OIDC Core §2). issuer를 넣어 IdP 둘이 같은 sub를 주어도 섞이지 않게 한다.
//   - issuer URL 전체 대신 hash 앞부분을 넣어 길이를 묶는다. email은 바뀔 수 있고 재사용되므로 쓰지 않는다.
//   - membership·API key 발급자(issued_by)·감사 actor_id가 이 값을 쓴다.
func OIDCUserID(issuer, subject string) (string, error) {
	if issuer == "" || subject == "" {
		return "", errors.New("authz: oidc issuer and subject are required")
	}
	if len(subject) > MaxOIDCSubject || !utf8.ValidString(subject) {
		return "", errors.New("authz: oidc subject too long or not utf-8")
	}
	sum := sha256.Sum256([]byte(issuer))
	return "oidc:" + hex.EncodeToString(sum[:6]) + ":" + subject, nil
}
