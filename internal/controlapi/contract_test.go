package controlapi

import (
	"os"
	"testing"

	"github.com/polynomeer/montracer/tests/contract"
)

// handler 시험이 주고받은 모든 요청·응답을 OpenAPI 원천으로 검사하고,
// control-api의 operation마다 검사된 성공 응답이 있는지 본다 (ADR 0059).
func TestMain(m *testing.M) { os.Exit(contract.Run(m, "control-api")) }

// 등록한 route와 OpenAPI 원천의 control-api operation이 정확히 같다.
func TestRoutesMatchOpenAPI(t *testing.T) {
	clock := now
	h := newHandler(t, &fakeAudit{}, principals{}, &clock)
	if err := contract.CheckRoutes("control-api", h.Routes()); err != nil {
		t.Fatal(err)
	}
}
