// montracer-admin은 플랫폼 운영자의 break-glass 도구다 (cmd/montracer-admin/README.md, D04 §01, §11 RB03, ADR 0033).
//
//	montracer-admin keys list   --tenant UUID --approver ID --ticket ID --reason TEXT
//	montracer-admin keys revoke --tenant UUID --key-id ID --approver ID --ticket ID --reason TEXT [--yes]
//
// 환경 변수
//
//	MONTRACER_PG_APP_DSN    제어 DB 앱 계정 (RLS 적용, tenant 범위로만 접근)
//	MONTRACER_OPERATOR_ID   운영자 ID (필수, bastion/session manager가 설정. 인자로 받지 않는다)
//
// 실행은 감사 가능한 bastion/session manager에서만 한다(D04 §02). 모든 시도는 대상 tenant의 security 감사에 남는다.
package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/polynomeer/montracer/internal/authz"
	"github.com/polynomeer/montracer/internal/controldb"
)

const usage = `usage:
  montracer-admin keys list   --tenant UUID --approver ID --ticket ID --reason TEXT
  montracer-admin keys revoke --tenant UUID --key-id ID --approver ID --ticket ID --reason TEXT [--yes]
env: MONTRACER_PG_APP_DSN, MONTRACER_OPERATOR_ID`

// keyStore는 이 도구가 쓰는 제어 DB 작업이다(시험에서 바꿔 끼운다).
type keyStore interface {
	BreakGlassListKeys(ctx context.Context, g authz.BreakGlassGrant, requestID string) ([]controldb.KeyMetadata, error)
	BreakGlassRevokeKey(ctx context.Context, g authz.BreakGlassGrant, keyID, requestID string) (controldb.RevokeOutcome, error)
}

type env struct {
	getenv func(string) string
	stdout io.Writer
	stderr io.Writer
	now    func() time.Time
	// open은 제어 DB에 연결한다. 인자 검증이 끝난 뒤에만 부른다.
	open func(ctx context.Context) (keyStore, func(), error)
}

