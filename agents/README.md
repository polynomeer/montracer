# agents

고객 환경에서 동작하는 계측 agent (D03, ADR 009).

| 경로 | 내용 | 단계 |
|---|---|---|
| `java/` | OTel Java agent 기반 opt-in extension: JVM Inspector, call tree, thread dump, probe | G1 (F13, F14), G2 (F16) |
| `runtime/` | node/host agent, runtime metadata 수집 | G1 (F18) |

- 첫 지원 후보: JDK 17·21, Spring Boot 3, JDBC, HTTP, Kafka. JDK 8·11은 G2 별도 트랙 (D03 §01).
- 다른 bytecode agent와 동시 설치는 기본 금지.
- 원격 설정은 data-only. shell 실행이나 임의 jar 다운로드 금지.
- 부하 예산: CPU 상대 증가 ≤3%, 요청 p99 ≤5% (지원 matrix별 실측).
