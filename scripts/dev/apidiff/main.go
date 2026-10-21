// apidiff는 OpenAPI 원천(api/openapi/montracer.yaml)의 호환을 깨는 변경을 base revision과 비교해 막는다 (D02 §12, ADR 0059 §3).
//
//	go run ./scripts/dev/apidiff -base origin/main
//
// base에 원천이 없으면(처음 도입) 비교하지 않는다. 의도한 breaking change는 v2 경로로 나누는 것이 원칙이고,
// 출시 전(M0)·합의된 예외는 MONTRACER_API_BREAKING_OK=1(CI는 PR label `api-breaking`)로 통과시키되 목록은 출력한다.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"time"

	"github.com/polynomeer/montracer/api/openapi"
	"github.com/polynomeer/montracer/tests/contract"
)

const specPath = "api/openapi/montracer.yaml"

func main() {
	base := flag.String("base", "origin/main", "비교할 git revision")
	flag.Parse()
	if err := run(*base); err != nil {
		_, _ = fmt.Fprintln(os.Stderr, "apidiff:", err)
		os.Exit(1)
	}
}

func git(ctx context.Context, args ...string) *exec.Cmd {
	return exec.CommandContext(ctx, "git", args...) //nolint:gosec // G204: 인자 배열, shell 없음
}

func run(base string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	// base는 개발자·CI가 주는 git revision이다(shell을 거치지 않고 인자로 넘긴다). 판정은 종료 코드로 한다(git 메시지 언어와 무관).
	if err := git(ctx, "rev-parse", "--verify", "--quiet", base+"^{commit}").Run(); err != nil {
		return fmt.Errorf("base revision %q가 없다: %w", base, err)
	}
	if err := git(ctx, "cat-file", "-e", base+":"+specPath).Run(); err != nil {
		fmt.Printf("apidiff: %s에 %s가 없다 — 비교하지 않는다(처음 도입)\n", base, specPath)
		return nil
	}
	old, err := git(ctx, "show", base+":"+specPath).Output()
	if err != nil {
		return fmt.Errorf("git show %s:%s: %w", base, specPath, err)
	}
	changes, err := contract.Breaking(old, openapi.Spec)
	if err != nil {
		return err
	}
	if len(changes) == 0 {
		fmt.Printf("apidiff: %s 대비 호환을 깨는 변경 없음\n", base)
		return nil
	}
	fmt.Printf("apidiff: %s 대비 호환을 깨는 변경 %d건:\n", base, len(changes))
	for _, c := range changes {
		fmt.Println("  - " + c)
	}
	if os.Getenv("MONTRACER_API_BREAKING_OK") == "1" {
		fmt.Println("apidiff: MONTRACER_API_BREAKING_OK=1 — 승인된 breaking change로 통과시킨다(PR에 이유와 consumer 영향을 적는다)")
		return nil
	}
	return errors.New("호환을 깨는 변경이 있다: 필드를 지우거나 바꾸는 대신 추가하거나, v2 경로로 나눈다 (ADR 0059 §3)")
}
