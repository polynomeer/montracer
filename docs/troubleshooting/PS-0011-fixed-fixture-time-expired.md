# PS-0011: 고정 날짜 fixture가 보존 기간을 지나 통합 시험이 저장 즉시 만료

- 날짜: 2026-10-13 (발견) / 2026-10-13 (해결)
- 영역: CI · 시험
- 영향: 개발 생산성. 코드 변경과 무관한 CI 실패(main도 같은 날부터 실패). 심각도 P2
- 수정: polynomeer/montracer#50
- 관련: D04 §04(trace·log 보존 7일), ADR 0018(expires_at mandatory predicate), [PS-0005](PS-0005-integration-test-shared-state.md)

## 증상

2026-10-13 CI의 `go integration (postgres)`에서 ClickHouse에 쓰고 읽는 통합 시험이 모두 0행을 읽었다.
- `internal/pipeline`: `TestWorkerCrashBetweenInsertAndCommit`, `TestSinkTokenDedup` — "spans = 0, want 20"
- `internal/query`: `TestVerticalSliceIngestToQuery` — "trace not visible in time: status=404"

같은 시험이 2026-10-10까지는 통과했다. PR의 변경(알림)은 이 경로를 건드리지 않는다.

## 원인

1. 두 시험은 이벤트 시각으로 **고정 날짜**를 썼다(query 시험은 OTLP fixture 파일의 시각까지).
   - `pipeline_test.go`의 `now` = 2026-10-05
   - `query/integration_test.go`의 `fixtureTime` = 2026-10-05
2. worker는 `expires_at = event_time + 보존 기간`으로 저장한다. trace·log 보존 기간은 7일이다(D04 §04).
3. 2026-10-12부터 이 행은 저장되는 순간 이미 만료였다.
   - 조회는 `expires_at > now`를 mandatory predicate로 건다(ADR 0018). 그래서 0행이 나온다.
   - ClickHouse `TTL expires_at DELETE`도 곧 지운다.
4. 단위 시험은 저장하지 않으므로 고정 날짜로도 통과했다. 그래서 날짜가 지나기 전까지 드러나지 않았다.

## 해결

- 두 변수를 실행 시각 기준으로 바꿨다.
  - `now = time.Now().UTC().Truncate(time.Hour)`
  - `fixtureTime = time.Now().UTC().Truncate(time.Second).Add(-time.Minute)`
- 이 변수를 쓰는 단위 시험은 절대 날짜에 기대지 않아 그대로 통과한다.
- query 시험이 보내는 OTLP fixture(`tests/fixtures/otlp/traces_checkout.json`)도 고정 시각(1791158400초)이다. 시험이 읽을 때 그 초를 `fixtureTime`으로 바꾼다. 그대로 두면 ingress 시간 창(24시간)을 벗어나 거절된다.
- 버린 대안: 시험에서만 보존 기간을 늘리기. 저장 경로의 실제 보존 의미를 시험하지 못한다.

## 재발 방지

- 저장소(ClickHouse·PostgreSQL)에 쓰는 통합 시험은 이벤트 시각을 실행 시각 기준으로 만든다. 다른 통합 시험(telemetrystore·alertworker·controldb)은 이미 그렇게 한다.
- 고정 날짜는 저장하지 않는 순수 계산 시험(정렬·경계 계산)에만 쓴다.
- 확인 방법: `grep -rn "time.Date(20\|time.Unix(1[78]" --include='*integration_test.go'`가 비어 있어야 한다.

## 교훈

보존 기간이 있는 저장소에 쓰는 fixture의 시각은 "언젠가 만료되는 값"이다. 고정 날짜 fixture는 시간이 흐르면서 시험을 깨뜨리는 시한폭탄이 된다.
