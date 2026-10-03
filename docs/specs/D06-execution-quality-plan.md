# D06 개발 실행과 품질 계획

> Montracer 개발 문서 세트 v2.0 (기준일 2026-10-03) · 원본: [`original/D06_Montracer_개발_실행과_품질_계획.docx`](original/D06_Montracer_개발_실행과_품질_계획.docx)  
> 이 파일은 `scripts/docs/convert_specs.py`로 생성한 파생본이다. 내용 변경은 원본 개정 + ADR로 한다.

개발팀이 요구사항을 백로그와 검증 증거로 전환하는 실행 문서다. Core 출시부터 고급 관측 기능까지 현실적인 인력과 단계별 Gate를 두고 확장한다.

### 사용 기준

제품 요구 F01~F27과 출시 범위는 D01을 기준으로 한다. 본 문서의 성능·보존·용량 수치는 구현 목표 또는 명시한 가정이며 실제 출시에는 D06 시험 증거와 승인 절차가 필요하다.

| **문서** | **읽는 목적**            |
|----------|--------------------------|
| D01      | 제품 요구사항과 벤치마크 |
| D02      | 시스템 데이터와 API 설계 |
| D03      | 계측과 고급 진단 설계    |
| D04      | 보안 운영과 상용화       |
| D05      | UX UI 디자인 명세        |
| D06      | 개발 실행과 품질 계획    |

### 참조 방법

D 번호는 문서, 절 번호는 각 문서의 목차 항목이다. B01~B16의 공개 벤치마크 원문은 D01 마지막 절, R1~R11의 표준과 엔진 근거는 D02 마지막 절에서 확인한다. 사용자·API·스키마·운영의 정의가 다르면 권위 문서를 수정한 뒤 계약 테스트를 갱신한다.

### 문서 상태

상용 구현과 인수 기준을 제안하는 설계 버전이다. 기능 완료, 보안 인증, 지원 계약 또는 확정 견적을 대신하지 않는다. 범위와 수치 변경은 revision 및 ADR로 기록한다.

## 목차

절 제목을 선택하면 해당 설계로 이동한다. 

