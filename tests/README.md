# tests

서비스 단위 테스트는 각 패키지 옆에 둔다. 여기는 여러 컴포넌트를 가로지르는 시험이다 (D06 §03~05).

| 경로 | 내용 | 실행 |
|---|---|---|
| `fixtures/otlp/` | golden OTLP JSON/proto fixture | 모든 ingest/query 변경 |
| `fixtures/pii/` | **가짜** PII·secret fixture (저장 계층 어디에도 남으면 실패) | PR |
| `contract/` | OpenAPI·proto·UI fixture 일치 (`make test-contract`) | release matrix |
| `isolation/` | query·stream·object·export cross-tenant 공격 (`make test-isolation`, [README](isolation/README.md)) — 1차: trace IDOR·tenant header·key 종류·감사·cursor 재사용 | 모든 PR (P0, CI 통합 job) |
| `e2e/` | 설치 → trace → log → monitor → 삭제 | staging 매일 |
| `load/` | open-loop generator와 workload manifest (seed 고정). `load/chlayout`: ClickHouse layout 실험 | 부하 시험 |

- fixture에 실제 고객 데이터·계정·secret을 넣지 않는다.
- 데이터 정확성 oracle: 동일 batch 3회 재전송 시 logical 결과 동일, percentile 평균으로 구현하면 반드시 실패하는 fixture 포함.
