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
| 018 | ClickHouse 접근 계정·tenant row policy·migration | clickhouse-go v2, goose ClickHouse, 관리자·ingest·query 계정 분리, `SQL_montracer_tenant` row policy(미설정 시 0행), DEFINER lookup MV | 승인 | [0018-clickhouse-access.md](0018-clickhouse-access.md) |
| 019 | PII redaction 기본 정책과 실패 처리 | key deny 토큰·header 기본 deny·URL/SQL/IP 값 규칙·패턴(보완), record 단위 실패 격리, 알려진 한계 명시 | 승인 | [0019-pii-redaction-policy.md](0019-pii-redaction-policy.md) |
| 020 | OTLP ingress·Kafka envelope·신호별 식별자 | OTLP/HTTP, 처리 순서·ACK 경계, header+단일 record OTLP envelope, event_id·partition 식별자, franz-go, topic 설정, 로컬 Kafka 포트 안전장치 | 승인 | [0020-ingress-envelope-kafka.md](0020-ingress-envelope-kafka.md) |
| 021 | 수집 worker 정규화·dedup·ClickHouse sink | partition 단위 batch token + (tenant, event_id) record key, 최초 수신 값 유지(같은 ms는 Kafka offset, metric 상충 값은 quarantine — Prometheus·Mimir 방식), 자연 키 결정적 service_id(New Relic·Datadog 방식), 행 단위 DLQ(Uber·Kafka Connect 방식), 원문 없는 quarantine, durable insert 후 commit, token에 행 내용 해시(offset 재사용 유실 방지) | 승인 (빅테크 사례 기준 결정 위임) | [0021-ingest-worker-sink.md](0021-ingest-worker-sink.md) |
| 022 | trace 단건 조회 API | API key 인증, 보이는 span 없으면 존재 비노출 404(GitHub 방식), environment 제한 key의 범위 밖 span 제외, `missing_root`·`missing_parent`·`span_limit_reached` reason(Jaeger 방식), 모르는 meta는 null, span 상한 10,000 | 승인 (빅테크 사례 기준 결정 위임) | [0022-trace-query-api.md](0022-trace-query-api.md) |
| 023 | 플랫폼 운영 지표·경보·runbook | client_golang, binary별 별도 listener(:9464) pull, tenant·ID label 금지, ingress·worker 같은 단위 회계, D04 §10 초기 경보 11종(정체·ISR 포함) + promtool 시험, commit 후 계수, 경보별 runbook 절(RB01) | 승인 (빅테크 사례 기준 결정 위임) | [0023-platform-ops-metrics.md](0023-platform-ops-metrics.md) |
| 024 | 수집 tenant quota | tenant·signal별 record·byte token bucket(Mimir·Tempo 기본값), rate 초과 429+Retry-After, burst 초과 413, 과부하 503, 한도÷replica(global), overrides 파일 10초 reload. fair queue·신규 series quota는 후속 | 승인 (빅테크 사례 기준 결정 위임) | [0024-ingest-tenant-quota.md](0024-ingest-tenant-quota.md) |
| 025 | metric window 집계 의미 | 관측 시각 배정, 값 감소·start_time 변경 = reset(0부터, Prometheus 방식), 첫 cumulative는 기준점만(OTel cumulativetodelta), NaN·불일치 제외와 품질 사유, bucket 병합 후 선형 보간 p95, 경계·단위 다르면 병합 오류 | 승인 (빅테크 사례 기준 결정 위임) | [0025-metric-window-semantics.md](0025-metric-window-semantics.md) |
| 026 | metric 1분 rollup job | metric_1m(revision, 빈 window는 행 없음), tenant별 watermark = 관측 − 2분 + idle 60초(Flink 방식), 처리 위치부터 따라잡기·10분 재계산·바뀐 window만 씀, 단조 revision, rollup 계정(전 tenant metric 읽기 명시 정책), worker rollup 역할 단일 실행(Thanos compactor 방식) | 승인 (빅테크 사례 기준 결정 위임) | [0026-metric-rollup-job.md](0026-metric-rollup-job.md) |
| 027 | metric 조회 API | JSON QuerySpec(Honeycomb 방식, 언어 없음), step 경계 정렬(Grafana 방식), ClickHouse 안 (group, step) 집계·bucket 원소별 합, 모든 step에 null+사유·completeness·missing 구간, metric_1m에 label 저장 | 승인 (빅테크 사례 기준 결정 위임) | [0027-metric-query-api.md](0027-metric-query-api.md) |
| 028 | metric 1시간 rollup과 해상도 자동 선택 | metric_1h(395일)를 원본에서 같은 계산으로, step이 1시간 배수면 metric_1h 읽기(Thanos auto downsampling 방식), 해상도별 정체 경보 | 승인 (빅테크 사례 기준 결정 위임) | [0028-metric-hourly-rollup.md](0028-metric-hourly-rollup.md) |
| 029 | metric cardinality quota | 금지 dimension·label 20개는 redaction 전 key로 판정, 활성 series 상한 100k(Mimir 방식 override)·기존 series 계속 수용, 제어 DB 공유 등록부 + replica cache, point 단위 partial success, 등록부 장애 503 | 승인 (빅테크 사례 기준 결정 위임) | [0029-metric-cardinality-quota.md](0029-metric-cardinality-quota.md) |
| 030 | metric label key당 활성 값 상한 | 신규 series만, (metric, key)당 활성 값 100(D02 §10), 값은 redaction 뒤 해시만 등록부에, 기존 series 갱신 시 값도 갱신. 공개 사례 없음(명세 직접 요구) | 승인 (결정 위임) | [0030-metric-label-value-quota.md](0030-metric-label-value-quota.md) |
| 031 | 플랫폼 synthetic probe | 별도 binary, probe tenant로 공개 경로 1분 주기(3-span trace·log·exemplar), signal별 수집·60초 조회·redaction·격리 검사, 평가 못한 검사는 blocked(실패로 세지 않음), check별 2회 연속 실패 page. 근거 Google SRE black-box, Datadog Synthetic 경보 조건 | 승인 (결정 위임) | [0031-platform-synthetic-probe.md](0031-platform-synthetic-probe.md) |
| 032 | 조회 값이 query_log·로그에 남지 않게 | 사용자 값은 서버 측 query parameter(`{name:Type}`)로 binding, ClickHouse가 parameter를 치환해 query_log에 남기므로 `query_masking_rules`로 문자열 literal을 `'?'`로 바꿈, query_log 행 전체 통합 시험. 근거 Datadog DBM·Cloud SQL Query Insights·OTel db.query.text sanitization | 승인 | [0032-query-log-value-masking.md](0032-query-log-value-masking.md) |
| 033 | 운영자 break-glass key 조회·폐기 | 일반 principal과 분리된 grant(사유·승인자≠운영자·ticket·30분·allowlist 2개), 앱 role+RLS로 tenant 범위만, 모든 시도를 대상 tenant security 감사(actor operator)에, 폐기는 `key.revoked` 재사용·outbox엔 revoke_at만. 근거 Google Access Transparency·Approval, Microsoft Customer Lockbox, AWS SEC03-BP03 | 승인 (결정 위임) | [0033-operator-break-glass-key-revocation.md](0033-operator-break-glass-key-revocation.md) |
| 034 | 감사 조회 API와 서명 page cursor | `GET /api/v1/audit-events`(control-api 첫 진입점), 범주 권한(ADR 0015 §4, 권한 없는 범주 명시 요청은 403), keyset `(occurred_at,id)`, HMAC 서명 cursor(tenant·권한 fingerprint·query hash·snapshot·만료 15분, limit 제외), 운영자 행의 직원 ID 비공개. 근거 Google AIP-158, Cloud Audit Logs 열람 권한 분리 | 승인 (결정 위임) | [0034-audit-events-api.md](0034-audit-events-api.md) |
| 035 | metric rollup backfill job | `worker backfill --tenant --from --to`, live job과 같은 설정·계산(`compute`, 기준점은 lookback 안만), tenant 한정 읽기, live 재계산 구간과 겹침 금지·원본 보존 여유, chunk point 상한·pacing, 재실행 멱등(chunk 무관), 경보 자동 취소 없음, 자동 실행은 job 원장과 함께. 근거 Prometheus recording rule backfill | 승인 (결정 위임) | [0035-metric-rollup-backfill.md](0035-metric-rollup-backfill.md) |
| 036 | OTLP/gRPC 수신 | HTTP와 같은 처리 core(`process`), raw codec으로 byte를 받아 같은 `otlp.Decode`(한도·pre-scan 동일), 압축 해제 후 8MiB, 상태 코드는 OTLP 규격(rate 한도만 RESOURCE_EXHAUSTED+RetryInfo, 과부하는 UNAVAILABLE), `:4317`, `transport` 지표 label. 근거 OTLP 규격, Honeycomb | 승인 (결정 위임) | [0036-otlp-grpc-ingress.md](0036-otlp-grpc-ingress.md) |
| 037 | query planner와 log 검색 | `internal/queryplan`(AST 연산자·깊이 4·조건 20·in 100, catalog, 서버 측 parameter만, mandatory predicate는 저장소), `POST /query`·`/query/logs`(log catalog, `LIMIT 1 BY` dedup, keyset, version 기반 수신 snapshot, 24h·limit 1000, env 제한 key 403), tenant별 동시 5+대기 20. 근거 Datadog Logs Search | 승인 (결정 위임) | [0037-query-planner-log-search.md](0037-query-planner-log-search.md) |
| 038 | 서비스 catalog | 제어 DB `services`(RLS, service_id = worker와 같은 결정적 UUID), ingress가 ACK 뒤 비동기 등록(5초 flush·5분 cache·queue 초과 시 버림), 앱 role은 자동 필드만 UPDATE(owner 보호를 권한으로), 상태는 조회 시 계산, `GET /api/v1/services`(keyset cursor, env 제한). 근거 Datadog Software Catalog | 승인 (결정 위임) | [0038-service-catalog.md](0038-service-catalog.md) |
| 040 | 디자인 토큰 패키지와 명세 밖 토큰 값 | TS 원천(값마다 D05/시안 출처) → `--mt-*` CSS 생성·커밋, `prefers-color-scheme` 기본 + `data-theme` 우선, 대비 단위 시험(본문 4.5:1·그래픽 3:1), 의존성 없이 Node type stripping·`node:test`. 근거 GitHub Primer primitives, IBM Carbon themes, WCAG 2.2 | 승인 (결정 위임) | [0040-design-tokens.md](0040-design-tokens.md) |
| 041 | web app shell과 frontend 도구 | Vite·React 19·React Router·TypeScript 7·Vitest(버전은 ADR 0013), 조사 context는 URL query가 원천(`env`·`range`|`from`/`to`·`tz`, timezone 없는 시각 거절), 메뉴 이동은 context만, 공유 링크는 UTC 절대시간·허용 키만, D05 정보 구조(Experience는 entitlement), 1440px 미만 drawer, Pretendard 로컬 번들. 근거 Grafana URL time range, Datadog 공유 링크, React 공식 문서 | 승인 (결정 위임) | [0041-web-app-shell.md](0041-web-app-shell.md) |

확정된 ADR은 위 표의 `원문` 칸을 해당 ADR 파일 링크로 바꾸고 상태를 갱신한다.