[01 단계별 구현과 인력 계획](#01-단계별-구현과-인력-계획)

[02 실행 백로그와 완료 조건](#02-실행-백로그와-완료-조건)

[03 성능과 부하 시험](#03-성능과-부하-시험)

[04 테스트 전략과 품질 Gate](#04-테스트-전략과-품질-gate)

[05 고급 기능 시험과 인수 시나리오](#05-고급-기능-시험과-인수-시나리오)

[06 확장 부하 시험과 출시 Gate](#06-확장-부하-시험과-출시-gate)

[07 CI와 CD 파이프라인](#07-ci와-cd-파이프라인)

[08 ADR과 위험 기록](#08-adr과-위험-기록)

[09 상세 ADR과 위험 등록부](#09-상세-adr과-위험-등록부)

[10 로컬 환경과 레포 규약](#10-로컬-환경과-레포-규약)

[11 레포 확장과 개발자 경험](#11-레포-확장과-개발자-경험)

## 01 단계별 구현과 인력 계획

### 일정 가정과 책임

Core 전담 11~12명은 backend/data 4, frontend 2, agent 2, SRE 1, QA 1, PM/design 1~2로 구성한다. 보안·법무는 공유 자원이지만 보안 reviewer 없이 release하지 않는다. G2/G3는 agent·frontend·data·SRE 전문 인력을 포함해 16~20명 또는 일정 연장이 필요하다. 숫자는 채용·숙련도·범위에 따른 추정이며 완료 약속이 아니다.

| **단계** | **착수 기준 상대 기간** | **산출물과 Exit Gate** |
|----|----|----|
| P0 | 1~4주 | canonical schema·tenant boundary·OTLP·CH 부하 PoC |
| M0 | 5~16주 | F01~F07, 내부 pilot, trace→log 조사와 기본 경보 |
| G1 후보 | 17~32주 | F08~F14, 3AZ·삭제·복구·유료 원장·Java 진단 |
| G2 | 9~15개월 | F15~F20·F22, 모듈별 Beta→GA, 추가 안전 시험 |
| G3 | 16~24개월 이상 | F21·F23~F25·F27, replay와 CI·serverless |
| Scale 병행 | G1 이후 검증 기반 | F26, 10배 부하·Cell 이동·지역 DR |

G1이 지연되면 장기 기능을 먼저 출시해 매출을 보전하려고 보안·정확성을 생략하지 않는다. G2/G3의 Datadog 전체 제품 포트폴리오 동등성을 약속하지 않는다. 지속 프로파일과 원격 probe는 독립 release이며 한쪽의 GA가 다른 쪽의 안전성을 증명하지 않는다.

### 선후 관계

tenant identity → ingest ACK → canonical schema → dedup/metric → query → UI/monitor → usage/청구 순으로 안정화한다. Java Inspector는 기본 계측과 metadata 이후, thread dump는 권한·agent channel·approval 이후, RUM/replay는 consent·PII·삭제와 sandbox 이후다.

각 주기 demo는 기능 버튼보다 end-to-end 시나리오를 기준으로 한다. 고객 pilot 3곳에서 서로 다른 Java stack·Kubernetes topology를 선택한다. 데이터 측정에 고객의 실제 secret이나 개인정보를 넣지 않는다.

## 02 실행 백로그와 완료 조건

| **Epic** | **요구와 담당** | **선행과 완료 증거** |
|----|----|----|
| E01 Tenant와 Identity | F07 F08, API/Security | principal·RLS·IDOR suite·revoke 60초 |
| E02 Ingest와 Quality | F01 F10, Data | E01, OTLP golden·PII fixture·ACK crash |
| E03 Storage와 Query | F02 F04, Data/API | E02, dedup oracle·budget·cursor |
| E04 Core 조사 UI | F02 F03 F12, FE/Design | E03 mock, S02~S05 E2E·접근성 |
| E05 Monitor와 Dashboard | F05 F06 F11, API/FE | metric oracle, outbox·S09 S10 |
| E06 Java 진단 | F13 F14 F16, Agent | E01 E02, matrix·dump approval·kill |
| E07 운영과 상용화 | F09 F26, SRE/Platform | E01~E05, HA·DR·삭제·billing 대사 |
| E08 Profile와 DBM | F15 F17, Agent/Data | E06, unit 정합·plan redaction |
| E09 RUM와 Synthetic | F19~F21, FE/Platform | PII·runner 격리·consent fixture |
| E10 Integration과 확대 | F18 F22~F25 F27 | capability·pack CI·RCA 근거 |

### 첫 두 Sprint의 작업 분해

Sprint 1은 tenant principal과 error envelope, OTLP fixture decoder, ClickHouse layout 실험, PG outbox, UI shell·context tokens, CI·container digest pin을 병렬 작업한다. 완료는 demo tenant 2개에서 교차 조회가 거절되고 redacted fixture가 Kafka에 들어가는 상태다.

Sprint 2는 durable ACK와 retry test, trace/log identity, metric reset oracle, trace query와 waterfall mock, redaction failure path, staging health와 SLO skeleton을 구현한다. 완료는 Kafka append 직후 연결 단절 시험에서 보존 대상이 회복되고 logical 중복이 없는 상태다.

### 공통 DoD

코드·공개 API·DB migration·문서·단위/통합/negative test·metric·runbook·위험 검토·rollback 계획이 같은 PR 또는 연결된 issue에 있어야 한다. API 예제와 UI fixture는 schema에서 검증한다. 기능 flag off 경로도 시험하며 예외 승인에는 owner·만료·잔여 위험을 남긴다.

F01~F27 각각의 state와 release tag는 요구사항 registry에 기록한다. 완료 비율을 단순 endpoint 개수로 계산하지 않고 Gate 증거가 없는 기능은 planned/beta 상태로 둔다.

## 03 성능과 부하 시험

### 재현 가능한 Workload

generator는 trace 10k spans/s, log 5k/s, 100k 활성 metric series를 15초마다 전송한다. trace 길이는 평균 10 spans, p99 100 spans이며 1% error, 5% slow, payload 길이 분포와 service fan-out을 seed로 고정한다. trace ID와 log event ID ground truth를 별도 기록한다.

MVP head 10% 시험과 tail 대비 head 100% 시험을 별도 실행한다. 각 시험은 버전·CPU·RAM·disk·network·compression·quota·sampling 설정을 함께 저장한다. client가 target rate를 못 만들면 서버 처리 한계로 해석하지 않는다. open-loop generator로 요청을 예정 시각에 발사하여 지연 측정 누락을 줄인다.

| **시나리오** | **실행 조건** | **합격 기준** |
|----|----|----|
| 기준 부하 | 준비 15분, steady 60분 | D01 08절 지연·freshness, 내부 drop 0 |
| Burst | 3배 10분, 정상 20분 | backlog 15분 내 해소, OOM 0 |
| Soak | 기준 부하 24시간 | 메모리 누수 추세 없음, merge backlog 안정 |
| Noisy tenant | 한 고객 10배 15분 | 다른 고객 SLO 유지, quota 정확 |
| 저장 장애 | insert 차단 30분 후 복구 | ACK된 데이터 회복, 중복 집계 없음 |
| Dashboard | 사용자 20명, 12위젯, 30초 갱신 | p95 2초, timeout \<0.1% |
| Scale gate | 모든 신호 10배와 100 동시 query | sharded Cell 예산 안에서 SLO 유지 |

### 관측과 결과 판정

수집 ACK p99, query p95/p99, 실제 검색 freshness, CPU·heap·GC, 압축 전후 byte, Kafka lag, ClickHouse part·merge, sampler eviction, duplicate와 reject를 측정한다. accepted 이후 정상 보존 대상의 eventual unique count를 ground truth와 대조하고 sampling drop은 따로 분리한다.

tail 시험은 정상 baseline 10%의 통계적 범위를 확인하고 오류 trace의 보존율을 budget 미포화 조건에서 100%로 검증한다. budget 포화에서는 정책대로 떨어지고 UI 경고가 나타나는지 확인한다. late span과 rebalance를 동시에 주입하여 구조 불완전 비율을 측정한다.

### 용량 확정

테스트에서 측정한 span/log byte, metric series당 실제 저장량, histogram 분포, 압축률을 D04 06절 산식에 대입한다. 최저 SLO 통과 용량의 1.5배를 초기 배포 여유로 제안한다. 최종 구매·과금 단위는 측정 보고서와 함께 승인하며 본 설계의 가상 단가만으로 계약하지 않는다.

## 04 테스트 전략과 품질 Gate

### 계층별 검증

| **계층** | **필수 검증** | **실행 위치** |
|----|----|----|
| Unit | AST, metric reset, histogram 병합, RBAC, redaction | 모든 PR |
| Property와 fuzz | timestamp overflow, 압축·protobuf, Unicode, ID | PR smoke와 nightly 장기 실행 |
| Integration | Kafka replay, CH insert 재시도, PG RLS/outbox | merge 전 ephemeral stack |
| Contract | OTLP JSON/proto, OpenAPI schema, SDK 호환 | release matrix |
| End to end | 설치→trace→log→monitor→삭제 | staging 매일과 release |
| Security | tenant escape, SSRF, key revoke, XSS, secret fixture | P0는 PR, 전체는 release |
| Chaos와 DR | broker/AZ/downstream 중단, restore | 매월 일부, 분기 전체 |

### 데이터 정확성 Oracle

fixture generator가 source sequence와 기대 aggregation을 기록한다. 동일 batch를 3회 재전송해 trace와 metric logical 결과가 동일한지 확인한다. log event_id가 없는 경우는 중복 허용 계약을 별도 테스트한다. 상충 span과 metric은 최초 값 또는 quarantine 정책대로 처리되는지 확인한다.

metric test는 cumulative reset, out-of-order, duplicate delta, missing baseline, histogram 경계 차이, 단위 변경, NaN/Inf, 빈 bucket을 포함한다. p95는 알려진 distribution으로 오차 허용 범위를 검증하며 percentile 평균으로 구현했을 때 반드시 실패하는 fixture를 둔다.

### 장애 주입 지점

Kafka append 완료 직후 ACK 전 연결 단절, ClickHouse insert 성공 직후 offset commit 전 crash, sampler decision 직후 output 전 crash, outbox commit 직후 dispatcher crash를 각각 재현한다. 이 시점들은 단순 정상 path 테스트로 확인할 수 없는 중복·손실 경계다.

tenant 삭제 중 replay, backup restore 후 삭제 재적용, revoke 직후 SSE reconnect, policy cache miss 시 fail closed를 시험한다. data plane뿐 아니라 export 파일과 service autocomplete도 negative test에 포함한다.

### 출시 차단 기준

P0 결함 0개, cross-tenant 접근 0건, secret fixture 잔존 0건, 지정 부하 SLO 통과, 복구 RPO/RTO 실측, 삭제 proof 통과가 필요하다. coverage 수치는 핵심 함수 80%를 참고 기준으로 쓰되 위 시나리오 검증을 대신하지 않는다. 실패한 시험은 owner·원인·재시험 결과와 연결해 release checklist에 남긴다.

## 05 고급 기능 시험과 인수 시나리오

| **Case** | **조작과 데이터** | **합격 조건** |
|----|----|----|
| T01 JVM | JDK matrix별 GC·deadlock·async·restart | runtime reset·stale·boot_id 정확 |
| T02 Thread dump | 만료·중복 nonce·권한 철회·완료 request | 무승인 실행 0, 민감값 0 |
| T03 Profile | 알려진 CPU hot loop·alloc·두 배포 | flame top 함수·unit·sample coverage |
| T04 Probe | class mismatch·throw·TTL·offline·quota | 요청 실패 0, 만료 후 제거 |
| T05 DBM | stats reset·plan change·같은 literal 변형 | fingerprint 안정·literal 비저장 |
| T06 RUM | pending→grant→revoke·SPA·hidden tab | 미동의 전송 0, ID 회전·gap 정확 |
| T07 Replay | password·nested DOM·iframe·mutation | 모든 저장 계층에서 fixture 미검출 |
| T08 Synthetic | SSRF·DNS rebind·중복 slot·runner kill | 외부/사설 경계 유지, 이중 청구 0 |
| T09 Billing | retry·replay·늦은 event·plan 변경 | 단위별 대사·정정 이력·invoice hold |
| T10 UI | 10k span·200 node·키보드·dark·zoom | 성능 예산과 상태별 screenshot 회귀 |

### 통합 시나리오

checkout 1,000 requests 중 100개를 2초 이상으로 만들고 그중 20개를 오류로 만든다. 비샘플링 metric은 요청 1,000·오류 20·오류율 2%다. tail baseline 10%와 slow/error 보존이면 budget 미포화에서 slow/error 100개와 빠른 900개의 표본이 남는다. 예상 표본 수는 약 190개지만 deterministic seed 결과를 oracle로 확정한다.

trace slow DB span에서 normalized query로 이동하고 같은 instance의 profile overlap을 연다. 오류를 하나의 issue로 묶고 monitor 임계치를 1%로 설정해 경보를 발생시킨다. 경보 deep link는 고정 시간이며 log fallback에는 related 표시가 있다. 2% 초과 조건이면 정확히 2%는 발화하지 않는 경계 시험도 둔다.

tenant B가 각 ID·cursor·profile object·replay manifest를 재사용하면 데이터가 보이지 않아야 한다. tenant A 삭제 후 raw/rollup/index/object/export를 다시 조회하고 backup restore에도 tombstone이 적용되는지 검증한다. 인수 결과에는 version, workload seed, 실제 수치, 실패·재시험, 승인자를 기록한다.

## 06 확장 부하 시험과 출시 Gate

### Workload matrix

기본 10k spans/s·5k logs/s·100k active series 외에 profile 100 instances·RUM 1M events/day·DBM 100 instances·browser 12 slots를 별도 시험한다. 이후 신호를 함께 실행해 shared resource 경합을 확인한다. 평균만 맞추지 말고 3배 burst, hot tenant, large trace, 높은 histogram bucket 수를 포함한다.

agent 시험은 baseline → 기본 tracing → tracing+runtime → profile → probe 순으로 측정하고 동일 throughput·heap·warmup을 유지한다. throughput 하락·CPU 상대 증가·p99 악화·GC·startup 시간을 함께 기록한다. 샘플링을 낮춰 목표를 통과하면 그 설정이 지원 조건에 반영되어야 한다.

### Gate와 증거

| **Gate**  | **차단 조건**                                | **승인 역할**    |
|-----------|----------------------------------------------|------------------|
| Q0 계약   | schema·identity·API ambiguity 미해결         | Tech lead와 Data |
| Q1 보안   | tenant 노출·PII fixture·무승인 진단 1건 이상 | Security         |
| Q2 정확성 | duplicate 집계·잘못된 SLO·청구 불일치        | Data와 QA        |
| Q3 신뢰성 | 지정 부하 SLO·AZ failover·restore 실패       | SRE              |
| Q4 UX     | keyboard 과업 불가·상태 오표시·오류 숨김     | Design와 QA      |
| Q5 운영   | runbook·당직·rollback·지원 매뉴얼 부재       | 운영 책임자      |

단일 release tag에는 Gate별 증거 URI와 checksum을 붙인다. 본 문서의 목표를 관측했다고 간주하지 않으며 production 승인자가 결과를 확인한다. 예외는 보안·격리·삭제 보장을 약화시키지 않는 낮은 위험 항목만 제한 기한으로 허용한다.

### Canary와 중단

tenant 내부→pilot 1곳→5%→25%→100% 순으로 전개하고 각 구간 최소 24시간 또는 충분한 event 수를 확보한다. tenant escape·PII·logical data loss는 즉시 중단이다. error +0.5%p, latency p99 +20%, agent overhead budget 초과는 rollout freeze와 원인 분석을 트리거한다.

무조건 이전 binary만 되돌리면 안전하지 않을 수 있다. schema expand는 유지하고 writer feature flag를 먼저 끈다. irreversible 데이터 변환은 사전 snapshot과 복구 plan이 있을 때만 실행한다. 플랫폼 실패 synthetic는 고객 availability 계산에서 별도로 표시한다.

## 07 CI와 CD 파이프라인

### PR부터 배포까지

PR에서 format·lint·type check·unit·schema compatibility·dependency scan·secret scan을 실행한다. 변경 범위에 따라 ephemeral Kafka·ClickHouse·PostgreSQL integration을 병렬 수행한다. golden OTLP fixture와 cross-tenant test는 모든 ingest/query 변경에서 필수다.

main merge 후 reproducible container build, SBOM, provenance와 image signature를 생성한다. staging은 digest로 배포하며 DB expand migration을 먼저 적용한다. E2E·load smoke·synthetic를 통과하면 immutable release manifest를 만든다. production 승인은 역할 기반 release workflow로 기록한다.

| **Gate** | **자동 차단 조건** | **복구** |
|----|----|----|
| Contract | OpenAPI 또는 proto 비호환 | v2 도입 또는 호환 adapter |
| Security | exploitable critical 또는 secret | patch·교체 후 재검증 |
| Canary | error 증가 \>0.5%p, p99 20% 악화 | stateless rollback, data 영향 확인 |
| Data | accepted 회계 불일치, tenant escape | rollout 중단과 incident |
| Migration | old/new reader 호환 실패 | contract migration 연기 |

### Migration과 Rollback

API는 적어도 현재와 직전 schema version을 읽는다. 새 column은 nullable/default로 추가하고 writer가 배포된 뒤 backfill을 실행한다. 모든 reader가 새 필드를 이해한 후 오래된 필드를 제거한다. irreversible migration은 backup·restore 검증과 별도 점검 시간을 요구한다.

feature flag는 tenant allowlist로 Beta를 시작하고 sampling 변경은 shadow evaluation 결과를 비교한다. 수집 policy 변경이 기존 client의 프로토콜을 깨뜨리지 않아야 한다. outbox와 Kafka event는 schema_version을 포함하며 unknown additive field는 무시, unknown major는 quarantine한다.

### 개발 생산성과 책임

CODEOWNERS는 protocol, tenant boundary, PII policy, DB migration, cost-sensitive query에 전문 reviewer를 지정한다. protected branch는 필수 checks와 최소 1명 review를 요구하며 보안 경계 변경은 2명으로 강화한다. 문서의 API 예제와 schema fixture는 CI에서 parse해 drift를 막는다.

릴리스 노트에는 사용자 동작 변화, 설정 변경, migration, 알려진 한계와 rollback 가능 범위를 쓴다. 운영자가 확인할 dashboard·runbook URL을 release artifact에 포함한다. production 권한은 CI workload identity로 제한하고 개인 장기 credential을 사용하지 않는다.

## 08 ADR과 위험 기록

### 주요 의사결정

| **ADR** | **결정과 상태** | **대안 및 재검토 조건** |
|----|----|----|
| 001 | OTel 우선, 제안 승인 대기 | 자체 SDK는 누락 계측이 입증될 때 |
| 002 | Kafka ACK 경계, 제안 승인 대기 | 직접 DB 쓰기는 내부 단일 node만 |
| 003 | ClickHouse 3신호 통합, 제안 승인 대기 | PromQL·전문 검색 요구가 운영비를 넘으면 분리 |
| 004 | SDK metric을 SLO 원천으로, 권고 | sampled trace 통계의 선택 편향 회피 |
| 005 | tail은 stateful worker로, Beta 조건부 | Collector만 쓰는 경로는 crash/replay 검증 필요 |
| 006 | shared Cell+quota, 전용 Cell 선택 | 규제·보안·hot tenant이면 격리 강화 |
| 007 | UI 실시간은 SSE, 권고 | 양방향 협업 요구 때 WebSocket |
| 008 | 삭제 원장과 restore filter, 필수 | TTL만으로 즉시 삭제를 주장하지 않음 |

각 ADR은 배경, 결정, 후보, 이점, 비용, rollback, owner, 날짜, 증거 링크를 갖는다. 본 문서의 기본안은 설계 제안이며 실제 조직의 승인 이력을 임의로 만들어 넣지 않는다. 구현 시작 주에 001~003을 확정하고 변경 시 기존 ADR을 삭제하지 않고 superseded로 표시한다.

### 리스크 등록부

| **위험** | **가능성과 영향** | **완화와 책임** |
|----|----|----|
| metric cardinality 폭증 | 높음·높음 | 신규 series quota, 비용 preview, Data lead |
| sampling 편향·불완전 trace | 높음·중간 | SDK metric, completeness UI, Data lead |
| 테넌트·PII 유출 | 중간·매우 높음 | 다층 auth, fixture, fail closed, Security |
| storage 운영 부담 | 중간·높음 | 관리형 비교, backup·merge runbook, SRE |
| 배포판 설정 drift | 중간·중간 | digest pin, validate, SDK matrix, Platform |
| log 검색 기대 불일치 | 높음·중간 | 검색 capability 명시, pilot 검증, PM |
| 비용 산정 오차 | 높음·높음 | measured byte/point와 25% 예비비, SRE |

위험은 주간 검토에서 owner·trigger·기한·잔여 위험을 갱신한다. 2주 내 기술 검증이 필요한 항목은 sampler recovery, metric histogram 저장 byte, ClickHouse multi-tenant query latency다. 검증 결과가 기본안 가정을 깨면 구현량이 적더라도 ADR을 다시 연다.

## 09 상세 ADR과 위험 등록부

### ADR 009 Java 진단의 확장 방식

상태는 제안, owner는 Agent lead다. OTel 표준 계측을 기본으로 하고 Inspector·dump·method event를 opt-in extension으로 추가한다. 대안은 Pinpoint fork 또는 자체 전체 agent다. 기본안은 표준 연결과 재사용을 얻지만 custom 기능과 context bridge 유지비가 든다. 두 agent 동시 설치는 bytecode 이중 변환 위험 때문에 기본 금지하고 migration twin environment로 검증한다.

재검토 조건은 지원 고객의 필수 call-stack coverage 부족과 overhead 초과다. rollback은 extension off와 표준 tracing 유지다. source/license 재사용은 별도 검토이며 호환 protocol을 암묵적으로 약속하지 않는다.

### ADR 010 저장소 분리의 기준

trace/log/metric은 ClickHouse로 시작하고 profile·replay·artifact는 객체 저장소와 index를 사용한다. metric에 PromQL 호환이 필수이거나 고카디널리티 부하가 Core query SLO를 침해하면 전용 TSDB를 검토한다. full-text의 morphology·ranking 요구가 확정되면 검색 엔진을 추가한다. 추가 엔진은 삭제·DR·비용 원장을 함께 구현해야 한다.

### ADR 011 원격 진단과 개인정보

단방향 outbound 명령 수신, 승인·TTL·nonce·scope·boot 검증, 임의 실행 금지를 채택한다. 대안인 원격 shell은 지원 편의보다 침해 위험이 커 제외한다. 민감값을 수집 후 UI에서만 가리는 방식도 제외한다. rollback은 server와 agent 양쪽 kill switch, 이미 저장된 결과는 deletion job이다.

### ADR 012 디자인과 범위

서비스 중심 조사와 공통 context는 채택하지만 vendor 브랜드·화면 자산은 복제하지 않는다. M0/G1/G2/G3 단계는 D01이 권위이며 과거 24주 전체 완료 가정을 폐기한다. PM은 매 4주 pilot 가치와 구현 비용으로 범위를 조정하고 변경된 인수 조건을 함께 승인한다.

### 추가 위험

| **위험** | **발생 Trigger** | **완화와 책임** |
|----|----|----|
| Agent의 JVM 불안정 | class transform 오류·crash | 지원 matrix·canary·kill, Agent lead |
| 진단 경로 권한 남용 | 예상 외 probe·dump 요청 | dual approval·TTL·감사, Security |
| Replay 개인정보 | fixture 미마스킹 1건 | 기본 off·차단·삭제, Privacy owner |
| Scope 과확장 | G1 지연 4주 또는 인력 부족 | G2/G3 독립 투자, PM |
| 원가 폭증 | 신호당 원가 예산 120% | quota·retention·runner cap, FinOps |
| UI 조사 오판 | stale을 healthy로 표시 | 상태 contract·E2E, FE lead |

## 10 로컬 환경과 레포 규약

### 권고 Monorepo 구조

```
montracer/
  apps/web/ React와 TypeScript UI
  cmd/ingress/ OTLP 인증과 내구성 승인
  cmd/query-api/ query planner와 API
  cmd/control-api/ tenant와 정책 API
  cmd/worker/ normalize와 sink
  cmd/alert-worker/ evaluate와 notification
  internal/authz/ principal과 scope
  internal/telemetry/ canonical schema
  internal/query/ AST와 실행 예산
  internal/pipeline/ dedup와 checkpoint
  api/openapi/ management와 query schema
  api/proto/ OTLP pin과 내부 event
  migrations/postgres/
  migrations/clickhouse/
  deploy/compose/ 로컬 stack
  deploy/helm/ staging과 production
  infra/ network와 managed service IaC
  tests/fixtures/ golden OTLP와 가짜 PII
  tests/load/ generator와 workload manifest
  docs/adr/ 결정 기록
  docs/runbooks/ 운영 지침
```

### 최초 실행 계약

로컬 전제는 container runtime, lock된 Go·Node와 package manager, 8 CPU·16GB RAM·50GB disk를 권고한다. make bootstrap은 toolchain 검증과 env.example 복사, make up은 Kafka·ClickHouse·PostgreSQL·Collector를 띄운다. make migrate, make seed, make dev, make smoke 순으로 실행한다. 이 명령은 신규 레포가 구현해야 할 개발 경험 계약이다.

sample checkout→payment→database 서비스가 synthetic trace·log·metric을 만든다. UI에서 demo tenant로 첫 trace를 확인하고 make smoke가 correlation·monitor·tenant 격리를 확인한다. seed key는 localhost 전용이고 production image에는 포함하지 않는다. make down은 서비스만 종료하고 data 삭제는 별도 명시적 명령으로 분리한다.

### 코딩 규약

Go는 gofmt·정적 분석, context timeout과 취소 전파, 명시적 error wrapping을 적용한다. TypeScript는 strict mode와 lint·formatter를 고정한다. 함수는 tenant context를 명시적으로 받고 global mutable tenant 상태를 금지한다. 숫자 timestamp의 단위는 이름에 붙이고 외부 입력에 길이·범위 검증을 적용한다.

로그는 구조화하고 secret·payload를 포함하지 않는다. 재시도는 멱등성·backoff·총 시간 제한을 명시한다. 저장소 코드는 raw SQL을 handler에 흩뿌리지 않고 repository/query planner에 모은다. 테스트는 동작과 실패 경계를 검증하며 fixture에 실제 고객 데이터나 계정을 넣지 않는다.

## 11 레포 확장과 개발자 경험

### 모듈 구조

기본 montracer monorepo에 agents/java, agents/runtime, sdk/browser, cmd/diagnostics-broker, cmd/profile-worker, cmd/usage-worker, cmd/synthetic-runner, packages/design-tokens, packages/ui, packages/query-schema, integrations/packs를 추가한다. 제품 기능별 owner와 API·event schema를 registry에 등록한다.

apps/web/features는 services, traces, inspector, monitors, dashboards, profiles, dbm, rum, settings로 나눈다. UI package는 domain API를 직접 호출하지 않고 typed props와 event만 사용한다. backend는 tenant context가 없는 repository method를 금지하며 secret은 config object와 logging object에서 분리한다.

### 개발 환경 단계

lite compose는 PG·Kafka 단일 broker·ClickHouse·Collector·API·UI로 Core를 검증한다. full profile은 object store emulator·identity provider·diagnostic agent sample·synthetic runner를 추가한다. Docker socket을 runner에 mount하지 않는다. 로컬 .env에는 가짜 credential만 두고 CI가 secret pattern을 검사한다.

| **명령 계약** | **기대 결과** | **실패 시 안내** |
|----|----|----|
| make doctor | runtime·port·memory·version 확인 | 설치 경로와 부족 자원 |
| make up PROFILE=lite | 의존 서비스 healthy | 어느 service와 probe가 실패했는지 |
| make seed SCENARIO=checkout | 2 tenant와 알려진 장애 fixture | 중복 실행해도 logical 중복 없음 |
| make test-contract | API·proto·UI fixture 일치 | breaking field와 consumer 목록 |
| make test-isolation | query·stream·object·export 공격 시험 | 재현 ID, raw 고객 데이터 없음 |
| make demo-reset | 명시 승인 후 demo 데이터만 삭제 | production endpoint는 강제 거절 |

이 명령들은 신규 레포에서 구현해야 할 개발 경험 계약이며 이 문서 패키지 자체가 실행 서버를 포함하는 것은 아니다. 초기 bootstrap은 version lock과 hash 검증을 수행하고 최신 이미지 태그를 그대로 쓰지 않는다.

### 코드와 문서 변경 규약

브랜치·PR에는 요구 F ID와 Epic, schema 변경, tenant 영향, data-retention 영향, rollout plan을 붙인다. Conventional Commits는 선택할 수 있지만 보안 contract test가 우선이다. DB migration은 재실행·구버전 reader 호환을 시험하고 UI API 예제·OpenAPI·문서를 같은 PR에서 갱신한다. 새 query·probe·runner 기능은 abuse case 없이는 merge하지 않는다.
