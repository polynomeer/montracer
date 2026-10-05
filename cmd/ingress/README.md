# cmd/ingress

OTLP/gRPC·OTLP/HTTP 수신. 인증 → 압축 해제 한도 → decode → tenant 주입 → 검증 → PII 제거 → quota → canonical envelope → Kafka append 후 ACK.

- 쓰기 소유 데이터: 정제된 canonical Kafka topic
- 동기 의존·장애 동작: 정책·키 cache 만료 시 fail closed. 키 폐기 확인 불가 60초 초과 시 해당 인증 경로 거절.
- 단계: M0 (F01)
- 구현: `cmd/ingress`(진입점, `/healthz`·`/readyz`) → `internal/ingest`(OTLP/HTTP 처리, Kafka producer), `internal/telemetry/{otlp,redact,envelope}` (ADR 0020)
- 실행 환경 변수: `MONTRACER_INGRESS_ADDR`, `MONTRACER_PG_APP_DSN`, `MONTRACER_KEY_PEPPER_HEX`, `MONTRACER_KAFKA_BROKERS`, `MONTRACER_ROUTING_EPOCH`, `MONTRACER_METRICS_ADDR`(기본 :9464), `MONTRACER_INGRESS_MAX_INFLIGHT`, `MONTRACER_INGRESS_REPLICAS`, `MONTRACER_QUOTA_{RECORDS,BYTES}_{PER_SEC,BURST}`, `MONTRACER_QUOTA_OVERRIDES_FILE`
- 운영 지표·경보: `montracer_ingress_*` (ADR 0023), 대응은 [RB01](../../docs/runbooks/RB01-kafka-backlog-and-store-failure.md)
- 명세: D02 §03~05, §22 · D04 §02~03
- tenant quota: tenant·signal별 rate 초과 429, burst 초과 413, instance 과부하 503 (ADR 0024). 조정 절차는 RB01 "tenant quota 조정"
- metric cardinality: 금지 dimension·label 20개 초과·활성 series 상한(기본 100k, `MONTRACER_QUOTA_ACTIVE_SERIES`, overrides `metrics.active_series`) 초과 point를 partial success로 거절, 등록부(제어 DB) 장애는 503 (ADR 0029)
