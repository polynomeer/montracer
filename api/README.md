# api

계약의 원천(source of truth). 코드 생성과 contract test는 여기서 출발한다 (D02 §12, §20).

| 경로 | 내용 |
|---|---|
| `openapi/` | 관리·조회 API OpenAPI 3.1 원천 `montracer.yaml` (`/api/v1`, operation마다 `x-montracer-service`, ADR 0059) |
| `proto/` | 고정(pin)한 OTLP proto와 내부 event schema (outbox, Kafka envelope) |

OTLP 데이터 모델은 `.proto`를 복사하지 않고 `go.opentelemetry.io/collector/pdata`(go.mod에서 버전 고정, 현재 v1.68.0)로 고정한다. pdata는 OTLP JSON 규칙(hex trace/span ID, 문자열 int64 등)을 공식 구현한다 (ADR 0001). 내부 event schema(.proto)는 Kafka envelope 작업(Sprint 2)에서 추가한다.

- CI에서 breaking-change 검사와 consumer contract test를 실행한다 (ADR 0059).
  - provider: `internal/query`·`internal/controlapi` handler 시험과 `tests/isolation`의 모든 교환을 원천으로 검사(strict: 선언 안 된 field도 위반), operation마다 성공 응답 coverage, 등록 route와 원천 일치.
  - consumer: `apps/web` 시험의 fetch 대역 응답·요청을 원천으로 검사.
  - breaking: `scripts/dev/apidiff`가 base 대비 비교. 의도한 예외는 PR label `api-breaking`.
  - `make test-contract`로 셋을 한 번에 돌린다.
- route·응답을 바꾸는 PR은 `montracer.yaml`을 같은 PR에서 고친다. 고치지 않으면 위 시험이 실패한다.
- additive 필드는 v1에 추가 가능, 의미 변경은 v2로 분리한다.
- event에는 `schema_version`을 넣는다. unknown additive field는 무시, unknown major는 quarantine.
