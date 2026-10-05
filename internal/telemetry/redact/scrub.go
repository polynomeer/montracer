package redact

import (
	"net"
	"strings"
)

// Kind는 치환 사유다. metric label로 쓰며 값 내용을 담지 않는다.
type Kind string

const (
	KindDeniedKey Kind = "denied_key"
	KindHeader    Kind = "header"
	KindURLQuery  Kind = "url_query"
	KindSQL       Kind = "sql_literal"
	KindIP        Kind = "ip"
	KindEmail     Kind = "email"
	KindCard      Kind = "payment_card"
	KindNationID  Kind = "national_id"
	KindPhone     Kind = "phone"
	KindJWT       Kind = "jwt"
	KindBearer    Kind = "bearer"
	KindAPIKey    Kind = "api_key"
	KindSecretKV  Kind = "secret"
	KindEncoded   Kind = "encoded" // 인코딩을 풀었을 때만 탐지된 값 → 값 전체 치환
	KindNonString Kind = "non_string"
)

func mask(k Kind) string { return "[REDACTED:" + string(k) + "]" }

// counter는 치환 건수를 센다.
type counter map[Kind]int

func sum(n counter) int {
	t := 0
	for _, v := range n {
		t += v
	}
	return t
}

// scrubText는 자유 텍스트에서 패턴을 치환한다.
//
// 인코딩 우회 대응: % 인코딩을 최대 3회 연쇄로 풀고 각 단계에서 @도 풀어 본다.
// 풀어 본 문자열에서 원문보다 많이 탐지되면, 원문 일부를 남기지 않도록 값 전체를 치환한다.
func scrubText(s string, n counter) string {
	if s == "" {
		return s
	}
	base := counter{}
	out := scrubPatterns(s, base)
	baseCount := sum(base)
	for _, d := range decodings(s) {
		probe := counter{}
		scrubPatterns(d, probe)
		if sum(probe) > baseCount {
			n[KindEncoded]++
			return mask(KindEncoded)
		}
	}
	for k, v := range base {
		n[k] += v
	}
	return out
}

const escapedAt = "\\u0040"

// decodings는 해석본 목록이다. %가 없고 @도 없으면 비어 있다(대부분의 값은 이 빠른 경로).
func decodings(s string) []string {
	if !strings.Contains(s, "%") && !strings.Contains(s, escapedAt) {
		return nil
	}
	var out []string
	cur := s
	for range 3 {
		d := percentDecode(cur)
		if d == cur {
			break
		}
		out = append(out, d)
		cur = d
	}
	for _, v := range append([]string{s}, out...) {
		if strings.Contains(v, escapedAt) {
			out = append(out, strings.ReplaceAll(v, escapedAt, "@"))
		}
	}
	return out
}

