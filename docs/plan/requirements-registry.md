# 요구사항 Registry (F01~F27)

F ID별 상태와 release tag의 단일 원천이다 (D06 §02). 기능 정의와 인수 기준의 권위는 [D01 §05~06](../specs/D01-product-requirements.md)이다.

- 상태: `planned` → `experimental` → `beta` → `GA` → `deprecated`
- Gate 증거(D06 §06) 없이 `GA`로 올리지 않는다. beta는 계약 보장 범위에 자동 포함되지 않는다.
- 상태를 바꾸는 PR에는 증거 링크(시험 보고서, Gate 승인)를 붙인다.

## Core (M0 / G1)

| ID | 기능 | 단계 | Epic | 주요 설계 | 상태 | Release |
|---|---|---|---|---|---|---|
| F01 | OTLP 3신호 수집, retry·partial success·tenant 위조 시험 | M0 | E02 | D02 §04~05 | planned | — |
| F02 | Trace waterfall, span 상세, 구조 불완전 사유와 log 연결 | M0 | E03 E04 | D02 §09, §22 / D05 §06 | planned | — |
| F03 | 서비스 RED·목록·소유자·map, client/server 중복 계수 없음 | M0 | E04 | D02 §08 / D05 §05, §07 | planned | — |
| F04 | Typed 필터·facet·저장 검색·공유 URL, context 유지 | M0 | E03 E04 | D02 §15, §19 / D05 §03 | planned | — |
| F05 | Dashboard 6종 위젯, 변수, revision 충돌 검출 | M0 | E05 | D02 §14 / D05 §10~11 | planned | — |
| F06 | 임계치 monitor, webhook, no data와 query 실패 구분 | M0 | E05 | D02 §17 / D05 §09 | planned | — |
| F07 | OIDC·조직 RBAC·키·PII·quota·사용량 조회 | M0 | E01 | D04 §01~03 | planned | — |
| F08 | 팀·환경 scope, SAML·SCIM, 조회 감사, step-up | G1 | E01 | D04 §12 | planned | — |
| F09 | HA·삭제 proof·월간 SLO·유료 과금 대사·지원 절차 | G1 | E07 | D04 §04~05, §13, §15 | planned | — |
| F10 | Tail sampling, 결정 복구, budget drop·late span 공개 | G1 | E02 | D02 §06 | planned | — |
| F11 | SLO burn·복합 monitor·mute·escalation·사건 timeline | G1 | E05 | D02 §17 / D05 §09 | planned | — |
| F12 | 요청 scatter·URI 통계, 영역 선택 필터 재현 | G1 | E04 | D03 §06 / D05 §06 | planned | — |
| F13 | JVM Inspector·활성 요청, 만료·stale 상태 노출 | G1 | E06 | D03 §04 / D05 §07 | planned | — |
| F14 | Call tree·SQL template·예외 chain·오류 그룹화 | G1 | E06 | D03 §06 | planned | — |

## 확장 (G2 / G3 / Scale)

| ID | 기능 | 단계 | Epic | 주요 설계 | 상태 | Release |
|---|---|---|---|---|---|---|
| F15 | CPU·allocation 프로파일, flame graph·배포 비교·trace overlap | G2 | E08 | D03 §07 | planned | — |
| F16 | 승인된 Java thread dump·제한 probe, 만료·kill switch | G1 Beta / G2 GA | E06 | D03 §05, §08 | planned | — |
| F17 | PostgreSQL·MySQL DBM, digest·wait·plan 비교 | G2 | E08 | D03 §09 | planned | — |
| F18 | Host·container·K8s ↔ 서비스, metadata TTL | G1 기본 / G2 확장 | E10 | D03 §12 | planned | — |
| F19 | Browser RUM, Web Vitals·JS error·view↔trace | G2 | E09 | D03 §10 | planned | — |
| F20 | API Synthetic, 사설 runner, runner 중단 구분 | G2 | E09 | D03 §11 | planned | — |
| F21 | Browser Synthetic·Session replay, 마스킹 회귀 | G3 | E09 | D03 §10~11 | planned | — |
| F22 | Kafka lag·message pathway·처리 지연 | G2 | E10 | D03 §11 | planned | — |
| F23 | 배포·CI pipeline·test 연결, flaky 분류 | G3 | E10 | D03 §12 | planned | — |
| F24 | Serverless cold start·클라우드 API 통합 | G3 | E10 | D03 §12 | planned | — |
| F25 | Anomaly·change detection·RCA 가설 | G3 | E10 | D03 §12 | planned | — |
| F26 | 전용 Cell·사설망 패키지·DR·감사 export | Scale | E07 | D02 §02 / D04 §07, §15 | planned | — |
| F27 | LLM span·token·비용 metadata, prompt 미수집 | G3 | E10 | D03 §12 | planned | — |

## Epic 매핑 (D06 §02)

| Epic | 요구 | 담당 | 선행 |
|---|---|---|---|
| E01 Tenant와 Identity | F07 F08 | API/Security | — |
| E02 Ingest와 Quality | F01 F10 | Data | E01 |
| E03 Storage와 Query | F02 F04 | Data/API | E02 |
| E04 Core 조사 UI | F02 F03 F12 | FE/Design | E03 mock |
| E05 Monitor와 Dashboard | F05 F06 F11 | API/FE | metric oracle |
| E06 Java 진단 | F13 F14 F16 | Agent | E01 E02 |
| E07 운영과 상용화 | F09 F26 | SRE/Platform | E01~E05 |
| E08 Profile와 DBM | F15 F17 | Agent/Data | E06 |
| E09 RUM와 Synthetic | F19~F21 | FE/Platform | PII·runner 격리·consent |
| E10 Integration과 확대 | F18 F22~F25 F27 | — | capability·pack CI |
