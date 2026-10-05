# migrations

| 경로 | 대상 | 명세 |
|---|---|---|
| `postgres/` | 제어 DB (tenants, memberships, api_keys, dashboards, monitors, deletion_jobs, outbox, audit) — RLS 필수 | D02 §11 |
| `clickhouse/` | 분석 저장소 (spans, logs, metric_points, rollup, trace_summary, service_edges) | D02 §09~10 |

실행: `make migrate` (로컬) / `cmd/migrate [postgres|clickhouse] up|down|status` (env `MONTRACER_MIGRATE_DSN` / `MONTRACER_MIGRATE_CH_DSN`, 관리자 계정). SQL은 `migrations/embed.go`로 바이너리에 포함된다 (ADR 0016, 0018).

규칙 (D06 §07)
- expand → backfill → contract 순서. 새 column은 nullable/default로 추가하고, 모든 reader가 이해한 뒤 옛 필드를 제거한다.
- 재실행 가능해야 하고 구버전 reader 호환을 시험한다.
- 테넌트 테이블은 `ENABLE` + `FORCE ROW LEVEL SECURITY`, 모든 자식 FK에 tenant_id 포함.
- 비가역 migration은 backup·restore 검증과 별도 점검 시간을 요구한다.