// percentDecode는 유효한 %XX만 풀고 잘못된 %는 그대로 둔다. '+'는 공백으로 바꾸지 않는다.
func percentDecode(s string) string {
	if !strings.Contains(s, "%") {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); i++ {
		if s[i] == '%' && i+2 < len(s) && isHex(s[i+1]) && isHex(s[i+2]) {
			b.WriteByte(unhex(s[i+1])<<4 | unhex(s[i+2]))
			i += 2
			continue
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

func isHex(c byte) bool {
	return (c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')
}

func unhex(c byte) byte {
	switch {
	case c >= '0' && c <= '9':
		return c - '0'
	case c >= 'a' && c <= 'f':
		return c - 'a' + 10
	default:
		return c - 'A' + 10
	}
}

type replacer interface {
	ReplaceAllStringFunc(string, func(string) string) string
}

func replace(s string, re replacer, k Kind, n counter) string {
	return re.ReplaceAllStringFunc(s, func(string) string { n[k]++; return mask(k) })
}

func countDigits(s string) int {
	c := 0
	for i := 0; i < len(s); i++ {
		if s[i] >= '0' && s[i] <= '9' {
			c++
		}
	}
	return c
}

// 대소문자 무시 정규식은 비싸므로, 소문자 키워드가 있을 때만 실행한다(사전 필터).
var (
	authKeywords   = []string{"authorization"}
	schemeKeywords = []string{"bearer", "token", "digest", "apikey", "basic"}
	secretKeywords = []string{"password", "passwd", "pwd", "passphrase", "secret", "token", "api", "access", "session", "ssn", "credential"}
)

func containsAny(lower string, words []string) bool {
	for _, w := range words {
		if strings.Contains(lower, w) {
			return true
		}
	}
	return false
}

func scrubPatterns(s string, n counter) string {
	lower := strings.ToLower(s)
	// 순서: 형식이 구체적인 것부터 (JWT·key·인증 header가 secret=값보다 먼저 통째로 사라지게)
	if strings.Contains(s, "eyJ") {
		s = replace(s, reJWT, KindJWT, n)
	}
	if strings.Contains(s, "mt") {
		s = replace(s, reAPIKey, KindAPIKey, n)
	}
	if containsAny(lower, authKeywords) {
		s = reAuthHeader.ReplaceAllStringFunc(s, func(m string) string {
			sub := reAuthHeader.FindStringSubmatch(m)
			n[KindBearer]++
			return sub[1] + sub[2] + mask(KindBearer)
		})
	}
	if containsAny(lower, schemeKeywords) {
		s = replace(s, reBearer, KindBearer, n)
	}
	if containsAny(lower, secretKeywords) {
		s = reSecretKV.ReplaceAllStringFunc(s, func(m string) string {
			sub := reSecretKV.FindStringSubmatch(m)
			n[KindSecretKV]++
			return sub[1] + sub[2] + mask(KindSecretKV)
		})
	}
	if strings.Contains(s, "@") {
		s = replace(s, reEmail, KindEmail, n)
	}
	if countDigits(s) >= 7 {
		s = replace(s, reRRN, KindNationID, n)
		s = replace(s, reSSN, KindNationID, n)
		s = replace(s, rePhoneKR, KindPhone, n)
		s = reCardRun.ReplaceAllStringFunc(s, func(m string) string {
			if !containsCard(m) {
				return m
			}
			n[KindCard]++
			return mask(KindCard)
		})
		s = reIPv4.ReplaceAllStringFunc(s, func(m string) string {
			if strings.Contains(m, "/") {
				return m // 이미 축약된 CIDR
			}
			return truncatePublic(m, n)
		})
	}
	if strings.Count(s, ":") >= 2 {
		s = reIPv6.ReplaceAllStringFunc(s, func(m string) string {
			if net.ParseIP(m) == nil {
				return m
			}
			return truncatePublic(m, n)
		})
	}
	return s
}

// containsCard는 숫자 묶음에서 이어진 묶음 조합(자릿수 13~19)이 카드번호(BIN 3~6, Luhn)인지 본다.
// 앞에 다른 숫자가 붙은 경우("qty 2 4111 1111 1111 1111")도 잡는다.
func containsCard(run string) bool {
	groups := strings.FieldsFunc(run, func(r rune) bool { return r == ' ' || r == '-' })
	for i := range groups {
		digits := ""
		for j := i; j < len(groups); j++ {
			digits += groups[j]
			if len(digits) > 19 {
				break
			}
			if len(digits) >= 13 && digits[0] >= '3' && digits[0] <= '6' && luhn(digits) {
				return true
			}
		}
	}
	return false
}

// luhn은 숫자열이 카드번호 검증 숫자를 만족하는지 본다.
func luhn(digits string) bool {
	total := 0
	for i := len(digits) - 1; i >= 0; i-- {
		d := int(digits[i] - '0')
		if (len(digits)-1-i)%2 == 1 {
			d *= 2
			if d > 9 {
				d -= 9
			}
		}
		total += d
	}
	return total%10 == 0
}

// cleanURL은 userinfo·query·fragment를 지우고 나머지(path)에 패턴 치환을 한다.
// url.Parse 실패와 무관하게 문자열 연산으로 userinfo를 먼저 제거한다.
func cleanURL(s string, n counter) string {
	cut := s
	if i := strings.IndexAny(cut, "?#"); i >= 0 {
		cut = cut[:i]
		n[KindURLQuery]++
	}
	if i := strings.Index(cut, "://"); i >= 0 {
		rest := cut[i+3:]
		authority := rest
		if j := strings.IndexByte(rest, '/'); j >= 0 {
			authority = rest[:j]
		}
		if at := strings.LastIndexByte(authority, '@'); at >= 0 {
			cut = cut[:i+3] + rest[at+1:]
			n[KindSecretKV]++
		}
	}
	return scrubText(cut, n)
}

// cleanSQL은 literal을 '?'로 바꾼다 (D04 §03 "parameterized query 형태만").
// 처리: '…'(”·\' escape), 닫히지 않은 '…(잘린 문장), $tag$…$tag$, 0x hex, 숫자.
// 보존: placeholder($1, :name, ?), 식별자 안의 숫자(table1), 식별자 형태의 "…".
func cleanSQL(s string, n counter) string {
	var b strings.Builder
	b.Grow(len(s))
	prevIdent := false // 직전 문자가 식별자 문자(문자·숫자·_·$·:)인지
	for i := 0; i < len(s); {
		c := s[i]
		switch {
		case c == '\'':
			j := i + 1
			for j < len(s) {
				if s[j] == '\\' && j+1 < len(s) {
					j += 2
					continue
				}
				if s[j] == '\'' {
					if j+1 < len(s) && s[j+1] == '\'' {
						j += 2
						continue
					}
					break
				}
				j++
			}
			b.WriteByte('?')
			n[KindSQL]++
			i = j + 1 // 닫히지 않았으면 끝까지 소비
			prevIdent = false
		case c == '"':
			j := strings.IndexByte(s[i+1:], '"')
			if j < 0 {
				b.WriteByte('?')
				n[KindSQL]++
				i = len(s)
				continue
			}
			inner := s[i+1 : i+1+j]
			if isIdentifier(inner) {
				b.WriteString(s[i : i+j+2]) // PostgreSQL 식별자
			} else {
				b.WriteByte('?') // MySQL 문자열 literal
				n[KindSQL]++
			}
			i += j + 2
			prevIdent = false
		case c == '$' && i+1 < len(s) && (s[i+1] == '$' || isIdentStart(s[i+1])) && dollarTag(s[i:]) != "":
			tag := dollarTag(s[i:])
			end := strings.Index(s[i+len(tag):], tag)
			b.WriteByte('?')
			n[KindSQL]++
			if end < 0 {
				i = len(s)
			} else {
				i += len(tag) + end + len(tag)
			}
			prevIdent = false
		case c >= '0' && c <= '9' && !prevIdent:
			j := i
			if c == '0' && i+1 < len(s) && (s[i+1] == 'x' || s[i+1] == 'X') {
				j = i + 2
				for j < len(s) && isHex(s[j]) {
					j++
				}
			} else {
				for j < len(s) && ((s[j] >= '0' && s[j] <= '9') || s[j] == '.') {
					j++
				}
			}
			b.WriteByte('?')
			n[KindSQL]++
			i = j
			prevIdent = false
		default:
			b.WriteByte(c)
			prevIdent = isIdentStart(c) || (c >= '0' && c <= '9') || c == '$' || c == ':'
			i++
		}
	}
	return b.String()
}

func isIdentStart(c byte) bool {
	return c == '_' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
}

func isIdentifier(s string) bool {
	if s == "" || !isIdentStart(s[0]) {
		return false
	}
	for i := 1; i < len(s); i++ {
		c := s[i]
		if !isIdentStart(c) && (c < '0' || c > '9') && c != '$' {
			return false
		}
	}
	return true
}

// dollarTag는 s가 $tag$ 또는 $$로 시작하면 그 tag를 돌려준다. $1 같은 placeholder는 아니다.
func dollarTag(s string) string {
	if strings.HasPrefix(s, "$$") {
		return "$$"
	}
	j := 1
	for j < len(s) && (isIdentStart(s[j]) || (s[j] >= '0' && s[j] <= '9')) {
		j++
	}
	if j > 1 && j < len(s) && s[j] == '$' && isIdentStart(s[1]) {
		return s[:j+1]
	}
	return ""
}

// truncateIP는 IP 속성 값을 처리한다. 목록(XFF)·port·bracket·zone을 풀어 주소마다 판정한다.
// 공인 IP는 IPv4 /24, IPv6 /48로 축약하고, 사설·loopback·link-local은 인프라 식별자라 그대로 둔다.
// 해석할 수 없는 값은 지운다 (D04 §03 "제거 또는 축약").
func truncateIP(s string, n counter) string {
	parts := strings.Split(s, ",")
	for i, p := range parts {
		p = strings.TrimSpace(p)
		if _, _, err := net.ParseCIDR(p); err == nil {
			parts[i] = p // 이미 축약된 값 (재처리 멱등성)
			continue
		}
		host := p
		if h, _, err := net.SplitHostPort(p); err == nil {
			host = h
		}
		host = strings.Trim(host, "[]")
		if z := strings.IndexByte(host, '%'); z >= 0 {
			host = host[:z]
		}
		if net.ParseIP(host) == nil {
			n[KindIP]++
			parts[i] = mask(KindIP)
			continue
		}
		parts[i] = truncatePublic(host, n)
	}
	return strings.Join(parts, ", ")
}

func truncatePublic(s string, n counter) string {
	ip := net.ParseIP(s)
	if ip == nil || ip.IsPrivate() || ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsUnspecified() {
		return s
	}
	n[KindIP]++
	if v4 := ip.To4(); v4 != nil {
		return v4.Mask(net.CIDRMask(24, 32)).String() + "/24"
	}
	return ip.Mask(net.CIDRMask(48, 128)).String() + "/48"
}
