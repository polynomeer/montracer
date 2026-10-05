// Package redact는 영속 저장 전에 telemetry에서 개인정보·secret을 제거한다 (D04 §03, ADR 0019).
//
// 처리 순서상 ingress의 tenant 주입·속성 검증 다음, quota·Kafka append 이전이다 (D02 §04).
// 패턴 탐지는 보완 수단이며 누락을 전제로 한다. 1차 방어는 key 규칙(필드 삭제)이다.
// redaction이 실패한 record는 저장하지 않고 거절 건수로만 보고한다. 원문은 어디에도 남기지 않는다.
package redact

import (
	"regexp"
	"strings"
	"unicode"
)

// Policy는 redaction 규칙 묶음이다. Version은 envelope의 policy_version으로 기록한다 (D02 §05).
type Policy struct {
	Version int64
	// DenyTokens: key를 정규화한 토큰(또는 인접 두 토큰을 붙인 형태) 중 하나라도 해당하면 필드를 삭제한다.
	// 정규화: camelCase 경계 분리, 소문자, 구분자 . _ - : / 공백.
	DenyTokens []string
	// DropPrefixes: 이 접두어로 시작하는 key는 삭제한다 (예: 바인딩된 SQL 파라미터 값).
	DropPrefixes []string
	// AllowKeys: deny 토큰에 걸려도 남길 key 접두어 (값에는 패턴 치환을 한다).
	AllowKeys []string
	// HeaderAllow: http.request/response.header.<name> 중 통과시킬 header 이름(소문자). 나머지는 삭제(기본 deny).
	HeaderAllow []string
	// URLKeys: query string·fragment를 제거할 URL 속성.
	URLKeys []string
	// DropKeys: 통째로 삭제할 속성 (예: url.query).
	DropKeys []string
	// SQLKeys: literal을 '?'로 바꿀 DB 문장 속성.
	SQLKeys []string
	// IPKeys: 축약할 IP 속성 (IPv4 /24, IPv6 /48).
	IPKeys []string
}

// DefaultPolicy는 D04 §03 표·최소 수집 원칙의 시작값이다. 세부 목록은 ADR 0019의 결정이다.
var DefaultPolicy = Policy{
	Version: 1,
	DenyTokens: []string{
		// 인증 정보: 필드 삭제, 예외 없음
		"authorization", "auth", "cookie", "password", "passwd", "pwd", "passphrase", "secret", "token",
		"apikey", "accesskey", "privatekey", "secretkey", "credential", "credentials", "session", "sessionid",
		"jwt", "otp", "pin", "cvv", "cvc",
		// 고객 식별자·민감 식별자: 기본 미수집
		"user", "username", "enduser", "customer", "email", "phone", "mobile", "ssn", "rrn",
		"card", "creditcard", "pan", "iban", "passport",
	},
	DropPrefixes: []string{"db.query.parameter.", "db.statement.parameter."},
	AllowKeys:    []string{"user_agent."},
	HeaderAllow:  []string{"content-type", "content-length", "accept", "accept-encoding", "user-agent", "x-request-id", "traceparent", "tracestate"},
	URLKeys:      []string{"url.full", "url.original", "http.url", "http.target", "url.path"},
	DropKeys:     []string{"url.query"},
	SQLKeys:      []string{"db.query.text", "db.statement"},
	// 최종 사용자 IP일 수 있는 key. 사설·loopback 주소는 인프라 식별자라 축약하지 않는다(scrub.go publicIP).
	IPKeys: []string{"client.address", "source.address", "http.client_ip", "net.peer.ip", "network.peer.address", "peer.ip", "http.request.header.x-forwarded-for"},
}

