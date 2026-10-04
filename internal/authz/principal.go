package authz

import (
	"context"
	"errors"
	"fmt"
)

// Kind는 principal의 인증 경로다.
type Kind uint8

const (
	kindUnknown Kind = iota
	// KindUser는 OIDC session으로 인증한 사람이다.
	KindUser
	// KindAPIKey는 관리·조회 API용 scoped bearer key다.
	KindAPIKey
	// KindIngestKey는 OTLP 수집 전용 key다. query·관리 API에 사용할 수 없다.
	KindIngestKey
)

func (k Kind) String() string {
	switch k {
	case KindUser:
		return "user"
	case KindAPIKey:
		return "api_key"
	case KindIngestKey:
		return "ingest_key"
	default:
		return "unknown"
	}
}

// Principal은 인증이 끝난 주체다. 필드는 비공개이며 이 패키지의 생성자로만 만든다.
// zero value는 미인증으로 취급한다.
type Principal struct {
	kind    Kind
	tenant  TenantID
	subject string
	role    Role            // KindUser
	scopes  map[Action]bool // KindAPIKey, KindIngestKey
	envs    map[string]bool // key의 environment scope. 비어 있으면 제한 없음(API key만 허용)
	stepUp  bool            // KindUser, 최근 MFA step-up 여부
}

const (
	maxSubjectLen     = 256
	maxEnvironmentLen = 64
)

// NewUserPrincipal은 OIDC 인증 결과로 사람 principal을 만든다.
// tenant는 인증된 membership 선택에서 도출한 값이어야 한다 (D02 §12).
func NewUserPrincipal(tenant TenantID, subject string, role Role, stepUpVerified bool) (Principal, error) {
	if err := validateIdentity(tenant, subject); err != nil {
		return Principal{}, err
	}
	if !role.Valid() {
		return Principal{}, fmt.Errorf("authz: unknown role %q", role)
	}
	return Principal{kind: KindUser, tenant: tenant, subject: subject, role: role, stepUp: stepUpVerified}, nil
}

// newKeyPrincipal은 저장된 key 기록으로 principal을 만든다. Authenticate에서만 호출한다.
func newKeyPrincipal(kind Kind, tenant TenantID, keyID string, scopes []Action, environments []string) (Principal, error) {
	if err := validateIdentity(tenant, keyID); err != nil {
		return Principal{}, err
	}
	if err := validateKeyScopes(kind, scopes); err != nil {
		return Principal{}, err
	}
	if err := validateEnvironments(kind, environments); err != nil {
		return Principal{}, err
	}
	envs := make(map[string]bool, len(environments))
	for _, e := range environments {
		envs[e] = true
	}
	return Principal{kind: kind, tenant: tenant, subject: keyID, scopes: toSet(scopes), envs: envs}, nil
}

// validateEnvironments: ingest key는 environment scope가 필수다 (D04 §02).
// API key의 environment 제한은 선택이며, 비어 있으면 조직 전체다 (MVP 조직 단위 RBAC, D04 §01).
func validateEnvironments(kind Kind, environments []string) error {
	if kind == KindIngestKey && len(environments) == 0 {
		return errors.New("authz: ingest key must have at least one environment")
	}
	for _, e := range environments {
		if e == "" || len(e) > maxEnvironmentLen {
			return fmt.Errorf("authz: environment must be 1..%d bytes", maxEnvironmentLen)
		}
	}
	return nil
}

func validateIdentity(tenant TenantID, subject string) error {
	if tenant.IsZero() {
		return errors.New("authz: tenant is required")
	}
	if subject == "" || len(subject) > maxSubjectLen {
		return errors.New("authz: subject must be 1..256 bytes")
	}
	return nil
}

// validateKeyScopes는 key 종류와 scope의 조합을 검사한다.
// ingest key는 ingest.* 만, API key는 ingest.* 를 제외한 action만 가질 수 있다.
func validateKeyScopes(kind Kind, scopes []Action) error {
	if len(scopes) == 0 {
		return errors.New("authz: key must have at least one scope")
	}
	for _, a := range scopes {
		switch kind {
		case KindIngestKey:
			if !isIngestAction(a) {
				return fmt.Errorf("authz: ingest key cannot hold scope %q", a)
			}
		case KindAPIKey:
			if isIngestAction(a) {
				return fmt.Errorf("authz: api key cannot hold ingest scope %q", a)
			}
			if stepUpActions[a] {
				return fmt.Errorf("authz: api key cannot hold step-up scope %q", a)
			}
		default:
			return errors.New("authz: not a key kind")
		}
	}
	return nil
}

// KeyIssuance는 검증을 통과한 발급 요청이다. Tenant는 항상 issuer의 tenant이며
// 호출자가 요청 body 등에서 고를 수 없다.
type KeyIssuance struct {
	Tenant       TenantID
	IssuedBy     string
	Kind         Kind
	Scopes       []Action
	Environments []string
}

