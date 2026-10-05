# cmd/worker

Kafka 소비 → 신호별 정규화·dedup → ClickHouse sink, metric window 집계. durable insert 후 offset commit.

- 쓰기 소유 데이터: 신호 원본(`spans_local`, `logs_local`, `metric_points`), `ingest_quarantine`, 처리 checkpoint(consumer group offset)
- 동기 의존·장애 동작: sink 실패 시 offset 보류. 같은 batch를 같은 token으로 60초까지 재시도하고, 넘으면 commit 없이 종료한다(재시작 시 마지막 commit부터 재처리). 드라이버가 특정 행을 결정적으로 거부하면 그 행만 `sink_rejected`로 quarantine하고 나머지를 저장한다. ingest 계정이 원본을 읽을 수 있으면 기동 거부.
- 단계: M0 (F01~F03)
- 구현: `cmd/worker`(진입점) → `internal/pipeline`(header 검증·정규화·(tenant, event_id) dedup·ClickHouse sink·consumer loop) (ADR 0021)
- 실행 환경 변수: `MONTRACER_KAFKA_BROKERS`, `MONTRACER_CH_INGEST_DSN`, `MONTRACER_CH_INSERT_QUORUM`(production 2), `MONTRACER_WORKER_GROUP`, `MONTRACER_METRICS_ADDR`(기본 :9464)
- 운영 지표·경보: `montracer_worker_*` (ADR 0023), 대응은 [RB01](../../docs/runbooks/RB01-kafka-backlog-and-store-failure.md)
- 아직 없는 것: metric_1h·backfill·window lease, batch를 넘는 충돌 격리 state
- 명세: D02 §05, §07, §09~10, §21~22
- 역할(`MONTRACER_WORKER_ROLES`): `ingest`(기본, Kafka → 원본) · `rollup`(metric_points → metric_1m, **cluster에 하나만**, `MONTRACER_CH_ROLLUP_DSN`) (ADR 0026)
