package session

import (
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
)

// ManagerFromEnv는 환경 변수로 Manager를 만든다. MONTRACER_SESSION_KEY_HEX가 비면 nil이다(session 끔 — bearer key만).
// 잘못된 값은 기동 오류다. 변경 요청의 Origin은 MONTRACER_PUBLIC_WEB_URL의 origin과 같아야 한다(비면 Origin을 보지 않는다).
// query-api와 control-api는 같은 key를 써야 한다(CSRF token이 같아야 한다).
func ManagerFromEnv(getenv func(string) string, store Store) (*Manager, error) {
	v := getenv("MONTRACER_SESSION_KEY_HEX")
	if v == "" {
		return nil, nil
	}
	key, err := hex.DecodeString(v)
	if err != nil || len(key) < 32 {
		return nil, errors.New("MONTRACER_SESSION_KEY_HEX must be hex of at least 32 bytes")
	}
	origin := ""
	if web := getenv("MONTRACER_PUBLIC_WEB_URL"); web != "" {
		u, err := url.Parse(web)
		if err != nil || u.Scheme == "" || u.Host == "" {
			return nil, errors.New("MONTRACER_PUBLIC_WEB_URL must be an absolute URL")
		}
		origin = u.Scheme + "://" + u.Host
	}
	return New(Config{Store: store, Key: key, AllowedOrigin: origin})
}

// ProviderFromEnv는 OIDC client를 만든다. MONTRACER_OIDC_ISSUER가 비면 nil이다(로그인 경로 404).
//
//	MONTRACER_OIDC_ISSUER, MONTRACER_OIDC_CLIENT_ID, MONTRACER_OIDC_CLIENT_SECRET(선택, confidential client),
//	MONTRACER_OIDC_REDIRECT_URL(고정 callback: https://<web>/api/v1/auth/callback)
func ProviderFromEnv(getenv func(string) string) (Provider, error) {
	issuer := getenv("MONTRACER_OIDC_ISSUER")
	if issuer == "" {
		return nil, nil
	}
	redirect := getenv("MONTRACER_OIDC_REDIRECT_URL")
	u, err := url.Parse(redirect)
	if err != nil || u.Scheme == "" || u.Host == "" || u.RawQuery != "" || u.Fragment != "" {
		return nil, errors.New("MONTRACER_OIDC_REDIRECT_URL must be an absolute URL without query or fragment")
	}
	p, err := NewOIDCProvider(OIDCConfig{Issuer: issuer, ClientID: getenv("MONTRACER_OIDC_CLIENT_ID"),
		ClientSecret: getenv("MONTRACER_OIDC_CLIENT_SECRET"), RedirectURL: redirect}, nil)
	if err != nil {
		return nil, fmt.Errorf("oidc: %w", err)
	}
	return p, nil
}
