# PS-0015: 권한 검사 query가 컬럼 권한을 보지 않아, 좁은 계정을 "권한 과다"로 판정함

- 날짜: 2026-10-19 (발견) / 2026-10-19 (해결)
- 영역: rollup · CI
- 영향: 가용성(servicemap job 기동 거부), 잘못 쓰면 반대로 권한 과다를 놓칠 수 있음 — P2
- 수정: polynomeer/montracer#63
- 관련: ADR 0062 §5, ADR 0018, ADR 0026 §3

## 증상

CI 통합 시험 `TestEdgeRollupEndToEnd`에서 servicemap 계정 연결이 `servicemap: account must not read span payload, logs, or metrics`로 거부됐다. 이 계정은 migration 00006에서 `spans_local`의 짝 계산 컬럼 11개만 받았고 payload는 받지 않았다. 같은 계정으로 `SELECT payload FROM spans_local`을 직접 실행한 `TestServiceMapAccountIsNarrow`는 ACCESS_DENIED(497)를 기대대로 받았다.

## 발견 경위

PR CI의 통합 job에서 처음 실행됐을 때 드러났다. 로컬 Docker stack은 다른 프로젝트와 메모리를 나눠 써서 띄우지 않기 때문에 CI가 첫 실행이었다.

## 원인

1. 기동 검사가 `SELECT count() FROM spans_local WHERE 0 AND payload != ''`였다. "payload를 읽을 수 있으면 성공한다"는 가정이었다.
2. ClickHouse는 `WHERE 0 AND …`를 상수 `0`으로 접는다. 그 뒤 payload 열이 query에서 사라져, 열 권한 검사 대상이 아니게 된다.
3. 남은 `count()`는 표의 열을 하나라도 SELECT할 수 있으면 통과한다. 이 계정은 짝 계산 열을 읽을 수 있으므로 query가 성공했다.
4. 성공을 "payload를 읽을 수 있다"로 해석해서 거부했다.

같은 실수로 반대 방향도 생길 수 있다. 열 하나를 실수로 더 준 계정을 이 방식으로 검사하면 놓친다.

## 해결

- 검사할 열을 SELECT 목록에 직접 쓴다(`SELECT payload FROM spans_local WHERE 0`). 열 권한은 실제로 읽을 열에만 적용된다.
- 검사 대상을 넓혔다: span name, log·metric 원본, metric rollup, trace lookup, quarantine, edge 집계 값.
- 버린 대안: `system.grants`를 읽어 비교하기. role 상속·wildcard grant를 직접 풀어야 해서, 실제 접근 시도보다 틀리기 쉽다.

## 재발 방지

- 시험: `internal/servicemap/integration_test.go`
  - `TestEdgeRollupEndToEnd`: 좁은 계정이 열리는지 확인한다.
  - `TestServiceMapAccountIsNarrow`: 같은 열들이 실제로 거부되는지 확인하고, metric rollup 계정이 servicemap store로 열리지 않는지도 확인한다.
- 코드: `internal/servicemap/clickhouse.go`의 검사 목록에 이 이유를 주석으로 남겼다.

## 교훈

권한을 "query가 성공하는가"로 검사할 때는, 확인하려는 열이 최적화 뒤에도 query에 남아 있어야 한다. `count()`와 상수 조건은 열을 지운다. 표 전체 접근(`count()`)과 특정 열 접근은 다른 질문이다.
