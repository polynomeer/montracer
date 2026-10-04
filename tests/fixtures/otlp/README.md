# OTLP golden fixtures

checkout → payment → PostgreSQL 장애 시나리오의 OTLP/HTTP JSON 본문이다. 모든 값은 가짜다.

| 파일 | 내용 | record 수 |
|---|---|---|
| `traces_checkout.json` | checkout(SERVER, 500) → payment client → payment(SERVER, 502) → DB(CLIENT). trace `4bf92f35…4736` | span 4 |
| `logs_checkout.json` | 같은 trace의 ERROR log 1건(trace·span 연결), 연결 없는 INFO log 1건 | log 2 |
| `metrics_checkout.json` | 요청 지연 histogram(cumulative), 오류 sum(monotonic), JVM heap gauge | data point 3 |

- 기준 시각: `2026-10-05T00:00:00Z` (= `1791158400000000000` ns). 검증 테스트는 이 시각 근처를 수신 시각으로 둔다.
- histogram은 bucket 합 = count(1000), 경계 오름차순이다 (metric oracle 전제, D06 §04).
- protobuf 형식은 테스트에서 JSON을 변환해 만든다. JSON과 protobuf 경로의 해석 결과가 같아야 한다.
- D06 §04 "golden OTLP fixture"의 시작점이다. 신호 연결·dedup oracle용 fixture는 Sprint 2에서 추가한다.
