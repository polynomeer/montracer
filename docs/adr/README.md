# ADR (Architecture Decision Records)

설계 범위·수치·기술 선택의 변경은 ADR로 기록한다 (D01 §01, D06 §08).

## 규칙

- 파일명: `NNNN-kebab-case-title.md` (예: `0001-otel-first.md`). 번호는 D06의 ADR 번호를 그대로 쓴다.
- 새 ADR은 [template.md](template.md)를 복사해 작성한다. Claude Code에서는 `/adr` 스킬을 쓸 수 있다.
- 상태: `제안` → `승인` / `거절` → (변경 시) `대체됨(superseded by NNNN)`. **기존 ADR은 삭제하지 않는다.**
- 문서의 기본안은 설계 제안이다. 실제 승인자·날짜 없이 `승인` 상태로 만들지 않는다.
- 결정이 D01~D06 내용과 달라지면 같은 PR에서 권위 문서 개정 요청과 계약 테스트 변경을 연결한다.

## 등록부

D06 §08~09에 정의된 초기 ADR 후보. 구현 착수 주(P0 1주차)에 001~003을 확정한다.

| ADR | 주제 | 기본안 | 상태 | 원문 |
|---|---|---|---|---|
| 001 | 계측 표준 | OTel 우선, 자체 SDK는 누락 계측 입증 시 | 승인 | [0001-otel-first.md](0001-otel-first.md) |
| 002 | 수집 ACK 경계 | Kafka durable append 후 ACK (acks=all, RF3, min.insync=2) | 승인 | [0002-kafka-ack-boundary.md](0002-kafka-ack-boundary.md) |
| 003 | 분석 저장소 | ClickHouse에 trace·log·metric 통합 | 승인 | [0003-clickhouse-unified-store.md](0003-clickhouse-unified-store.md) |
| 004 | SLO 원천 | 비샘플링 SDK metric을 SLO 원천으로 | 권고 | D06 §08 |
| 005 | Tail sampling | stateful sampler worker (checkpoint/changelog) | Beta 조건부 | D06 §08, D02 §06 |
| 006 | 테넌트 격리 | shared Cell + quota, 전용 Cell 선택 | 제안 | D06 §08, D02 §02 |
| 007 | UI 실시간 | SSE (양방향 요구 시 WebSocket) | 권고 | D06 §08, D02 §16 |
| 008 | 삭제 | 삭제 원장 + restore filter | 필수 | D06 §08, D04 §04 |
| 009 | Java 진단 확장 | OTel 표준 + opt-in extension, 이중 agent 금지 | 제안 | D06 §09 |
| 010 | 저장소 분리 기준 | CH 시작, profile/replay는 object store, TSDB·검색엔진은 조건부 | 제안 | D06 §09 |
| 011 | 원격 진단과 개인정보 | outbound 명령, 승인·TTL·nonce·scope, 원격 shell 금지 | 제안 | D06 §09 |
| 012 | 디자인과 범위 | 독립 디자인, D01 단계 권위, 4주 단위 범위 조정 | 제안 | D06 §09 |
| 013 | 런타임·인프라 버전 고정 | Go 1.26, Node 24 LTS, pnpm 11, PG 17, Kafka 4.3, CH 26.8 LTS, Collector 0.161 (digest 고정) | 승인 | [0013-toolchain-and-image-pinning.md](0013-toolchain-and-image-pinning.md) |
| 014 | 오류 처리 설계 | 경계에서 한 번 변환·한 번 로그, INTERNAL(500) 추가, 요청 안전성 기반 retryable, 서버 발급 request ID | 승인 | [0014-error-handling.md](0014-error-handling.md) |
| 015 | API key 권한 수명·step-up·key 조회·운영 감사 | API key = 저장 scope ∩ 발급자 현재 role(ingest key는 조직 소유), step-up 15분, keys.read 분리, Operator는 audit.operations.read | 승인 | [0015-authz-key-and-role-details.md](0015-authz-key-and-role-details.md) |
| 016 | 제어 DB 접근 방식 | pgx v5, goose 라이브러리 + `cmd/migrate`(embed), `montracer_rw` 그룹, `WithTenant` 트랜잭션, key 인증용 한 행 RLS 정책 | 승인 | [0016-control-db-access.md](0016-control-db-access.md) |
| 017 | OTLP 수신 복잡도 한도·record 검증 | decode 전 pre-scan(요소 50만·깊이 32/64), envelope 1MiB 근사, 속성 한도를 모든 record·resource·scope에 적용, log 시간 보정 | 승인 | [0017-otlp-ingest-limits.md](0017-otlp-ingest-limits.md) |

확정된 ADR은 위 표의 `원문` 칸을 해당 ADR 파일 링크로 바꾸고 상태를 갱신한다.
