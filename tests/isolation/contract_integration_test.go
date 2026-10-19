//go:build integration

package isolation

import (
	"os"
	"testing"

	"github.com/polynomeer/montracer/tests/contract"
)

// 실제 저장소를 거친 query-api·control-api 응답도 OpenAPI 원천으로 검사한다(공격 시험의 거절 응답 포함, ADR 0059).
// 이 suite는 모든 operation을 부르지 않으므로 coverage는 보지 않는다(handler 시험이 본다).
func TestMain(m *testing.M) { os.Exit(contract.Run(m, "")) }