// ValidateKeyIssuance는 issuer가 주어진 scope로 key를 발급할 수 있는지 검사한다.
// key는 발급자 권한보다 강해질 수 없고, 발급 자체는 keys.manage(step-up 필요)를 요구한다 (D04 §01~02).
// ingest key의 ingest.* scope는 keys.manage 권한으로 발급한다.
func ValidateKeyIssuance(issuer Principal, kind Kind, scopes []Action, environments []string) (KeyIssuance, error) {
	if err := Authorize(issuer, KeysManage); err != nil {
		return KeyIssuance{}, err
	}
	if err := validateKeyScopes(kind, scopes); err != nil {
		return KeyIssuance{}, err
	}
	if err := validateEnvironments(kind, environments); err != nil {
		return KeyIssuance{}, err
	}
	if kind == KindAPIKey {
		for _, a := range scopes {
			if !issuer.allows(a) {
				return KeyIssuance{}, fmt.Errorf("%w: issuer lacks scope %q", ErrForbidden, a)
			}
		}
	}
	return KeyIssuance{
		Tenant:       issuer.Tenant(),
		IssuedBy:     issuer.Subject(),
		Kind:         kind,
		Scopes:       append([]Action(nil), scopes...),
		Environments: append([]string(nil), environments...),
	}, nil
}

// Kind는 인증 경로를 반환한다.
func (p Principal) Kind() Kind { return p.kind }

// Tenant는 인증된 tenant를 반환한다. 권한 판단의 유일한 tenant 원천이다.
func (p Principal) Tenant() TenantID { return p.tenant }

// Subject는 사용자 ID 또는 key ID다. 감사 로그 actor로 쓴다.
func (p Principal) Subject() string { return p.subject }

// Authenticated는 생성자로 만든 principal인지 보고한다.
func (p Principal) Authenticated() bool { return p.kind != kindUnknown && !p.tenant.IsZero() }

func (p Principal) allows(a Action) bool {
	switch p.kind {
	case KindUser:
		return !isIngestAction(a) && p.role.Grants(a)
	case KindAPIKey, KindIngestKey:
		return p.scopes[a]
	default:
		return false
	}
}

// Authorize는 principal이 action을 수행할 수 있는지 검사한다.
func Authorize(p Principal, a Action) error {
	if !p.Authenticated() {
		return ErrUnauthenticated
	}
	if !p.allows(a) {
		return ErrForbidden
	}
	if stepUpActions[a] && !(p.kind == KindUser && p.stepUp) {
		return ErrStepUpRequired
	}
	return nil
}

// AllowsEnvironment는 principal이 environment에 접근·기록할 수 있는지 보고한다.
// 사람은 MVP에서 조직 단위다(environment 제한은 G1 F08). key는 저장된 environment scope를 따른다.
func (p Principal) AllowsEnvironment(env string) bool {
	if !p.Authenticated() || env == "" {
		return false
	}
	if p.kind == KindUser || (p.kind == KindAPIKey && len(p.envs) == 0) {
		return true
	}
	return p.envs[env]
}

// CheckRequestTenant는 요청이 지정한 tenant(URL 경로 등)가 principal의 tenant와 같은지 검사한다.
// 불일치는 403이다 (D02 §12). 요청 값은 권한 근거가 아니라 비교 대상일 뿐이다.
// 저장된 resource의 tenant 검사에는 CheckTenant를 쓴다.
func CheckRequestTenant(p Principal, requested TenantID) error {
	if !p.Authenticated() {
		return ErrUnauthenticated
	}
	if requested != p.tenant {
		return ErrForbidden
	}
	return nil
}

// CheckTenant는 resource가 principal의 tenant에 속하는지 검사한다.
// 다른 tenant의 resource는 존재를 숨기기 위해 ErrNotFound를 반환한다 (D02 §12, D04 §01).
func CheckTenant(p Principal, resourceTenant TenantID) error {
	if !p.Authenticated() {
		return ErrUnauthenticated
	}
	if resourceTenant.IsZero() || resourceTenant != p.tenant {
		return ErrNotFound
	}
	return nil
}

type principalKey struct{}

// WithPrincipal은 인증 middleware가 principal을 context에 싣는다.
func WithPrincipal(ctx context.Context, p Principal) context.Context {
	return context.WithValue(ctx, principalKey{}, p)
}

// FromContext는 context의 principal을 반환한다. 없거나 미인증이면 ErrUnauthenticated.
func FromContext(ctx context.Context) (Principal, error) {
	p, ok := ctx.Value(principalKey{}).(Principal)
	if !ok || !p.Authenticated() {
		return Principal{}, ErrUnauthenticated
	}
	return p, nil
}
