# internal

서비스 간 공유하는 Go 패키지 (외부 import 불가).

| 패키지 | 책임 | 명세 |
|---|---|---|
| `authz/` | principal, tenant context, RBAC scope, step-up, API key 권한. 내부 system principal(읽기 전용 action만, alert-worker) | D04 §01~02, §12 · ADR 0015, 0051 |
| `controldb/` | 제어 DB(PostgreSQL) 접근. `WithTenant` 트랜잭션, RLS, key 조회, 과다 권한 계정 기동 거부. 서비스 catalog(목록·단건·이름 풀기·id → 이름). monitor 정의(revision 이력·tombstone·Idempotency-Key). alert-worker 저장소(할 일 찾기 scan 정책, slot lease, 경보 상태·평가 기록·전이 outbox) | D02 §11·§17 · ADR 0016, 0038, 0042, 0043, 0049, 0051 |
| `alertworker/` | alert-worker 평가 루프: tick·lease claim·tenant system principal 조회·`alerting.Evaluate`·완료 기록(사건 id 수명) | D02 §17 · ADR 0051 |
| `alerting/` | monitor 평가 의미(순수): window·조회·group 값·판정·상태 머신(alert-worker·dry-run 공유) | D02 §17·§21 · ADR 0050 |
| `metricvalue/` | rollup bucket의 연산 값·결측 사유(query-api와 alert-worker 공유) | D02 §10·§13 · ADR 0027, 0050 |
| `monitor/` | MonitorSpec 검증·정규형(control-api 저장 전 검증, alert-worker 평가가 공유) | D02 §14·§17 · ADR 0049 |
| `apierr/` | 관리·조회 API 공통 오류 envelope와 code | D02 §12, §19 · ADR 0014 |
| `httpapi/` | HTTP 경계: 오류를 한 번 변환·한 번 로그, panic 복구, request ID 발급 | ADR 0014 |
| `telemetry/` | `telemetry/otlp`: OTLP/HTTP bounded decode와 record 검증. `telemetry/redact`: 영속 저장 전 PII·secret 제거. `telemetry/envelope`: record 단위 Kafka envelope와 신호별 식별자 | D02 §04, §05, §07, §18 · D03 §02 · D04 §03 · ADR 0017, 0019, 0020 |
| `ingest/` | OTLP/HTTP 수신 경계: 인증·검증·environment 범위·redaction·quota·envelope·Kafka append 후 ACK | D02 §04, §22 · ADR 0002, 0020 |
| `quota/` | tenant·signal별 record·byte token bucket, overrides 파일 reload, metric 활성 series·label 값 상한(제어 DB 등록부 + replica cache) | D02 §10 · D04 §08 · ADR 0024, 0029, 0030 |
| `pipeline/` | 수집 worker: envelope 검증·정규화·(tenant, event_id) dedup·ClickHouse sink·offset commit·quarantine | D02 §05, §09~10, §21~22 · ADR 0021 |
| `metricagg/` | metric window 집계의 순수 계산(reset, cumulative 기준점, bucket 병합 percentile) | D02 §07, §10 · ADR 0025 |
| `rollup/` | metric 1분·1시간 rollup job: tenant별 watermark, 재계산, revision | D02 §10, §21~22 · ADR 0026, 0028 |
| `telemetrystore/` | ClickHouse 조회 계층: query 계정(읽기 전용 강제), tenant row policy, trace 단건·metric 조회(해상도 자동 선택)·metric 사전(이름별 유형 조합·label key)·log 검색·trace 검색(span 조건 → trace 요약) | D02 §09, §15 · ADR 0018, 0027, 0028, 0037, 0043, 0046 |
| `queryplan/` | filter JSON AST 검증·SQL 조각 컴파일(서버 측 parameter만). signal별 catalog(log, trace span·요약), `Split`(두 단계), `ParamPrefix`, `Bool` | D02 §15 · ADR 0037, 0039, 0043 |
| `query/` | 조회 API: 인증·인가 범위·응답 조립(trace 단건, metric QuerySpec, metric 사전·유형별 허용 연산, log·trace 검색, 서비스 목록·단건), tenant 실행 slot, 서명 cursor | D02 §12~15, §19 · ADR 0022, 0027, 0037, 0038, 0042, 0043, 0046 |
| `controlapi/` | 관리 API: 인증·입력 검증·서명 cursor. 감사 조회, monitor 정의(validate·CRUD, Idempotency-Key, If-Match) | D02 §14, §20 · ADR 0034, 0049 |
| `probe/` | 플랫폼 synthetic probe 검사: 공개 경로 수집·조회·redaction·격리 | D04 §10 · ADR 0031 |
| `opsmetrics/` | 플랫폼 자체 운영 지표(Prometheus, 별도 listener). 도메인 패키지의 Observer 구현 | D04 §10 · ADR 0023 |

규칙 (D06 §10~11)
- 함수는 tenant context를 명시적으로 받는다. 전역 mutable tenant 상태 금지, tenant 없는 repository method 금지.
- raw SQL은 repository / query planner에만 둔다.
- secret은 config object와 logging object에서 분리한다.
- 새 패키지를 추가하면 이 표에 책임·명세·ADR을 적고 `// Package` 주석을 단다. 구성 요소 간 흐름이 바뀌면 [아키텍처 문서](../docs/architecture/README.md)도 고친다.
