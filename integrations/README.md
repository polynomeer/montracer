# integrations

| 경로 | 내용 | 단계 |
|---|---|---|
| `packs/` | 통합 팩(Kafka, DB, 클라우드, CI 등) 정의와 capability 선언 | G1 기본, G2~G3 확장 (F18, F22~F24) |

- 각 팩은 CI에서 검증하고, 지원하지 않는 기능은 capability 차이로 표시한다.
- 외부 connector는 별도 pool에서 격리해 주 수집 plane을 재시작시키지 않는다 (D02 §03).