// errUsage는 인자 오류다(종료 코드 2).
var errUsage = errors.New("usage")

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	e := env{
		getenv: os.Getenv, stdout: os.Stdout, stderr: os.Stderr, now: time.Now,
		open: func(ctx context.Context) (keyStore, func(), error) {
			db, err := controldb.Open(ctx, os.Getenv("MONTRACER_PG_APP_DSN"))
			if err != nil {
				return nil, nil, fmt.Errorf("control db: %w", err)
			}
			return controldb.NewKeyStore(db), db.Close, nil
		},
	}
	err := run(ctx, os.Args[1:], e)
	switch {
	case errors.Is(err, errUsage):
		os.Exit(2)
	case err != nil:
		fmt.Fprintln(os.Stderr, "montracer-admin:", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string, e env) error {
	if len(args) < 2 || args[0] != "keys" || (args[1] != "list" && args[1] != "revoke") {
		_, _ = fmt.Fprintln(e.stderr, usage)
		return errUsage
	}
	sub := args[1]
	fs := flag.NewFlagSet("montracer-admin keys "+sub, flag.ContinueOnError)
	fs.SetOutput(e.stderr)
	var (
		tenantS  = fs.String("tenant", "", "대상 tenant UUID")
		approver = fs.String("approver", "", "승인자 ID (운영자와 달라야 한다)")
		ticket   = fs.String("ticket", "", "incident·지원 ticket ID")
		reason   = fs.String("reason", "", "사유 10~500자. 고객 데이터(값·payload)를 적지 않는다")
		keyID    = fs.String("key-id", "", "폐기할 key ID (revoke)")
		yes      = fs.Bool("yes", false, "확인 없이 폐기 (revoke)")
	)
	if err := fs.Parse(args[2:]); err != nil {
		return errUsage
	}
	if fs.NArg() != 0 {
		_, _ = fmt.Fprintln(e.stderr, "unexpected arguments:", strings.Join(fs.Args(), " "))
		return errUsage
	}
	// 운영자 신원은 session manager가 넣은 env에서만 온다(인자로 받지 않는다, 감사 actor 위조 방지).
	// env 자체를 바꿀 수 있는 사람은 이 통제를 우회할 수 있다 — ADR 0033 §4 공백.
	op := e.getenv("MONTRACER_OPERATOR_ID")
	action := authz.BreakGlassKeysList
	if sub == "revoke" {
		action = authz.BreakGlassKeysRevoke
	}
	// 거절된 시도도 구조화 로그에 남긴다(AWS SEC03-BP03). 사유 원문은 쓰지 않는다.
	logger := slog.New(slog.NewJSONHandler(e.stderr, nil)).With(
		slog.String("operator", op), slog.String("approver", *approver), slog.String("ticket", *ticket),
		slog.String("tenant_id", *tenantS), slog.String("action", string(action)))
	reject := func(err error) error {
		logger.Warn("break-glass rejected", slog.String("error", err.Error()))
		return err
	}
	if op == "" {
		return reject(errors.New("MONTRACER_OPERATOR_ID is not set (run from the bastion session)"))
	}
	tenant, err := authz.ParseTenantID(*tenantS)
	if err != nil {
		return reject(fmt.Errorf("--tenant: %w", err))
	}
	if sub == "revoke" && !authz.ValidKeyID(*keyID) {
		return reject(errors.New("--key-id must be 16 lowercase hex characters"))
	}
	// 한 번 실행에 한 작업만 허용하는 grant. 30분은 grant 객체의 수명 상한이다(승인 자체의 수명이 아니다, ADR 0033 §4).
	g, err := authz.NewBreakGlassGrant(tenant, op, *approver, *ticket, *reason, []authz.BreakGlassAction{action}, e.now(), authz.MaxBreakGlassTTL)
	if err != nil {
		return reject(err)
	}
	if sub == "revoke" && !*yes {
		_, err := fmt.Fprintf(e.stdout, "will revoke key %s of tenant %s immediately (ticket %s, approver %s). re-run with --yes to proceed.\n",
			*keyID, tenant, g.Ticket(), g.Approver())
		return err
	}

	requestID, err := newRequestID()
	if err != nil {
		return err
	}
	logger = logger.With(slog.String("request_id", requestID))

	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	store, closeStore, err := e.open(ctx)
	if err != nil {
		return err
	}
	defer closeStore()

	switch sub {
	case "list":
		keys, err := store.BreakGlassListKeys(ctx, g, requestID)
		if err != nil {
			logger.Warn("break-glass failed", slog.String("error", err.Error()))
			return err
		}
		logger.Info("break-glass keys listed", slog.Int("keys", len(keys)))
		return printKeys(e.stdout, keys)
	default:
		out, err := store.BreakGlassRevokeKey(ctx, g, *keyID, requestID)
		if err != nil {
			logger.Warn("break-glass failed", slog.String("key_id", *keyID), slog.String("error", err.Error()))
			if errors.Is(err, authz.ErrNotFound) {
				return fmt.Errorf("key %s not found in tenant %s (attempt audited)", *keyID, tenant)
			}
			return err
		}
		msg := "revoked"
		if out == controldb.RevokeAlreadyRevoked {
			msg = "already revoked, no change"
		}
		logger.Info("break-glass key revoke", slog.String("key_id", *keyID), slog.String("outcome", msg))
		// 폐기는 이미 적용됐다. 출력 실패는 결과를 바꾸지 않지만 운영자가 알 수 있게 오류로 돌려준다.
		_, err = fmt.Fprintf(e.stdout, "key %s: %s (request %s). authentication reads the control DB directly, so the key is rejected from now on.\n", *keyID, msg, requestID)
		return err
	}
}

func printKeys(w io.Writer, keys []controldb.KeyMetadata) error {
	tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	if _, err := fmt.Fprintln(tw, "KEY_ID\tKIND\tSCOPES\tENVIRONMENTS\tCREATED\tEXPIRES\tREVOKED"); err != nil {
		return err
	}
	for _, k := range keys {
		scopes := make([]string, len(k.Scopes))
		for i, s := range k.Scopes {
			scopes[i] = string(s)
		}
		revoked := "-"
		if k.RevokedAt != nil {
			revoked = k.RevokedAt.UTC().Format(time.RFC3339)
		}
		if _, err := fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n", k.KeyID, k.Kind, strings.Join(scopes, ","), strings.Join(k.Environments, ","),
			k.CreatedAt.UTC().Format(time.RFC3339), k.ExpiresAt.UTC().Format(time.RFC3339), revoked); err != nil {
			return err
		}
	}
	return tw.Flush()
}

func newRequestID() (string, error) {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("request id: %w", err)
	}
	return "bg-" + hex.EncodeToString(b[:]), nil
}
