# internal

서비스 간 공유하는 Go 패키지 (외부 import 불가).

| 패키지 | 책임 | 명세 |
|---|---|---|
| `authz/` | principal, tenant context, RBAC scope, step-up | D04 §01~02, §12 |
| `telemetry/` | canonical schema, envelope, 신호 identity. `telemetry/otlp`: OTLP/HTTP bounded decode와 record 검증. `telemetry/redact`: 영속 저장 전 PII·secret 제거 | D02 §04, §05, §07, §18 · D03 §02 · D04 §03 |
| `telemetrystore/` | ClickHouse 조회 계층: query 계정 연결(읽기 전용 강제), tenant row policy 설정, trace 조회 | D02 §09, §15 · ADR 0018 |
| `query/` | filter AST, field catalog, 실행 예산, mandatory predicate | D02 §15, §19 |
| `pipeline/` | dedup, checkpoint, offset commit, metric window | D02 §05, §10, §21~22 |

규칙 (D06 §10~11)
- 함수는 tenant context를 명시적으로 받는다. 전역 mutable tenant 상태 금지, tenant 없는 repository method 금지.
- raw SQL은 repository / query planner에만 둔다.
- secret은 config object와 logging object에서 분리한다.
