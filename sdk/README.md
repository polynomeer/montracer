# sdk

| 경로 | 내용 | 단계 |
|---|---|---|
| `browser/` | Browser RUM SDK: Web Vitals, JS error, view ↔ backend trace 연결, consent 상태 머신 | G2 (F19), G3 replay (F21) |

- 동의하지 않은 세션은 전송하지 않는다. 허용 origin에만 trace context를 붙인다 (D03 §10).
- Backend 언어 SDK는 OTel 공식 SDK를 우선 사용한다 (ADR 001).
