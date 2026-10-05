# cmd/ingress

OTLP/gRPC·OTLP/HTTP 수신. 인증 → 압축 해제 한도 → decode → tenant 주입 → 검증 → PII 제거 → quota → canonical envelope → Kafka append 후 ACK.

- 쓰기 소유 데이터: 정제된 canonical Kafka topic
- 동기 의존·장애 동작: 정책·키 cache 만료 시 fail closed. 키 폐기 확인 불가 60초 초과 시 해당 인증 경로 거절.
- 단계: M0 (F01)
- 구현: `cmd/ingress`(진입점, `/healthz`·`/readyz`) → `internal/ingest`(OTLP/HTTP 처리, Kafka producer), `internal/telemetry/{otlp,redact,envelope}` (ADR 0020)
- 실행 환경 변수: `MONTRACER_INGRESS_ADDR`, `MONTRACER_PG_APP_DSN`, `MONTRACER_KEY_PEPPER_HEX`, `MONTRACER_KAFKA_BROKERS`, `MONTRACER_ROUTING_EPOCH`
- 명세: D02 §03~05, §22 · D04 §02~03
