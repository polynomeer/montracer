# cmd/query-api

Query planner와 조회 API. JSON AST → field catalog → parameter binding, mandatory predicate(시간·tenant·scope·expires_at·tombstone) 강제, 실행 예산.

- 쓰기 소유 데이터: query job, 일시 cache
- 동기 의존·장애 동작: mandatory predicate를 붙일 수 없으면 거절. shard 일부 실패는 기본 503.
- 단계: M0 (F02, F04)
- 명세: D02 §12~16, §19
- 구현(M0 1차): `POST /api/v1/query`·`/query/logs`(log 검색, query planner `internal/queryplan`, ADR 0037), `POST /api/v1/query/metrics`(ADR 0027), `GET /api/v1/traces/{trace_id}` — `cmd/query-api`(진입점, `/healthz`·`/readyz`) → `internal/query`(인증·인가 범위·구조 완결성·응답) → `internal/telemetrystore`(query 계정, row policy) (ADR 0022)
- 실행 환경 변수: `MONTRACER_QUERY_ADDR`, `MONTRACER_PG_APP_DSN`, `MONTRACER_KEY_PEPPER_HEX`, `MONTRACER_CH_QUERY_DSN`, `MONTRACER_CURSOR_KEY_HEX`(검색 cursor 서명, 32 byte 이상, secret manager), `MONTRACER_METRICS_ADDR`(기본 :9464)
- 운영 지표: `montracer_query_*` (ADR 0023)
- tenant별 조회 동시 실행 5·대기 20(모든 조회 경로, 넘으면 429)
- 아직 없는 것: trace·error 검색(`/query/traces`), 서비스 catalog(service.name 필터·env 제한 key의 log 검색), `/query-jobs`·scan 추정, 결과 cache, session 인증, OpenAPI 원천, watermark·sampled 계산