// 자유 텍스트 탐지 패턴. 치환 결과에 원문 일부를 남기지 않는다.
var (
	reEmail = regexp.MustCompile(`[A-Za-z0-9._%+\-]+@[A-Za-z0-9.\-]+\.[A-Za-z]{2,}`)
	reJWT   = regexp.MustCompile(`eyJ[A-Za-z0-9_\-]+\.[A-Za-z0-9_\-]+\.[A-Za-z0-9_\-]*`)
	// 인증 scheme 뒤 credential. basic은 base64 8자 이상일 때만 (일반 문장 "basic auth failed" 오탐 방지).
	reBearer = regexp.MustCompile(`(?i)\b(?:(?:bearer|token|digest|apikey)\s+[A-Za-z0-9._~+/=\-]{6,}|basic\s+[A-Za-z0-9+/]{8,}={0,2})`)
	reAPIKey = regexp.MustCompile(`\bmt[ai]_[0-9a-f]{16}_[A-Za-z0-9_\-]{43}\b`)
	// key=value·key: value 형태의 secret. key는 남기고 값만 치환한다.
	// key 앞 단어 경계는 요구하지 않는다(긴 문자열에 붙은 "…token=값"도 잡는다).
	// 따옴표 값(JSON·YAML)도 잡는다: {"password":"x"}, password='x', password: "x".
	reSecretKV = regexp.MustCompile(`(?i)(password|passwd|pwd|passphrase|secret|token|access_token|refresh_token|id_token|api[_\-]?key|access[_\-]?key|session(?:id)?|ssn|credential)("?\s*[=:]\s*)(?:"[^"]*"|'[^']*'|[^\s&;,"'<>]+)`)
	// Authorization 값은 scheme과 credential을 모두 지운다 (줄 끝 또는 따옴표까지).
	reAuthHeader = regexp.MustCompile(`(?i)(authorization|proxy-authorization)("?\s*[=:]\s*)("[^"]*"|[^\r\n"]+)`)
	// 주민·외국인등록번호: 앞 6자리는 YYMMDD 유효값, 성별 자리 1~8 (epoch ms 오탐 방지)
	reRRN     = regexp.MustCompile(`\b\d{2}(?:0[1-9]|1[0-2])(?:0[1-9]|[12]\d|3[01])-?[1-8]\d{6}\b`)
	reSSN     = regexp.MustCompile(`\b\d{3}-\d{2}-\d{4}\b`)
	rePhoneKR = regexp.MustCompile(`\b01[016789][-\s]?\d{3,4}[-\s]?\d{4}\b`)
	// 카드 후보: 구분자(공백·-)로 이어진 숫자 묶음. 경계 \b를 요구하지 않는다("card4111…").
	reCardRun = regexp.MustCompile(`\d(?:[ \-]?\d){11,}`)
	// 자유 텍스트 IP 후보 (net.ParseIP로 확정)
	// 이미 축약된 CIDR(…/24)도 함께 잡아 그대로 둔다(재처리 멱등성).
	reIPv4 = regexp.MustCompile(`\b(?:\d{1,3}\.){3}\d{1,3}(?:/\d{1,3})?\b`)
	reIPv6 = regexp.MustCompile(`[0-9A-Fa-f]{0,4}(?::[0-9A-Fa-f]{0,4}){2,7}`)
	// SQL literal
)

type compiled struct {
	version      int64
	deny         map[string]bool
	dropPrefixes []string
	allowKeys    []string
	headerAllow  map[string]bool
	url          map[string]bool
	drop         map[string]bool
	sql          map[string]bool
	ip           map[string]bool
}

func set(xs []string) map[string]bool {
	m := make(map[string]bool, len(xs))
	for _, x := range xs {
		m[strings.ToLower(x)] = true
	}
	return m
}

func lowerAll(xs []string) []string {
	out := make([]string, len(xs))
	for i, x := range xs {
		out[i] = strings.ToLower(x)
	}
	return out
}

// keyTokens는 key를 정규화한 토큰이다. camelCase 경계도 나눈다 ("userEmail" → user, email).
func keyTokens(key string) []string {
	var toks []string
	var cur []rune
	flush := func() {
		if len(cur) > 0 {
			toks = append(toks, strings.ToLower(string(cur)))
			cur = cur[:0]
		}
	}
	rs := []rune(strings.TrimSpace(key))
	for i, r := range rs {
		switch {
		case r == '.' || r == '_' || r == '-' || r == ':' || r == '/' || unicode.IsSpace(r):
			flush()
		case unicode.IsUpper(r) && i > 0 && (unicode.IsLower(rs[i-1]) || (i+1 < len(rs) && unicode.IsLower(rs[i+1]) && unicode.IsUpper(rs[i-1]))):
			flush()
			cur = append(cur, r)
		default:
			cur = append(cur, r)
		}
	}
	flush()
	return toks
}

func compile(p Policy) compiled {
	return compiled{
		version:      p.Version,
		deny:         set(p.DenyTokens),
		dropPrefixes: lowerAll(p.DropPrefixes),
		allowKeys:    lowerAll(p.AllowKeys),
		headerAllow:  set(p.HeaderAllow),
		url:          set(p.URLKeys),
		drop:         set(p.DropKeys),
		sql:          set(p.SQLKeys),
		ip:           set(p.IPKeys),
	}
}

// keyAction은 속성 key에 대한 결정이다.
type keyAction uint8

const (
	keepScrub keyAction = iota // 값에 패턴 치환
	drop                       // 필드 삭제
	scrubURL
	scrubSQL
	truncIP
)

const (
	reqHeaderPrefix  = "http.request.header."
	respHeaderPrefix = "http.response.header."
)

func (c compiled) action(key string) keyAction {
	k := strings.ToLower(strings.TrimSpace(key))
	if c.drop[k] {
		return drop
	}
	for _, p := range c.dropPrefixes {
		if strings.HasPrefix(k, p) {
			return drop
		}
	}
	for _, prefix := range []string{reqHeaderPrefix, respHeaderPrefix} {
		if strings.HasPrefix(k, prefix) {
			if c.headerAllow[strings.TrimPrefix(k, prefix)] {
				return keepScrub
			}
			return drop // header 기본 deny
		}
	}
	allowed := false
	for _, p := range c.allowKeys {
		if strings.HasPrefix(k, p) {
			allowed = true
		}
	}
	if !allowed {
		toks := keyTokens(key)
		for i, tok := range toks {
			// 단일 토큰 또는 인접 두 토큰을 붙인 형태 (api+key, credit+card, private+key)
			if c.deny[tok] || (i+1 < len(toks) && c.deny[tok+toks[i+1]]) {
				return drop
			}
		}
	}
	switch {
	case c.url[k]:
		return scrubURL
	case c.sql[k]:
		return scrubSQL
	case c.ip[k]:
		return truncIP
	}
	return keepScrub
}
