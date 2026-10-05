# cmd/query-api

Query planner와 조회 API. JSON AST → field catalog → parameter binding, mandatory predicate(시간·tenant·scope·expires_at·tombstone) 강제, 실행 예산.

- 쓰기 소유 데이터: query job, 일시 cache
- 동기 의존·장애 동작: mandatory predicate를 붙일 수 없으면 거절. shard 일부 실패는 기본 503.
- 단계: M0 (F02, F04)
- 명세: D02 §12~16, §19
- 구현(M0 1차): `POST /api/v1/query/metrics`(ADR 0027), `GET /api/v1/traces/{trace_id}` — `cmd/query-api`(진입점, `/healthz`·`/readyz`) → `internal/query`(인증·인가 범위·구조 완결성·응답) → `internal/telemetrystore`(query 계정, row policy) (ADR 0022)
- 실행 환경 변수: `MONTRACER_QUERY_ADDR`, `MONTRACER_PG_APP_DSN`, `MONTRACER_KEY_PEPPER_HEX`, `MONTRACER_CH_QUERY_DSN`, `MONTRACER_METRICS_ADDR`(기본 :9464)
- 운영 지표: `montracer_query_*` (ADR 0023)
- 아직 없는 것: `POST /query`·`/query/traces|logs`·query planner, session 인증, OpenAPI 원천, rate limit, watermark·sampled 계산
