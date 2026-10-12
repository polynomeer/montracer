package notify

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// 목적지 검증과 SSRF 방어 (D02 §17, D04 §03 "webhook SSRF: IP 검증, DNS 재검증, private IPv4·IPv6·redirect").
//   - 저장할 때: HTTPS, host 필수, userinfo 없음, IP literal이면 공개 주소만(ValidateURL).
//   - 보낼 때: 이름을 풀어 실제로 연결하는 주소를 dialer Control에서 검사한다. 저장 때 검사한 이름이 사설 주소로 바뀌어도
//     (DNS rebinding) 연결 직전 주소로 막힌다. redirect는 따라가지 않고, 환경 변수 proxy를 쓰지 않는다(검사를 우회하므로).

// 영구 실패 사유 (last_error). 다시 보내도 같은 결과라 재시도하지 않는다.
const (
	ErrCodeBlocked = "destination_blocked" // 사설·예약 주소로 연결하려 했다
	ErrCodeInvalid = "invalid_destination" // URL 형식
)

// 일시 실패 사유 (재시도한다).
const (
	ErrCodeTimeout  = "timeout"
	ErrCodeConnect  = "connect_failed"
	ErrCodeTLS      = "tls_failed"
	ErrCodeStatus   = "http_status"   // 2xx 아님
	ErrCodeRedirect = "redirect"      // 3xx(따라가지 않는다)
	ErrCodeRequest  = "request_error" // 그 밖의 전송 오류
)

// blocked는 공개 인터넷이 아닌 주소 범위다(net/netip 분류에 더해).
var blocked = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"),       // this network
	netip.MustParsePrefix("100.64.0.0/10"),   // CGNAT
	netip.MustParsePrefix("192.0.0.0/24"),    // IETF protocol assignments
	netip.MustParsePrefix("192.0.2.0/24"),    // TEST-NET-1
	netip.MustParsePrefix("198.18.0.0/15"),   // benchmarking
	netip.MustParsePrefix("198.51.100.0/24"), // TEST-NET-2
	netip.MustParsePrefix("203.0.113.0/24"),  // TEST-NET-3
	netip.MustParsePrefix("240.0.0.0/4"),     // reserved, broadcast
	netip.MustParsePrefix("64:ff9b::/96"),    // NAT64(내부 IPv4로 번역될 수 있다)
	netip.MustParsePrefix("64:ff9b:1::/48"),  // local-use NAT64
	netip.MustParsePrefix("2001:db8::/32"),   // documentation
	netip.MustParsePrefix("2002::/16"),       // 6to4(내부 IPv4를 품을 수 있다)
	netip.MustParsePrefix("100::/64"),        // discard
	netip.MustParsePrefix("192.88.99.0/24"),  // 6to4 relay anycast
	netip.MustParsePrefix("2001::/32"),       // Teredo(내부 IPv4를 품을 수 있다)
	netip.MustParsePrefix("2001:2::/48"),     // benchmarking
	netip.MustParsePrefix("2001:10::/28"),    // ORCHID
	netip.MustParsePrefix("2001:20::/28"),    // ORCHIDv2
	netip.MustParsePrefix("3fff::/20"),       // documentation
}

// globalUnicast6은 IPv6에서 공개 인터넷 unicast가 배정되는 범위다. 그 밖(IPv4-compatible ::/96, site-local fec0::/10 등)은 막는다.
var globalUnicast6 = netip.MustParsePrefix("2000::/3")

// PublicAddr는 공개 인터넷 unicast 주소인지 본다. loopback·사설·link-local(cloud metadata 169.254.169.254 포함)·multicast·예약은 아니다.
func PublicAddr(a netip.Addr) bool {
	a = a.Unmap() // ::ffff:10.0.0.1 같은 IPv4-mapped도 IPv4로 본다
	if !a.IsValid() || a.IsLoopback() || a.IsPrivate() || a.IsLinkLocalUnicast() || a.IsLinkLocalMulticast() ||
		a.IsInterfaceLocalMulticast() || a.IsMulticast() || a.IsUnspecified() {
		return false
	}
	if a.Is6() && !globalUnicast6.Contains(a) {
		return false // IPv6는 허용 목록(2000::/3) 안에서만
	}
	for _, p := range blocked {
		if p.Contains(a) {
			return false
		}
	}
	return true
}

// numericHost는 IP literal이 아닌 숫자 표기(127.1, 2130706433, 0x7f000001)다. resolver에 따라 내부 주소로 풀리므로 저장 때 거절한다.
var numericHost = regexp.MustCompile(`^(0x[0-9a-f]+|[0-9.]+)$`)

// ValidateURL은 저장할 webhook URL을 검사한다. 이름은 여기서 풀지 않는다(보낼 때 연결 주소로 검사한다).
func ValidateURL(raw string) error { return validateURL(raw, PublicAddr) }

func validateURL(raw string, allow func(netip.Addr) bool) error {
	if len(raw) > 2048 {
		return errors.New("must be at most 2048 bytes")
	}
	u, err := url.Parse(raw)
	if err != nil {
		return errors.New("must be an absolute https URL")
	}
	switch {
	case u.Scheme != "https":
		return errors.New("must use https")
	case u.Host == "" || u.Hostname() == "":
		return errors.New("must have a host")
	case u.User != nil:
		return errors.New("must not contain credentials")
	case u.Fragment != "" || strings.Contains(raw, "#"):
		return errors.New("must not contain a fragment")
	}
	if p := u.Port(); p != "" {
		if n, err := strconv.Atoi(p); err != nil || n < 1 || n > 65535 {
			return errors.New("has an invalid port")
		}
	}
	host := strings.TrimSuffix(strings.ToLower(u.Hostname()), ".")
	if host == "localhost" || strings.HasSuffix(host, ".localhost") || strings.HasSuffix(host, ".internal") || strings.HasSuffix(host, ".local") {
		return errors.New("must be a public host")
	}
	if a, err := netip.ParseAddr(host); err == nil {
		if !allow(a) {
			return errors.New("must be a public address")
		}
	} else if numericHost.MatchString(host) {
		return errors.New("must be a host name or a canonical IP address")
	}
	return nil
}

