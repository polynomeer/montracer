# cmd

배포 가능한 Go 서비스의 진입점(main). 비즈니스 로직은 `internal/`에 두고 여기서는 조립(wiring)만 한다.

| 서비스 | 역할 | 단계 |
|---|---|---|
| `ingress/` | OTLP 인증과 내구성 승인(ACK) | M0 |
| `query-api/` | query planner와 조회 API | M0 |
| `control-api/` | tenant·정책·설정 API (1차: 감사 조회, ADR 0034) | M0 |
| `worker/` | 정규화·dedup·sink·metric 집계 | M0 |
| `alert-worker/` | monitor 평가와 알림 (1차: 주기 평가·상태·전이 outbox, ADR 0051) | M0 |
| `diagnostics-broker/` | 승인된 진단 job과 agent channel | G1 Beta |
| `profile-worker/` | profile·replay 객체 정제와 manifest | G2 |
| `usage-worker/` | 불변 usage ledger와 대사 | G1 |
| `synthetic-runner/` | Synthetic 테스트 실행 | G2 |
| `migrate/` | PG·ClickHouse migration, Kafka topic 생성·검증 (운영 도구) | M0 |
| `platform-probe/` | 플랫폼 synthetic probe (공개 경로 블랙박스 감시, ADR 0031) | G1 |
| `montracer-admin/` | 운영자 break-glass 도구: tenant key 조회·폐기, 모든 시도 감사 (ADR 0033) | G1 |

- M0에서는 control-api와 query-api를 같은 binary로 운영할 수 있지만 package와 DB 소유권은 분리한다 (D02 §03).
- 여러 서비스가 같은 제어 테이블을 직접 수정하는 shared database 패턴 금지.
- 각 서비스 README에는 책임, 쓰기 소유 데이터, 장애 동작, 구현 위치, 환경 변수, 관련 ADR을 적는다. 구현이 바뀌면 같은 PR에서 고친다.
