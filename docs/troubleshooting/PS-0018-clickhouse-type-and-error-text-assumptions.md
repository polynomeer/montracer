# PS-0018: 집계 SQL이 ClickHouse 결과 타입과 오류 문구를 추정해 틀렸다

- 날짜: 2026-11-01 (발견) / 2026-11-01 (해결)
- 영역: telemetrystore (trace·log 집계)
- 영향: 표시 의미 — 심각도 P1(리뷰 P1, 배포 전).
  - (1) `histogram:true`인 log 집계가 모두 실패(500)했을 것이다.
  - (2) 값이 너무 많은 속성 facet을 "예산 초과"로 안내했을 것이다(계약 6의 사유 구분).
- 수정: 집계 백엔드 PR(ADR 0074 구현)
- 관련: ADR 0074 §4·§5, PS-0015(ClickHouse 상수 접기 — 같은 종류의 "서버 동작 추정")

## 증상

1. **심각도 히스토그램 scan 실패:** `sumMapIf([idx * 7 + <심각도 묶음>], …)`의 key를 Go `[]uint32`로 읽었다. 그런데 ClickHouse가 key를 `Array(UInt64)`로 돌려준다.
2. **group 상한 초과의 사유가 틀림:** 속성 facet의 `max_rows_to_group_by` 초과를 `too_many_values`로 분류하지 못했다. 같은 오류 코드 158(`TOO_MANY_ROWS`)인 읽기 상한 초과와 함께 `budget_exceeded`로 나갔다.

## 발견 경위

백엔드 spec-reviewer가 `clickhouse-local` 26.3으로 두 식을 직접 실행해 재현했다. 로컬에서 통합 시험을 못 돌렸고, 단위 시험은 둘 다 놓쳤다.

## 원인

1. `toUInt32 * 상수`는 ClickHouse 산술 승격 규칙으로 UInt64가 된다(`toTypeName(toUInt32(1) * 7)` = `UInt64`). 식의 결과 타입을 확인하지 않고 scan 타입을 정했다.
2. 오류 문구에 설정 이름(`max_rows_to_group_by`)이 들어 있다고 추정했다.
   - 실제 문구는 `Limit for rows to GROUP BY exceeded: has 667 rows, maximum: 10`이다.
   - 단위 시험의 fixture도 같은 추정으로 지어낸 문구라, 시험이 결함을 함께 숨겼다.

## 해결

1. key를 `toUInt32(...)`로 감싸 scan 타입과 맞췄다.
2. 판정을 `rows to GROUP BY`(그리고 설정 이름) 포함으로 바꿨다. 단위 시험 fixture를 실제 서버 문구로 바꿨다.

## 재발 방지

- 시험: `internal/telemetrystore/aggregate_integration_test.go`
  - `TestAggregateLogSeverityClasses`: 히스토그램 묶음별 합(UInt32 scan)과 심각도 경계
  - `TestAggregateLogs`: group 상한을 2로 낮춰 실제 서버 오류 → `too_many_values`(CI ClickHouse 26.8 문구를 고정)
- 시험: `internal/telemetrystore/aggregate_test.go` `TestFacetFailure` — 실제 문구, 같은 158의 읽기 상한은 `budget_exceeded`
- 규칙:
  - ClickHouse 식의 결과 타입이 Go scan 타입과 맞아야 하면 식을 명시 변환(`toUInt32` 등)으로 감싼다.
  - 오류 문구로 가를 때는 실제 서버 출력으로 fixture를 만들고, 통합 시험이 그 경로를 한 번은 실행하게 한다.

## 교훈

- 시험 fixture를 구현과 같은 가정으로 만들면 시험이 가정을 검증하지 못한다. 서버 동작은 서버에서 뽑은 값으로 시험한다.
