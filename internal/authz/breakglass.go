package authz

import (
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode"
)

// BreakGlassAction은 플랫폼 운영자가 break-glass로만 할 수 있는 작업이다 (D04 §01, ADR 0033).
// 고객 role·key scope(Action)와 다른 타입이라 일반 Authorize 경로에 섞이지 않는다.
type BreakGlassAction string

const (
	// BreakGlassKeysList는 tenant의 key metadata(hash·원문 없음) 목록 조회다.
	BreakGlassKeysList BreakGlassAction = "break_glass.keys.list"
	// BreakGlassKeysRevoke는 tenant key의 즉시 폐기다 (D04 §11 RB03 "해당 key 차단").
	BreakGlassKeysRevoke BreakGlassAction = "break_glass.keys.revoke"
)

var breakGlassActions = map[BreakGlassAction]bool{BreakGlassKeysList: true, BreakGlassKeysRevoke: true}

// MaxBreakGlassTTL은 break-glass 권한의 최대 유효 기간이다 (D04 §01 "만료 30분").
const MaxBreakGlassTTL = 30 * time.Minute

// break-glass 입력 길이 상한. audit_events 컬럼 제약(actor_id 256, details)보다 작게 둔다.
const (
	maxOperatorIDLen = 128
	maxTicketLen     = 64
	minReasonLen     = 10
	maxReasonLen     = 500
)

// ErrBreakGlassExpired는 grant 유효 기간이 지났다는 뜻이다.
var ErrBreakGlassExpired = errors.New("authz: break-glass grant expired")

// BreakGlassGrant는 운영자 한 명이 한 tenant에 대해 정해진 작업을 30분 이하로 할 수 있다는 증서다.
// 사유·승인자·ticket을 담고, 사용할 때마다 tenant의 security 감사에 남는다(controldb).
// 승인자 신원 확인과 2인 통제는 bastion/session manager가 맡는다(D04 §02). 이 타입은 기록과 범위만 강제한다.
type BreakGlassGrant struct {
	tenant    TenantID
	operator  string
	approver  string
	ticket    string
	reason    string
	actions   map[BreakGlassAction]bool
	issuedAt  time.Time
	expiresAt time.Time
}

// NewBreakGlassGrant는 입력을 검증해 grant를 만든다.
//   - operator ≠ approver (본인 승인 금지, 대소문자 무시)
//   - ttl은 (0, 30분]
//   - action은 break-glass allowlist 안
//   - reason은 10~500자, 제어 문자 금지. 고객 데이터(값·payload)를 적지 않는다.
func NewBreakGlassGrant(tenant TenantID, operator, approver, ticket, reason string, actions []BreakGlassAction, now time.Time, ttl time.Duration) (BreakGlassGrant, error) {
	if tenant.IsZero() {
		return BreakGlassGrant{}, errors.New("authz: break-glass tenant is required")
	}
	if err := checkField("operator", operator, 1, maxOperatorIDLen); err != nil {
		return BreakGlassGrant{}, err
	}
	if err := checkField("approver", approver, 1, maxOperatorIDLen); err != nil {
		return BreakGlassGrant{}, err
	}
	if strings.EqualFold(strings.TrimSpace(operator), strings.TrimSpace(approver)) {
		return BreakGlassGrant{}, errors.New("authz: break-glass approver must differ from operator")
	}
	if err := checkField("ticket", ticket, 1, maxTicketLen); err != nil {
		return BreakGlassGrant{}, err
	}
	if err := checkField("reason", reason, minReasonLen, maxReasonLen); err != nil {
		return BreakGlassGrant{}, err
	}
	if ttl <= 0 || ttl > MaxBreakGlassTTL {
		return BreakGlassGrant{}, fmt.Errorf("authz: break-glass ttl must be within (0, %s]", MaxBreakGlassTTL)
	}
	if len(actions) == 0 {
		return BreakGlassGrant{}, errors.New("authz: break-glass needs at least one action")
	}
	set := make(map[BreakGlassAction]bool, len(actions))
	for _, a := range actions {
		if !breakGlassActions[a] {
			return BreakGlassGrant{}, fmt.Errorf("authz: unknown break-glass action %q", a)
		}
		set[a] = true
	}
	return BreakGlassGrant{
		tenant: tenant, operator: strings.TrimSpace(operator), approver: strings.TrimSpace(approver),
		ticket: strings.TrimSpace(ticket), reason: strings.TrimSpace(reason), actions: set,
		issuedAt: now.UTC(), expiresAt: now.UTC().Add(ttl),
	}, nil
}

func checkField(name, v string, minLen, maxLen int) error {
	v = strings.TrimSpace(v)
	n := len([]rune(v))
	if n < minLen || n > maxLen {
		return fmt.Errorf("authz: break-glass %s length must be within [%d, %d]", name, minLen, maxLen)
	}
	for _, r := range v {
		if unicode.IsControl(r) {
			return fmt.Errorf("authz: break-glass %s has control characters", name)
		}
	}
	return nil
}

// Check는 grant가 now에 action을 허용하는지 검사한다.
func (g BreakGlassGrant) Check(a BreakGlassAction, now time.Time) error {
	if g.tenant.IsZero() {
		return ErrUnauthenticated
	}
	if !g.actions[a] {
		return ErrForbidden
	}
	if !now.Before(g.expiresAt) {
		return ErrBreakGlassExpired
	}
	return nil
}

// Tenant는 grant의 대상 tenant다.
func (g BreakGlassGrant) Tenant() TenantID { return g.tenant }

// Operator는 실행하는 운영자 ID다. 감사 actor다.
func (g BreakGlassGrant) Operator() string { return g.operator }

// Approver는 승인자 ID다.
func (g BreakGlassGrant) Approver() string { return g.approver }

// Ticket은 incident·지원 ticket ID다.
func (g BreakGlassGrant) Ticket() string { return g.ticket }

// Reason은 사유다.
func (g BreakGlassGrant) Reason() string { return g.reason }

// IssuedAt은 발급 시각이다.
func (g BreakGlassGrant) IssuedAt() time.Time { return g.issuedAt }

// ExpiresAt은 만료 시각이다.
func (g BreakGlassGrant) ExpiresAt() time.Time { return g.expiresAt }
