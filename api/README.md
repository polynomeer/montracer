# api

계약의 원천(source of truth). 코드 생성과 contract test는 여기서 출발한다 (D02 §12, §20).

| 경로 | 내용 |
|---|---|
| `openapi/` | 관리·조회 API OpenAPI 3.1 (`/api/v1`) |
| `proto/` | 고정(pin)한 OTLP proto와 내부 event schema (outbox, Kafka envelope) |

- CI에서 breaking-change 검사와 consumer contract test를 실행한다.
- additive 필드는 v1에 추가 가능, 의미 변경은 v2로 분리한다.
- event에는 `schema_version`을 넣는다. unknown additive field는 무시, unknown major는 quarantine.
