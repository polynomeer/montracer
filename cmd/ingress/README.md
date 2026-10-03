# cmd/ingress

OTLP/gRPC·OTLP/HTTP 수신. 인증 → 압축 해제 한도 → decode → tenant 주입 → 검증 → PII 제거 → quota → canonical envelope → Kafka append 후 ACK.

- 쓰기 소유 데이터: 정제된 canonical Kafka topic
- 동기 의존·장애 동작: 정책·키 cache 만료 시 fail closed. 키 폐기 확인 불가 60초 초과 시 해당 인증 경로 거절.
- 단계: M0 (F01)
- 명세: D02 §03~05, §22 · D04 §02~03