// RedactURL은 화면·감사에 보여줄 URL이다: scheme과 host만 남긴다(path·query에 receiver token이 있는 경우가 많다).
func RedactURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return ""
	}
	s := u.Scheme + "://" + u.Host
	if u.Path != "" && u.Path != "/" || u.RawQuery != "" {
		s += "/…"
	}
	return s
}

// ClientOptions는 전송 client 설정이다.
type ClientOptions struct {
	Timeout time.Duration // 요청 하나(연결·TLS·응답 header·본문 읽기) 상한. 기본 10초
	// allow는 연결 주소 검사다. nil이면 PublicAddr. 시험만 바꾼다(httptest 서버는 loopback이다).
	allow   func(netip.Addr) bool
	rootCAs *x509.CertPool
}

// Client는 SSRF를 막는 webhook client다.
type Client struct {
	http  *http.Client
	allow func(netip.Addr) bool
}

// errBlocked는 dialer가 막은 연결이다.
var errBlocked = errors.New("notify: destination address is not public")

// NewClient는 client를 만든다.
func NewClient(o ClientOptions) *Client {
	if o.Timeout <= 0 {
		o.Timeout = 10 * time.Second
	}
	allow := o.allow
	if allow == nil {
		allow = PublicAddr
	}
	dialer := &net.Dialer{
		Timeout: 5 * time.Second,
		// 연결 직전의 실제 주소를 검사한다(DNS rebinding·이름이 여러 주소로 풀리는 경우 모두)
		Control: func(_, address string, _ syscall.RawConn) error {
			ap, err := netip.ParseAddrPort(address)
			if err != nil || !allow(ap.Addr()) {
				return errBlocked
			}
			return nil
		},
	}
	transport := &http.Transport{
		Proxy:                 nil, // 환경 변수 proxy를 쓰지 않는다(연결 주소 검사를 우회한다)
		DialContext:           dialer.DialContext,
		TLSClientConfig:       &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: o.rootCAs},
		TLSHandshakeTimeout:   5 * time.Second,
		ResponseHeaderTimeout: o.Timeout,
		MaxIdleConns:          50,
		IdleConnTimeout:       90 * time.Second,
		DisableCompression:    true,
	}
	return &Client{
		allow: allow,
		http: &http.Client{
			Transport: transport,
			Timeout:   o.Timeout,
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse // redirect를 따라가지 않는다(3xx는 실패)
			},
		},
	}
}

// Result는 전송 시도 하나의 결과다. ErrCode가 비어 있으면 성공(2xx)이다.
type Result struct {
	StatusCode int
	ErrCode    string
	Permanent  bool // 다시 보내도 같다(목적지 차단·형식)
}

// Deliver는 envelope을 서명해 보낸다. 응답 본문은 4KiB까지만 읽고 버린다(내용을 저장·로그하지 않는다).
func (c *Client) Deliver(ctx context.Context, rawURL string, sec Secret, eventID string, body []byte, attempt int, now time.Time) Result {
	if err := validateURL(rawURL, c.allow); err != nil {
		return Result{ErrCode: ErrCodeInvalid, Permanent: true}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, rawURL, bytes.NewReader(body))
	if err != nil {
		return Result{ErrCode: ErrCodeInvalid, Permanent: true}
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "Montracer-Webhook/1")
	req.Header.Set(HeaderEventID, eventID)
	req.Header.Set(HeaderTimestamp, strconv.FormatInt(now.Unix(), 10))
	req.Header.Set(HeaderKeyID, sec.KeyID)
	req.Header.Set(HeaderSignature, Sign(sec.Key, now, eventID, body))
	req.Header.Set(HeaderAttempt, strconv.Itoa(attempt))
	resp, err := c.http.Do(req)
	if err != nil {
		return classifyError(err)
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))
	switch {
	case resp.StatusCode >= 200 && resp.StatusCode < 300:
		return Result{StatusCode: resp.StatusCode}
	case resp.StatusCode >= 300 && resp.StatusCode < 400:
		return Result{StatusCode: resp.StatusCode, ErrCode: ErrCodeRedirect}
	default:
		return Result{StatusCode: resp.StatusCode, ErrCode: ErrCodeStatus}
	}
}

func classifyError(err error) Result {
	var (
		netErr  net.Error
		certErr *tls.CertificateVerificationError
	)
	switch {
	case errors.Is(err, errBlocked):
		return Result{ErrCode: ErrCodeBlocked, Permanent: true}
	case errors.As(err, &certErr), strings.Contains(err.Error(), "tls:"):
		return Result{ErrCode: ErrCodeTLS}
	case errors.Is(err, context.DeadlineExceeded), errors.As(err, &netErr) && netErr.Timeout():
		return Result{ErrCode: ErrCodeTimeout}
	case errors.As(err, new(*net.OpError)), errors.As(err, new(*net.DNSError)):
		return Result{ErrCode: ErrCodeConnect}
	default:
		return Result{ErrCode: ErrCodeRequest}
	}
}

// String은 로그용이다(URL·본문 없음).
func (r Result) String() string {
	if r.ErrCode == "" {
		return fmt.Sprintf("delivered (%d)", r.StatusCode)
	}
	return fmt.Sprintf("%s (%d, permanent=%v)", r.ErrCode, r.StatusCode, r.Permanent)
}
