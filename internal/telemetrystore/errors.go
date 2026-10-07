package telemetrystore

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"

	"github.com/ClickHouse/clickhouse-go/v2"
)

// 이 패키지의 오류는 apierr를 import하지 않고 메서드로 분류를 알린다 (ADR 0014 §1).
//   Unavailable() bool                   → 503 UNAVAILABLE
//   QueryTimeout() bool                  → 504 QUERY_TIMEOUT
//   BudgetExceeded() map[string]any      → QUERY_BUDGET_EXCEEDED (details: 넘은 한도)
//   InvalidArgument() (field, reason)    → 400 INVALID_ARGUMENT

type unavailableError struct {
	op  string
	err error
}

func (e *unavailableError) Error() string {
	return "telemetrystore: " + e.op + ": unavailable: " + e.err.Error()
}
func (e *unavailableError) Unwrap() error     { return e.err }
func (e *unavailableError) Unavailable() bool { return true }

func unavailable(op string, err error) error { return &unavailableError{op: op, err: err} }

type timeoutError struct {
	op  string
	err error
}

func (e *timeoutError) Error() string {
	return "telemetrystore: " + e.op + ": query timeout: " + e.err.Error()
}
func (e *timeoutError) Unwrap() error      { return e.err }
func (e *timeoutError) QueryTimeout() bool { return true }

// budgetError는 실행 예산·범위 한도 초과다 (D02 §15, §19). details는 넘은 한도를 담는다(값 내용 없음).
type budgetError struct {
	op      string
	details map[string]any
	err     error
}

func (e *budgetError) Error() string {
	if e.err != nil {
		return "telemetrystore: " + e.op + ": budget exceeded: " + e.err.Error()
	}
	return "telemetrystore: " + e.op + ": budget exceeded"
}
func (e *budgetError) Unwrap() error                  { return e.err }
func (e *budgetError) BudgetExceeded() map[string]any { return e.details }

type invalidArgError struct {
	field, reason string
}

func (e *invalidArgError) Error() string {
	return "telemetrystore: invalid " + e.field + ": " + e.reason
}
func (e *invalidArgError) InvalidArgument() (string, string) { return e.field, e.reason }

func invalidArg(field, reason string) error { return &invalidArgError{field: field, reason: reason} }

// ClickHouse ErrorCodes 분류.
var (
	unavailableCodes = map[int32]bool{
		202: true, // TOO_MANY_SIMULTANEOUS_QUERIES
		209: true, // SOCKET_TIMEOUT
		210: true, // NETWORK_ERROR
		241: true, // MEMORY_LIMIT_EXCEEDED (서버 전체. query 한도면 아래 isQueryMemoryLimit로 예산 초과)
		242: true, // TABLE_IS_READ_ONLY (replica 장애)
	}
	timeoutCodes = map[int32]bool{
		159: true, // TIMEOUT_EXCEEDED (max_execution_time)
	}
	budgetCodes = map[int32]bool{
		158: true, // TOO_MANY_ROWS
		160: true, // TOO_SLOW
		307: true, // TOO_MANY_BYTES (max_bytes_to_read)
		396: true, // TOO_MANY_ROWS_OR_BYTES (max_result_rows)
	}
)

// classify는 연결·과부하는 503, 시간 초과는 504, 예산 초과는 QUERY_BUDGET_EXCEEDED로 표시하고
// 나머지(문법·권한 등 코드 결함)는 그대로 wrap해 500이 되게 한다.
func classify(op string, err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return fmt.Errorf("telemetrystore: %s: %w", op, err)
	}
	var ex *clickhouse.Exception
	if errors.As(err, &ex) {
		switch {
		case ex.Code == 241 && isQueryMemoryLimit(ex.Message):
			// query 하나의 max_memory_usage 초과는 다시 해도 같다 — 503(재시도)이 아니라 예산 초과다(ADR 0043)
			return &budgetError{op: op, err: err}
		case unavailableCodes[ex.Code]:
			return unavailable(op, err)
		case timeoutCodes[ex.Code]:
			return &timeoutError{op: op, err: err}
		case budgetCodes[ex.Code]:
			return &budgetError{op: op, err: err}
		}
		return fmt.Errorf("telemetrystore: %s: %w", op, err)
	}
	var netErr net.Error
	if errors.As(err, &netErr) || errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return unavailable(op, err)
	}
	return fmt.Errorf("telemetrystore: %s: %w", op, err)
}

func lower(s string) string { return strings.ToLower(s) }

// isQueryMemoryLimit는 MEMORY_LIMIT_EXCEEDED가 query 한도(max_memory_usage)인지 본다. 서버 전체 한도("(total)")는 과부하다.
func isQueryMemoryLimit(msg string) bool {
	return strings.Contains(msg, "(for query)")
}
