# D01 제품 요구사항과 벤치마크

> Montracer 개발 문서 세트 v2.0 (기준일 2026-10-03) · 원본: [`original/D01_Montracer_제품_요구사항과_벤치마크.docx`](original/D01_Montracer_제품_요구사항과_벤치마크.docx)  
> 이 파일은 `scripts/docs/convert_specs.py`로 생성한 파생본이다. 내용 변경은 원본 개정 + ADR로 한다.

제품 책임자와 기술 책임자가 상용 범위와 인수 기준을 확정하는 문서다. 핵심 APM과 JVM 진단을 G1로 만들고 고급 관측 기능을 G2와 G3로 확장한다.

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

[01 제품 비전과 문서 운영](#01-제품-비전과-문서-운영)

[02 설계 기준과 핵심 결정](#02-설계-기준과-핵심-결정)

[03 제품 요구사항과 사용자 여정](#03-제품-요구사항과-사용자-여정)

[04 벤치마크 적용 기준](#04-벤치마크-적용-기준)

[05 핵심 기능 요구사항](#05-핵심-기능-요구사항)

[06 확장 기능과 비범위](#06-확장-기능과-비범위)

[07 사용자 여정과 상용 인수](#07-사용자-여정과-상용-인수)

[08 비기능 요구사항과 SLO](#08-비기능-요구사항과-slo)

[09 공식 근거와 검토 관리](#09-공식-근거와-검토-관리)

## 01 제품 비전과 문서 운영

### 제품 결정

Montracer는 서비스 상태 파악에서 요청 단위 원인 조사, JVM 내부 진단, 사용자 경험 확인까지 연결하는 B2B 관측 플랫폼이다. Datadog의 신호 통합과 탐색 흐름, Pinpoint의 Java 중심 깊이 있는 진단을 벤치마킹한다. 두 제품의 전체 포트폴리오를 한 번에 복제하지 않고, 검증 가능한 기능 단위로 제품화한다.

초기 구매자는 Java와 Kubernetes를 함께 운영하는 20~200명 규모 개발 조직이다. SRE가 장애 영향을 좁히고 개발자가 코드·SQL·스레드 원인을 확인하는 공동 작업을 핵심 경험으로 정한다. 클라우드 SaaS를 먼저 제공하고, 전용 Cell과 사설망 설치는 별도 운영 모델로 확장한다.

### 문서 세트와 권위

| **문서** | **주요 독자** | **결정 범위** |
|----|----|----|
| D01 제품 요구사항과 벤치마크 | PM와 기술 책임자 | 출시 범위, 기능 ID, 목표와 비범위 |
| D02 시스템 데이터와 API 설계 | Backend와 Data | 내구성, 스키마, 검색, 이벤트와 API |
| D03 계측과 고급 진단 설계 | Agent와 Platform | SDK, JVM, DBM, 프로파일, RUM과 Synthetic |
| D04 보안 운영과 상용화 | SRE와 Security | 격리, 삭제, HA, 비용, 과금과 지원 |
| D05 UX UI 디자인 명세 | Design와 Frontend | 정보 구조, 화면, 토큰, 상호작용과 접근성 |
| D06 개발 실행과 품질 계획 | 전 개발팀 | 일정, 백로그, 테스트, ADR과 출시 Gate |

문서 버전은 2.0, 기준일은 2026년 10월 3일이다. 기능 단계는 D01이, 저장·API 의미는 D02가, 개인정보 정책은 D04가 권위 문서다. 불일치 발견 시 구현자가 임의 선택하지 않고 ADR과 계약 테스트를 함께 수정한다. 이전 통합 설계서의 제외 범위와 24주 전체 일정은 이 세트로 대체한다.

### 설계와 구현 완료의 구분

이 문서는 상용 제품의 구현·검증 기준이다. 기능이 이미 구현되었거나 보안 인증을 획득했다는 뜻이 아니다. 수치에는 목표 또는 산정 가정을 표시하며, 운영 전 부하·복구·고객 인수 시험 결과로 확정한다. 출시 상태는 planned, experimental, beta, GA, deprecated로 관리하고 beta 기능을 계약 보장 범위에 자동 포함하지 않는다.

## 02 설계 기준과 핵심 결정

### 목표와 적용 환경

개발자는 배포 후 지연과 오류를 서비스 수준에서 발견하고, 동일한 시간·서비스·trace 맥락으로 원인을 조사할 수 있어야 한다. 고객은 수집량·보존기간·샘플링을 직접 관리하고 비용 및 데이터 손실 상태를 확인한다. 성공은 기능 수보다 설치 시간, 조사 시간, 수집 신뢰성과 격리 검증으로 판단한다.

본 기본안은 B2B SaaS, Kubernetes 또는 VM 고객 환경, 단일 데이터 거주 지역을 가정한다. 서비스별 하루 트래픽 편차가 크며, SQL 트랜잭션보다 시계열 추가 쓰기가 압도적으로 많다. 온프레미스는 Helm 패키지를 재사용하되 별도의 지원 제품으로 취급한다.

| **항목**      | **MVP 기준**                 | **운영 확장 기준**          |
|---------------|------------------------------|-----------------------------|
| 조직과 사용량 | 10 테넌트, 100 서비스        | 100 테넌트, 1,000 서비스    |
| 수집 부하     | 평균 10k spans/s, 3배 피크   | 평균 100k spans/s, 3배 피크 |
| 로그와 metric | 5k logs/s, 활성 100k series  | 50k logs/s, 활성 1M series  |
| 배치와 보존   | metric 15초, trace와 log 7일 | 동일한 기본값, 계약별 확장  |
| 사용자 부하   | 동시 20 쿼리                 | 동시 100 쿼리, Cell별 예산  |

### 변경 불가 계약

인증으로 얻은 tenant_id만 신뢰한다. 고객 payload의 tenant_id와 X-Tenant-ID는 권한 근거가 아니다. 수집 승인 ACK는 정제된 데이터가 Kafka에 내구성 있게 기록된 이후에만 반환한다. ACK는 검색 완료, 영구 저장 또는 모든 trace 보존을 의미하지 않는다.

전체 경로는 at-least-once로 설계한다. 중복 제거는 신호별 식별자와 재처리 체크포인트로 해결하며, Kafka의 보장을 외부 저장소까지 확장해서 exactly-once라고 부르지 않는다. 개인정보 제거는 최초 영속 저장 이전에 수행한다.

### MVP와 운영의 경계

로컬 개발은 단일 노드, 내부 MVP는 단일 지역의 축소된 복제 구성으로 시작할 수 있다. 고객 유료 production은 3 AZ Kafka, ClickHouse 복제, PostgreSQL HA 및 복구 훈련을 충족해야 한다. 축소 MVP의 가용성은 production SLO와 구분해 공개한다.

이 절의 규모는 Core 부하 기준이고 확장 신호는 D04 추가 모델을 적용한다. 모든 표의 성능과 비용은 목표·가정이다. 정확한 버전과 클라우드 인스턴스는 초기 기술 검증에서 잠그고, D06 03절의 부하 시험으로 처리량과 압축률을 측정한 뒤 조정한다. 공식 표준 사실은 \[R1\]부터 \[R11\]로 표시한다.

## 03 제품 요구사항과 사용자 여정

### 사용자와 문제

| **사용자** | **핵심 과업** | **성공 기준** |
|----|----|----|
| 애플리케이션 개발자 | 느린 요청과 예외 원인 조사 | 오류 trace에서 관련 log까지 3단계 이하 |
| SRE와 당직자 | 오류 예산과 서비스 장애 대응 | 경보에서 영향 서비스와 runbook 바로 접근 |
| 플랫폼 관리자 | SDK 배포, 서비스 소유권 관리 | 신규 서비스 수집 확인까지 30분 이내 |
| 보안 관리자 | 접근 감사, 민감정보 유출 방지 | 권한 변경과 삭제 작업을 증거와 함께 조회 |
| 조직 관리자 | 사용량과 보존 비용 관리 | 일별 신호별 사용량과 제한 상태 확인 |

### 대표 유스케이스

UC01 설치: 조직 관리자가 ingest 전용 키를 발급한다. 개발자는 환경별 resource 속성을 설정하고 테스트 요청을 보낸다. 연결 진단은 인증, 수집, 처리, 검색 단계를 별도로 표시한다. 실패 시 마지막 성공 시각과 거절 이유를 제공하되 비밀키를 표시하지 않는다.

UC02 장애 조사: 오류율 경보에서 서비스 상세로 이동하고 배포 이벤트와 p95 변화를 비교한다. 사용자는 실패 trace의 critical path, DB span, 연결된 log를 확인한다. 샘플링 또는 보존 만료로 원본이 없으면 이유를 표시하고 인접 시간의 metric 탐색을 제공한다.

UC03 비용 제어: 관리자는 조직별 수집 상한과 trace 보존 비율을 바꾼다. UI는 적용 예상 시점과 통계 영향, 최근 24시간 기준 절감 추정치를 보여준다. 비용 상한 도달 시 어떤 신호가 제한되는지 경고하고 정책 변경을 감사 로그에 기록한다.

### 제품 지표와 출시 검증

첫 trace 검색까지의 시간은 설치 시작부터 검색 가능 상태까지 측정한다. 목표 중앙값은 15분, p90은 30분이다. 개발자 5명 이상이 샘플 서비스에 설치하는 사용성 시험으로 검증한다. 원인 조사 시간은 고정 장애 시나리오 5개로 기존 로그 조사 방식과 비교하고 중앙값 30% 감소를 목표로 한다.

지표는 제품 도입 목표이며 시스템 SLO와 별개다. 주간 활성 조사 사용자, 경보 후 trace 탐색 전환율, 미소유 서비스 비율, 사용량 초과 빈도를 함께 본다. 수집 데이터 양만으로 성공을 판단하지 않는다.

## 04 벤치마크 적용 기준

### 공식 자료에서 확인한 관찰

Datadog 서비스 상세는 요청·지연·오류, 리소스, 의존성과 배포·오류 정보를 한 맥락에서 제공한다. Trace Explorer는 검색 결과를 조사 대상으로 연결하고 dashboard 변수와 context link는 탐색 범위를 유지한다. 이를 서비스 중심 정보 구조와 공유 필터 설계의 근거로 삼는다. \[B01, B02, B03, B04\]

Pinpoint 공식 개요는 ServerMap, 요청 응답 scatter, CallStack과 CPU·메모리·GC Inspector를 설명한다. 실시간 요청 진단 문서는 필요할 때 활성 요청과 스레드 정보를 요청하는 경로를 설명한다. 이를 상시 저비용 계측과 승인된 일시 진단의 분리 근거로 삼는다. \[B05, B06\]

### 기능 비교와 구현 방향

| **영역** | **공개 벤치마크 근거** | **Montracer 결정** |
|----|----|----|
| 서비스와 의존성 | Datadog 서비스 상세, Pinpoint ServerMap | 공통 service ID와 관측 신뢰도 표시 |
| 요청 조사 | Trace Explorer, Pinpoint scatter와 CallStack | histogram 영역 선택에서 waterfall과 call tree 연결 |
| 런타임 내부 | Pinpoint Inspector와 실시간 요청 | JVM 지표는 상시, thread dump는 승인 기반 |
| 코드 병목 | Datadog trace와 profile 연결 | 프로파일 표본과 span 이벤트를 구분 |
| 사용자 경험 | Datadog RUM와 Synthetic | 브라우저→backend 연결, 테스트 트래픽 별도 집계 |
| 운영 제어 | Datadog dashboard와 권한 | 정책·쿼터·보존·조회 권한을 서버에서 강제 |

### 비교의 한계와 지식재산 경계

이 표는 기능 존재와 공개 동작에 대한 비교다. 양 제품의 최신 모든 SKU·라이선스·성능을 검증한 동등성 평가가 아니다. 공식 문서에서 확인되지 않은 기능을 미지원으로 단정하지 않는다. Pinpoint 항목의 일부 근거는 버전별 문서이며 호환성은 배포 전 다시 검증한다.

Montracer는 독립 명칭·색상·아이콘·컴포넌트를 사용한다. Datadog 로고, 보라색 브랜드 구성, 문구·화면 자산을 복제하지 않는다. 오픈소스 코드 재사용 시 파일별 LICENSE와 NOTICE 및 배포 의무를 검토하고 SBOM에 기록한다. 공개 UI 개념의 참고와 소스 코드 재사용 승인은 별도다.

## 05 핵심 기능 요구사항

### 제품 단계

M0는 내부 MVP, G1은 유료 Core GA, G2는 Advanced APM, G3는 Full Stack 확장이다. Scale은 기능 단계가 아니라 G1 이후 적용하는 용량·지역 확장 트랙이다. Beta는 각 기능의 제한된 고객 시험 상태다. 모든 요구사항은 구현 이후 기능별 Gate를 통과해야 GA가 된다.

| **ID** | **기능과 검증 가능한 인수 기준** | **목표** |
|----|----|----|
| F01 | OTLP 3신호 수집, retry·partial success·tenant 위조 시험 통과 | M0 |
| F02 | trace waterfall, span 상세, 구조 불완전 사유와 log 연결 | M0 |
| F03 | 서비스 RED·목록·소유자·map, client/server 중복 계수 없음 | M0 |
| F04 | typed 필터·facet·저장 검색·공유 URL, 재방문 context 유지 | M0 |
| F05 | dashboard 6종 위젯, 변수, revision 충돌 검출 | M0 |
| F06 | 임계치 monitor, webhook, no data와 query 실패 구분 | M0 |
| F07 | OIDC·조직 RBAC·키·PII·quota·사용량 조회 | M0 |
| F08 | 팀과 환경 scope, SAML·SCIM, 조회 감사와 step-up | G1 |
| F09 | HA·삭제 proof·월간 SLO·유료 과금 대사·지원 절차 | G1 |
| F10 | tail sampling, 결정 복구, budget drop와 late span 공개 | G1 |
| F11 | SLO burn·복합 monitor·mute·escalation·사건 timeline | G1 |
| F12 | 요청 scatter와 URI 통계, 영역 선택이 같은 필터로 재현 | G1 |
| F13 | JVM Inspector·활성 요청, 만료와 stale 상태 노출 | G1 |
| F14 | call tree·SQL template·예외 chain·오류 그룹화 | G1 |

G1은 Pinpoint의 주요 조사 흐름을 포괄하되 임의 메서드 전체 계측을 기본 활성화하지 않는다. 계측되지 않은 메서드는 존재하지 않는 것으로 간주하지 않고 coverage 정보를 보여준다. G1 thread dump는 제한 Beta로 제공하고 G2에서 지원 JVM matrix와 안전 예산을 충족한 뒤 GA로 전환한다.

### 우선순위 원칙

P0는 고객 간 격리·정확성·PII·내구성·삭제와 복구다. P1은 위 조사 기능 및 운영 경험이다. P2는 확장 통합과 생산성이다. 일정이 밀리면 P1·P2의 고객 수나 플랫폼 범위를 줄이지 P0를 생략하지 않는다.

## 06 확장 기능과 비범위

| **ID** | **구현 범위와 합격 기준** | **단계와 근거** |
|----|----|----|
| F15 | CPU·allocation 프로파일, flame graph·배포 비교·trace overlap | G2, B07 |
| F16 | 승인된 Java thread dump와 제한 probe, 만료·kill switch | G2, B06 B08 |
| F17 | PostgreSQL·MySQL DBM, query digest·wait·plan 비교 | G2, B09 |
| F18 | host·container·Kubernetes와 서비스 연결, metadata TTL | G1 기본 G2 확장 |
| F19 | Browser RUM, Web Vitals·JS error·view와 trace 연결 | G2, B10 |
| F20 | API Synthetic, 사설 runner, 장애와 runner 중단 구분 | G2, B11 |
| F21 | Browser Synthetic와 session replay, 마스킹 회귀 통과 | G3, B11 B12 |
| F22 | Kafka lag·message pathway·처리 지연, payload 미수집 | G2, B13 |
| F23 | 배포·CI pipeline와 test 연결, flaky test 분류 | G3, 제품 확장안 |
| F24 | serverless cold start·클라우드 API 통합, credential 격리 | G3, 제품 확장안 |
| F25 | anomaly·change detection·RCA 가설, 근거와 신뢰도 | G3, 제품 확장안 |
| F26 | 전용 Cell·사설망 패키지·DR·감사 export | Scale, 독립 운영 설계 |
| F27 | LLM span·token·비용 metadata, prompt 기본 미수집 | G3, 제품 확장안 |

### 영구 또는 별도 제품 비범위

SIEM 전체, CSPM, 취약점 스캐너, WAF·RASP 차단, 전체 network packet 저장, cloud cost management 전체, 업무 BI, 결제 실행은 이 제품 범위 밖이다. 보안 이벤트 수집 connector는 가능하지만 보안 제품과 같은 탐지 보장을 하지 않는다. 모바일 native RUM은 Browser RUM의 후속 투자 항목이며 G3 필수 Gate가 아니다.

PromQL 전체 호환과 Datadog·Pinpoint agent wire protocol drop-in 호환은 보장하지 않는다. 계측된 서비스의 공통 표준 데이터와 설정 이관을 지원하고 독점 기능은 capability 차이로 표시한다. 고객 SQL 실행·운영 프로세스 임의 명령·무승인 자동 복구는 제공하지 않는다.

### 지원 모델

UI/API는 한국어와 영어를 제공하며 UTC 저장과 사용자 timezone 표시를 분리한다. 사설망 제품은 라이선스 검증이 일시 실패해도 기존 수집·조회·삭제를 즉시 중단하지 않는다. 상용 지원 버전·SLO·요금은 주문서의 entitlement로 관리하며 가상의 경쟁사 가격을 사용하지 않는다.

## 07 사용자 여정과 상용 인수

### 핵심 시나리오

UC04 JVM hang: SRE가 p95 증가를 발견하고 같은 instance의 active request age와 blocked thread 증가를 확인한다. Operator가 사유를 입력해 dump를 요청한다. 이미 끝난 요청에는 completed_before_capture를 반환한다. 민감 인자 없이 lock owner와 상위 frame을 보고 관련 trace에 조사 메모를 남긴다.

UC05 DB regression: 배포 후 checkout latency가 증가하면 이전 배포와 traffic mix를 비교한다. slow span의 db fingerprint로 DBM에 이동하고 wait 종류와 plan 변화를 확인한다. query 실행권은 부여하지 않으며 비용 추정치를 실제 실행시간으로 표시하지 않는다.

UC06 사용자 장애: RUM view에서 JS 오류와 backend 실패 요청을 찾는다. 동의하지 않은 세션에는 replay 링크가 생기지 않는다. 허용 origin의 trace context만 사용하며 sampling 때문에 trace가 없으면 동일 서비스와 시간의 검색을 제안한다.

UC07 비용 통제: 관리자가 월 예상 사용량과 실제 billable ledger를 비교한다. 예산 80%·100% 알림 후 soft cap 또는 explicit hard cap을 선택한다. 소급 요금 변경·무고지 drop·무단 업셀을 금지한다.

### 제품 지표와 인수 증거

| **지표** | **목표** | **측정 방법** |
|----|----|----|
| 설치 성공 | 첫 trace p50 15분 p90 30분 | 신규 개발자 5명 이상 관찰 |
| 원인 조사 | 고정 5개 장애에서 중앙값 30% 단축 | 기존 로그 조사 대비 동일 과업 |
| 맥락 유지 | 경보에서 관련 log까지 3단계 이하 | UI 자동화와 사용자 평가 |
| Java 진단 안전성 | 기본 계측 CPU 상대 증가 3% 이내 목표 | 지원 matrix별 비교 부하, 실측 공개 |
| 청구 신뢰성 | 청구된 모든 단위에 ledger 근거 존재 | 중복·정정·지연·취소 fixture 대사 |
| 데이터 신뢰성 | missing·sampled·stale·partial 상태 오표시 0 | 오류 주입 E2E 회귀 |

3%는 Montracer 설계 예산이며 경쟁 제품 수치를 성능 보증으로 옮긴 것이 아니다. 사용자 요청 latency p99 증가 5% 이내도 함께 평가한다. 기본 계측·프로파일·probe를 각각 켜고 누적 효과를 측정한다. 예산을 넘는 기능은 기본 off 또는 지원 범위 축소 후 다시 평가한다.

## 08 비기능 요구사항과 SLO

### 운영 목표와 측정 경계

| **SLI** | **GA 목표** | **측정과 제외 조건** |
|----|----|----|
| 수집 가용성 | 월 99.95% | 계약 한도 내 유효 요청의 2xx 비율, 내부 과부하 429는 실패 |
| 검색 API 가용성 | 월 99.9% | 유효 인증·허용 범위 요청의 성공 비율 |
| 검색 지연 | p95 2초, p99 5초 | 최근 1시간, 단일 서비스, 제한 내 hot query |
| 원본 검색 신선도 | p99 60초 이내 | ingress부터 원본 조회 노출까지, tail 대기 포함 |
| Metric rollup 신선도 | p99 180초 이내 | 2분 watermark 대기와 window 계산 포함 |
| 경보 지연 | p95 120초 이내 | watermark 확정 위반부터 발송 큐 내구성 기록까지 |
| 보존 대상 저장 | 99.99% 24시간 내 | 승인 unique 중 명시적 sampling·drop 제외, 저장 실패는 미달 |

월은 UTC 달력월이다. 30일 환산 오류 예산은 99.95%에서 21.6분, 99.9%에서 43.2분이다. 잘못된 인증, 유효하지 않은 payload, 계약 초과가 확실한 요청은 분모에서 제외하고 별도 집계한다. 과부하를 계약 초과로 오분류하지 않도록 quota 결정 로그를 남긴다.

### 데이터 정확성과 제한

SDK 발생부터의 지연은 source clock skew 영향을 받으므로 별도 참고 지표로 표시한다. 공식 freshness는 서버 ingress_received_at 기준이다. trace 검색의 incomplete, sampled, expired 상태를 구분한다. 로그와 metric은 기본적으로 sampling하지 않는다. 계정의 명시적인 drop 정책만 예외다.

쿼리별 최대 10GB scan, 1,000개 series, 페이지당 1,000행·누적 10,000행, interactive 5초 timeout을 시작값으로 한다. 일반 비동기 조회는 최대 60초·100GB, 대량 export job은 별도 승인 예산에서 최대 30분·1GiB 결과로 제한한다. 상한을 넘긴 결과는 정상 전체 결과처럼 반환하지 않는다.

### 성능 및 품질 기준

SDK 부하 목표는 기준 서비스 대비 CPU 사용률 상대 증가 3% 이하, 요청 지연 p99 상대 증가 5% 이하이다. Java agent와 Python/Node SDK를 각각 실제 워크로드로 측정하며 언어 공통 보장으로 홍보하지 않는다. exporter 장애가 애플리케이션 요청을 차단해서는 안 된다.

UI 핵심 흐름은 키보드로 조작 가능해야 하며 색상만으로 상태를 구분하지 않는다. 지원 브라우저와 화면 크기는 릴리스 검증표에 고정한다. 고객별 데이터 경계·삭제·복구 시험은 성능 목표와 동등한 출시 차단 조건이다.

## 09 공식 근거와 검토 관리

### 벤치마크 출처

각 출처는 공개 기능 확인용이다. Montracer의 한도·API·수치·일정은 별도의 설계 결정이다. 확인일은 2026년 10월 3일이며 공식 사이트 개편·SKU·버전에 따라 기능 조건이 달라질 수 있다.

[B01 https://docs.datadoghq.com/tracing/services/service_page/](https://docs.datadoghq.com/tracing/services/service_page/)

[B02 https://docs.datadoghq.com/tracing/trace_explorer/?lang_pref=en](https://docs.datadoghq.com/tracing/trace_explorer/?lang_pref=en)

[B03 https://docs.datadoghq.com/dashboards/template_variables/](https://docs.datadoghq.com/dashboards/template_variables/)

[B04 https://docs.datadoghq.com/dashboards/guide/context-links/](https://docs.datadoghq.com/dashboards/guide/context-links/)

[B05 https://pinpoint-apm.gitbook.io/pinpoint/want-a-quick-tour/overview](https://pinpoint-apm.gitbook.io/pinpoint/want-a-quick-tour/overview)

[B06 https://pinpoint-apm.gitbook.io/pinpoint/documents/realtime](https://pinpoint-apm.gitbook.io/pinpoint/documents/realtime)

[B07 https://docs.datadoghq.com/profiler/connect_traces_and_profiles/?lang_pref=en](https://docs.datadoghq.com/profiler/connect_traces_and_profiles/?lang_pref=en)

[B08 https://docs.datadoghq.com/tracing/trace_collection/dynamic_instrumentation/sensitive-data-scrubbing/](https://docs.datadoghq.com/tracing/trace_collection/dynamic_instrumentation/sensitive-data-scrubbing/)

[B09 https://docs.datadoghq.com/database_monitoring/](https://docs.datadoghq.com/database_monitoring/)

[B10 https://docs.datadoghq.com/real_user_monitoring/application_monitoring/browser/monitoring_page_performance/](https://docs.datadoghq.com/real_user_monitoring/application_monitoring/browser/monitoring_page_performance/)

[B11 https://docs.datadoghq.com/synthetics/browser_tests/](https://docs.datadoghq.com/synthetics/browser_tests/)

[B12 https://docs.datadoghq.com/data_security/](https://docs.datadoghq.com/data_security/)

[B13 https://docs.datadoghq.com/internal_developer_portal/catalog/entity_model/native_entities/](https://docs.datadoghq.com/internal_developer_portal/catalog/entity_model/native_entities/)

[B14 https://github.com/pinpoint-apm/pinpoint](https://github.com/pinpoint-apm/pinpoint)

[B15 https://pinpoint-apm.gitbook.io/pinpoint/v3.0.3/documents/error_analysis](https://pinpoint-apm.gitbook.io/pinpoint/v3.0.3/documents/error_analysis)

[B16 https://www.w3.org/WAI/WCAG22/quickref/](https://www.w3.org/WAI/WCAG22/quickref/)

### 근거 갱신

PM은 각 출시 직전 공식 기능 페이지와 지원 조건을 재확인한다. Agent lead는 지원 런타임을, Security는 라이선스와 데이터 수집 경계를 검토한다. 근거 변경은 기능 약속을 자동 확대하지 않으며 D01 요구사항 변경 PR과 관련 D02~D06 계약 검토를 거친다.
