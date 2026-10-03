# cmd/query-api

Query planner와 조회 API. JSON AST → field catalog → parameter binding, mandatory predicate(시간·tenant·scope·expires_at·tombstone) 강제, 실행 예산.

- 쓰기 소유 데이터: query job, 일시 cache
- 동기 의존·장애 동작: mandatory predicate를 붙일 수 없으면 거절. shard 일부 실패는 기본 503.
- 단계: M0 (F02, F04)
- 명세: D02 §12~16, §19
