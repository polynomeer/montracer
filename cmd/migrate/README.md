# cmd/migrate

PostgreSQL·ClickHouse schema migration과 Kafka 수집 topic 생성·설정 검증을 맡는다. SQL은 `migrations/embed.go`로 바이너리에 포함된다.

- 사용: `migrate [postgres|clickhouse] up|down|status` · `migrate kafka up` (target을 생략하면 postgres)
- 실행 계정: owner(관리자) 계정. 앱 계정에는 migration이 최소 권한만 GRANT한다.
- 환경 변수: `MONTRACER_MIGRATE_DSN`(PG) · `MONTRACER_MIGRATE_CH_DSN`(ClickHouse) · `MONTRACER_KAFKA_BROKERS` · `MONTRACER_KAFKA_ALLOW_LOW_REPLICATION=1`(로컬·CI 단일 broker에서만 RF 3 미만 허용)
- Kafka: topic 3개(`telemetry.{traces,logs,metrics}.raw.v1`)를 멱등하게 만든다. **이미 있는 topic도 RF·min.insync.replicas·retention·max.message.bytes를 검증하고, 다르면 실패한다.** RF 1로 미리 만들어진 topic이 조용히 통과하면 `acks=all`의 의미가 깨지기 때문이다(ADR 0002, 0020).
- `down`은 한 단계만 되돌린다. 비가역 migration은 backup·restore 검증 후 실행한다 (D06 §07).
- 로컬: `make migrate`, `make migrate-kafka`, `make migrate-status`
- 결정: ADR 0016(PG·goose), 0018(ClickHouse 계정·row policy), 0020(topic 설정)
