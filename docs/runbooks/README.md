# 운영 Runbook

D04 §11의 장애 대응 Runbook을 실제 운영 절차로 구체화해 이곳에 둔다.
각 기능은 DoD상 runbook을 함께 제출해야 한다 (D06 §02 공통 DoD).

| ID | 주제 | 원문 | 상태 |
|---|---|---|---|
| RB01 | Kafka 적체와 저장 장애 | D04 §11 | [작성](RB01-kafka-backlog-and-store-failure.md) — 경보 14종 대응 (ADR 0023, 0031) |
| RB02 | 검색 지연과 Cardinality 폭증 | D04 §11 | [작성](RB02-query-latency-and-cardinality.md) — 조회 p95·5xx, cardinality 상한 거절, 관리 API 5xx |
| RB03 | 개인정보 유출과 권한 침해 | D04 §11 | [작성](RB03-pii-exposure-and-access-breach.md) — probe redaction·isolation 경보, break-glass key 폐기(ADR 0033). 삭제 job·key 밖 break-glass 접근은 공백 |
| RB04 | 알림 누락과 지역 장애 | D04 §11 | 미작성 — alert-worker·지역 구성 이후 |
| — | 신규 기능 Runbook (진단, Profile, RUM, Synthetic 등) | D04 §15 | 미작성 |

파일명: `RBNN-kebab-case.md`. 각 Runbook은 탐지 신호, 영향 판단, 즉시 조치, 복구 확인, 사후 기록을 포함한다.

경보 규칙의 `runbook_url`은 runbook 안의 경보 이름 절(anchor)을 가리킨다. 경보를 추가하면 같은 PR에서 runbook 절을 추가한다.
