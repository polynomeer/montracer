# D03 계측과 고급 진단 설계

> Montracer 개발 문서 세트 v2.0 (기준일 2026-10-03) · 원본: [`original/D03_Montracer_계측과_고급_진단_설계.docx`](original/D03_Montracer_계측과_고급_진단_설계.docx)  
> 이 파일은 `scripts/docs/convert_specs.py`로 생성한 파생본이다. 내용 변경은 원본 개정 + ADR로 한다.

Agent와 Platform 개발자를 위한 계측 및 고급 관측 모듈 명세다. OTel 기반 수집에 JVM 진단, 프로파일링, DBM, RUM과 Synthetic을 단계적으로 추가한다.

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

[01 에이전트 제품 구조와 호환성](#01-에이전트-제품-구조와-호환성)

[02 에이전트와 SDK 수집 전략](#02-에이전트와-sdk-수집-전략)

[03 Collector 구성과 설치 계약](#03-collector-구성과-설치-계약)

[04 JVM Inspector와 실시간 요청](#04-jvm-inspector와-실시간-요청)

[05 Thread dump와 원격 진단 계약](#05-thread-dump와-원격-진단-계약)

[06 Call tree와 URI 통계와 오류 분석](#06-call-tree와-uri-통계와-오류-분석)

[07 Continuous Profiling](#07-continuous-profiling)

[08 Dynamic instrumentation](#08-dynamic-instrumentation)

[09 Database Monitoring](#09-database-monitoring)

[10 Browser RUM과 Session Replay](#10-browser-rum과-session-replay)

[11 Synthetic와 비동기 메시지 관측](#11-synthetic와-비동기-메시지-관측)

[12 Infrastructure와 확장 통합](#12-infrastructure와-확장-통합)

## 01 에이전트 제품 구조와 호환성

### 배포 단위

montracer-collector는 검증한 OTel Collector 구성 배포판이고, montracer-java는 OTel Java 기반 확장 및 별도 진단 모듈이다. SDK의 trace 기능과 진단 프로토콜을 분리한다. 기본 SDK만 설치한 고객도 F01~F07을 사용할 수 있어야 하며, JVM 기능을 위해 독점 telemetry 전송으로 갈아타게 하지 않는다.

| **조합** | **첫 지원 후보** | **출시 전 필수 시험** |
|----|----|----|
| Java | JDK 17·21, Spring Boot 3, JDBC, HTTP, Kafka | 시작·재변환·async context·classloader |
| 기존 Java | JDK 8·11, legacy Spring과 servlet | G2 별도 호환 트랙, 별도 agent artifact |
| Go | 지원 중인 Go 2개 minor, net/http·gRPC·sql | context 취소, goroutine 누수, shutdown |
| Node와 Python | 선정 시점 지원 중인 LTS·CPython | 초기화 순서, worker와 async propagation |
| .NET | 선정 시점 지원 중인 LTS | ASP.NET·HTTP·DB 계측, runtime별 Gate |

정확한 patch 버전은 compatibility.lock에 CI digest와 함께 고정한다. 후보 버전 목록은 지원 인증이 아니다. Plugin마다 runtime·library·OS·architecture·feature capability 조합을 갖고 통과한 조합만 설치 UI에 노출한다. 애플리케이션과 agent의 의존성 충돌을 피하기 위해 shading과 classloader 격리를 시험한다.

### 설치와 업그레이드

VM은 패키지와 service unit, Kubernetes는 Helm과 opt-in namespace injection을 제공한다. admission 실패가 고객 배포를 막지 않도록 injection은 기본 fail-open이며 계측 누락 경보를 별도로 낸다. privileged·hostPID는 기본 요청하지 않고 필요한 진단에만 별도 권한 설명을 제공한다.

agent는 서명한 package·manifest를 검증하고 현재/직전 두 안정 버전을 지원한다. 원격 설정은 data-only이며 shell 실행이나 임의 jar 다운로드가 아니다. 정책 N에서 N-1로 되돌리면 다음 heartbeat에 적용 결과를 보고한다. 런타임 계측 비활성화와 프로세스 재시작 필요 여부를 구분한다.

## 02 에이전트와 SDK 수집 전략

### 계측 표준

Java는 공식 자동 계측 agent를 우선 제공하고 Go는 HTTP·gRPC·DB 수동 instrumentation 가이드를 제공한다. Node.js와 Python은 프로세스 시작 전 instrumentation 초기화를 강제한다. .NET은 Beta 지원 대상으로 둔다. 언어별 지원 런타임·라이브러리 조합은 CI 호환성 표로 관리한다.

필수 resource는 service.name, service.namespace, service.version, deployment.environment.name이다. service.instance.id와 k8s metadata는 agent가 보강한다. SDK semantic convention schema_url을 보존하고 정규화 worker에서 canonical version으로 변환한다. 누락된 service.name은 unknown_service로 보관하되 설치 경고를 발생시킨다.

| **신호** | **기본 수집 방식** | **운영 제한** |
|----|----|----|
| trace | SDK batch exporter, W3C propagation | span 64KiB, 속성 128개, 값 4KiB |
| metric | SDK OTLP 15초 주기, infra scrape | 사용자·trace ID label 금지, series quota |
| log | 구조화 stdout와 node file receiver | body 32KiB, multiline 최대 256KiB에서 분할 |
| 배포 이벤트 | CI API 또는 resource version 변경 | 시각·서비스·환경·commit만 저장 |

### 컨텍스트 전달과 실패 동작

HTTP/gRPC는 traceparent와 tracestate를 전달하고 MQ는 message header로 전달한다. fan-out 및 batch 소비는 span link를 사용한다. trust boundary에서 baggage는 allowlist만 통과시킨다. 외부 입력의 trace ID는 권한 정보가 아니다. W3C 형식과 all-zero ID를 검증한다. \[R2\]

SDK는 비동기 exporter와 bounded queue를 사용한다. 기본 SDK queue 2,048 spans, export batch 512, flush 5초, 종료 flush 최대 5초를 시작값으로 제공한다. 포화 시 span을 버리고 dropped counter를 증가시키며 비즈니스 요청은 계속 처리한다. 언어별 실제 설정명은 지원 패키지에 맞춰 생성한다.

node agent의 file checkpoint는 재시작 후 로그 재수집을 제한한다. hostPath와 파일 읽기 권한을 최소화하고 privileged 모드는 기본 금지한다. Kubernetes API metadata 권한은 list/watch에 필요한 namespace 범위로 제한한다. 수집 설정은 서명된 artifact로 배포하고 이전 설정으로 되돌릴 수 있게 한다.

## 03 Collector 구성과 설치 계약

### 참조 설정

아래는 고객 측 Collector의 구성 예시다. 인증된 서버 ingress가 최종 개인정보 제거와 내구성 ACK를 책임진다. volume /var/lib/otel은 재시작 후 유지되어야 한다. 설정은 선택한 contrib 배포판의 검증 명령으로 확인한 후 배포한다.

``` yaml
extensions:
  file_storage:
    directory: /var/lib/otel
receivers:
  otlp:
    protocols:
      grpc:
        endpoint: 127.0.0.1:4317
      http:
        endpoint: 127.0.0.1:4318
processors:
  memory_limiter:
    check_interval: 1s
    limit_mib: 512
    spike_limit_mib: 128
  batch:
    timeout: 1s
    send_batch_size: 512
    send_batch_max_size: 1024
exporters:
  otlp/platform:
    endpoint: ${env:APM_ENDPOINT}
    headers:
      authorization: Bearer ${env:APM_INGEST_KEY}
    tls:
      insecure: false
    sending_queue:
      enabled: true
      storage: file_storage
      queue_size: 2000
    retry_on_failure:
      enabled: true
      max_elapsed_time: 300s
service:
  extensions: [file_storage]
  pipelines:
    traces:
      receivers: [otlp]
      processors: [memory_limiter, batch]
      exporters: [otlp/platform]
```

metric과 log pipeline도 동일한 receiver·memory limiter·batch·exporter를 각각 등록한다. node 간 접속이 필요하면 bind 주소를 변경하고 mTLS 및 network policy를 함께 설정한다. 큐 크기는 요청 수이며 byte 보장으로 간주하지 않는다. \[R3\]

persistent queue는 고객 디스크에 민감정보를 남길 수 있다. 운영 배포에는 batch 이전의 검증된 allowlist redaction processor와 암호화 volume을 추가한다. 필터 없는 위 예시는 비민감 테스트 데이터의 연결 확인용이다. 상세 redaction 정책은 D04 03절을 따른다.

## 04 JVM Inspector와 실시간 요청

### 수집 데이터와 의미

JVM CPU, heap·nonheap used/committed/max, GC pause count·duration, thread 상태 수, class count, pool active/idle/pending을 15초마다 수집한다. 카운터 reset은 process_start_time으로 구분한다. JVM arguments는 allowlist 이름만 기본 수집하고 -D 값·환경변수·전체 command line을 수집하지 않는다.

active request는 agent가 관측한 server request의 진입과 종료를 monotonic clock으로 측정한 개수다. 이것은 JVM 전체 thread 수가 아니다. 0~1초, 1~3초, 3~5초, 5초 이상 age bucket을 5초 주기로 보낸다. 웹의 live 구독이 있을 때만 1초 주기를 허용하고 마지막 구독 종료 30초 후 되돌린다.

| **필드** | **규칙** | **보존** |
|----|----|----|
| agent_id와 boot_id | 설치 ID와 프로세스 시작별 UUID | inventory 30일 |
| sample_seq와 captured_at | boot별 증가, clock skew 별도 | active sample 24시간 |
| active_by_age | 4개 bucket, count 음수 금지 | 장기 집계는 1분 |
| coverage | 계측 모듈·지원 여부·오류 | 최신 상태와 변경 이력 |
| process tags | host·pod UID·container ID·service ID | tenant scope 내 연결 |

### 조회 계약

GET /api/v1/agents?service_id=...는 capability, last_seen, boot_id, policy_revision을 반환한다. GET /api/v1/agents/{id}/runtime은 시간 구간과 metric 목록을 받고 D02 공통 query envelope를 반환한다. active request 스트림은 D02 /streams의 kind=active_requests이며 TTL 60초 구독 lease로 관리한다.

1초 수집 시 화면 갱신 p95 3초, 3회 누락 또는 마지막 sample 10초 초과 시 stale을 표시한다. 연결 종료를 active=0으로 그리지 않는다. 동일 boot/sample_seq는 중복 제거한다. time 범위의 과거 Inspector에서는 보존되지 않은 상세 live request 목록을 재구성했다고 주장하지 않는다.

### 안전 예산

agent의 active map은 최대 10,000 requests, age 계산은 bounded 작업으로 한다. 초과 시 샘플 목록을 줄이고 정확한 전체 계수 가능 여부를 표시한다. async request와 virtual thread는 logical request와 execution thread를 분리하며 thread ID의 장기 안정성을 가정하지 않는다.

## 05 Thread dump와 원격 진단 계약

### 승인과 상태 머신

진단은 기본 off이며 조직 opt-in, 해당 agent capability, diagnostics.execute 권한과 MFA step-up을 모두 요구한다. 운영 환경은 요청자와 승인자를 분리한다. 작업은 requested → approved → dispatched → running → completed로 진행하고 rejected, expired, cancelled, failed가 terminal 상태다. 승인 없이 실행하지 않는다.

POST /api/v1/diagnostic-jobs는 agent_id, boot_id, kind=thread_dump, reason, target_request_id, expires_in_seconds를 받는다. 만료는 최대 120초, agent당 동시 작업 1개, dump는 분당 1회가 시작값이다. 서버는 202와 job_id를 반환하고 UI는 polling 또는 SSE로 상태를 읽는다. 이미 끝난 요청은 completed_before_capture를 결과로 남긴다.

### 제어 경로

agent가 시작한 outbound mTLS 채널만 사용한다. command에는 job_id, tenant_id, agent_id, boot_id, nonce, not_before, expires_at, command_schema, approved_scope와 서명을 담는다. agent는 boot mismatch·만료·중복 nonce·미지원 명령을 거절한다. 네트워크 단절 시 명령을 무기한 재실행하지 않는다.

dump 결과는 thread state, lock owner, redacted stack frames, truncated와 captured_at을 포함한다. local variable·method argument·heap dump는 G2 범위 밖이다. 최대 2,000 threads, thread당 128 frames, 결과 2MiB이며 초과는 truncation으로 명시한다. 결과는 agent에서 먼저 scrub한 뒤 전용 ingest로 보낸다.

| **실패** | **API와 UI 표현** | **재시도 정책** |
|----|----|----|
| capability 없음 | 422 unsupported_capability | 설정 안내, 자동 재시도 없음 |
| 권한 또는 승인 없음 | 403 diagnostics_forbidden | 재인증 또는 승인 요청 |
| agent 재시작 | 409 boot_changed | 새 boot 대상으로 새 job |
| 실행 중 만료 | expired, 결과 미수집 | 새 승인 없이는 재실행 금지 |
| 요청 종료 | completed_before_capture | 정상 경쟁 상태, 오류로 경보하지 않음 |

### 보관과 증거

결과는 tenant별 객체 저장소에 24시간, 비민감 작업 audit는 365일 보존한다. read permission은 실행 권한과 분리하고 접근마다 감사한다. 지원 번들에는 dump를 자동 포함하지 않는다. 취소는 best effort로 이미 실행한 순간 측정을 되돌리지 못하지만 업로드·조회는 차단한다. kill switch 전파 목표는 60초이며 검증에 실패하면 신규 작업을 차단한다.

## 06 Call tree와 URI 통계와 오류 분석

### 계측 깊이

기본 call tree는 framework·RPC·DB·메시지 계측 span과 승인된 method event를 결합한다. 모든 Java method를 무조건 span으로 만드는 접근은 CPU·저장량 폭증 때문에 채택하지 않는다. method allowlist는 package·class·method signature와 최대 depth 32, trace당 2,000 events, invocation당 1KiB를 갖는다.

method event에는 parent_event_id, relative_start_ns, duration_ns, symbol_id와 outcome만 둔다. SQL은 dialect-aware normalization 후 db.system, database alias, query_fingerprint, operation을 남긴다. SQL literal과 bind 값은 기본 제거한다. critical path는 overlapping·async 경계를 고려한 추정치이며 self time을 자식 duration의 단순 합으로 계산하지 않는다.

### URI와 scatter

HTTP route template와 method, service, environment로 endpoint를 식별한다. raw path의 ID를 dimension으로 쓰지 않는다. route가 없으면 정규화 정책과 unknown route bucket을 사용하고 자동 route 추정의 confidence를 표시한다. 비샘플링 histogram에서 요청 수·오류율·percentile을 계산한다.

scatter의 점은 보존된 request sample이며 전체 요청 분포는 별도 density/histogram이다. time \[from,to)와 duration \[min,max) 드래그는 query AST로 변환한다. 최대 5,000점 초과 시 고정 seed reservoir 표본과 총 대상 수를 표시하고 zoom하면 서버 재조회한다. 화면에 없는 점을 전체 요청이 없었다는 뜻으로 해석하지 않는다.

### Error grouping

fingerprint는 tenant, service, exception type, 정규화한 상위 application frame 5개와 cause type의 hash다. line number·request ID·원문 message 숫자는 기본 제거하여 배포와 동적 값 때문에 과도하게 분리되지 않게 한다. stack의 원문 인자·개인 경로는 scrub한다. 알고리즘 version을 저장하고 재그룹은 새 version으로 수행한다.

issue는 open → acknowledged → resolved, 해결 이후 동일 오류 재발은 regressed → open이다. owner·first_seen·last_seen·affected_version·occurrence_count·sample_trace_id를 저장한다. log와 span의 같은 exception은 공통 exception_event_id가 있을 때만 중복 제거한다. 그렇지 않으면 출처별 수치를 분리하며 고유 사용자 수라고 주장하지 않는다.

인수 시험은 같은 원인의 100개 동적 message가 한 그룹, 서로 다른 cause가 다른 그룹, 해결 후 재발 이벤트, trace 미보존 시 graceful fallback을 포함한다. \[B05, B14, B15\]

## 07 Continuous Profiling

### 구현안과 대안

G2는 Java의 JFR 기반 capture와 Go pprof 수집을 첫 대상으로 한다. platform별 profiler adapter에서 CPU·allocation·wall profile을 canonical profile metadata와 압축 payload로 변환한다. 네이티브 프로파일 engine을 자체 개발하지 않고 검증된 라이브러리를 통합한다. 채택 버전의 라이선스와 production overhead를 먼저 검증한다.

OTLP profile 지원은 버전별 capability로 협상한다. 지원을 가정해 trace endpoint로 억지 전송하지 않고 독립 profile ingress를 둔다. pprof 계열은 타입·단위·period·samples·locations·functions를 보존한다. symbolication은 build_id와 서명된 symbol artifact를 사용하며 임의 외부 URL을 서버가 fetch하지 않는다.

| **항목** | **시작값** | **정확성 경계** |
|----|----|----|
| Window와 upload | 60초 profile, 60초 전송 | sample count가 적으면 low confidence |
| CPU sampling | 49Hz 후보, runtime별 검증 | CPU budget 초과 시 빈도 감소 |
| 저장 | 객체 payload 7일, metadata 30일 | profile 삭제가 양쪽에 적용 |
| 비교 | 동일 runtime·type·unit의 두 구간 | 요청 수 또는 sample time으로 정규화 |
| 연결 | tenant·service·instance·time·optional span ID | 시간 겹침은 인과관계가 아님 |

### API와 UI

POST /api/v1/profiles/search는 service_id, from, to, profile_type과 filter를 받고 profile_id, duration, sample_count, unit, coverage를 반환한다. GET /api/v1/profiles/{id}/flamegraph는 tree nodes를 parent index와 inclusive/exclusive 값으로 반환한다. 최대 100,000 nodes는 server aggregation 후 내려주고 작은 frame은 other로 합친다.

trace에서 profile로 이동할 때 정확한 span 연계가 있으면 linked, 시간만 겹치면 overlapping으로 표시한다. flame graph 폭은 sample weight이며 wall timeline으로 오해시키지 않는다. diff에서 빨강은 비용 증가, 파랑은 감소로 라벨링하고 장애 상태 색과 범례를 분리한다.

### 승인 기준

CPU·heap·p99 overhead를 기본 agent 대비 추가 측정한다. profiler crash는 고객 process를 종료시키지 않도록 가능한 격리 경로를 쓰고 runtime 내 native extension 위험은 지원 matrix에 공개한다. 불가능한 강한 격리를 보장하지 않는다. kill switch와 upload quota, symbol artifact tenant 격리 시험을 통과해야 GA다. \[B07\]

## 08 Dynamic instrumentation

### 허용 동작

G2에서 Java method entry/exit의 duration counter와 고정 문자열 diagnostic log만 제한 Beta로 시작한다. 조건식은 typed 비교·boolean 결합의 작은 DSL이며 임의 Java·JavaScript eval, reflection 호출, 파일·네트워크 접근은 없다. object snapshot과 local variable capture는 별도 보안 검증 전 제공하지 않는다.

probe 정의는 id, revision, service selector, build_id, class, method, action, condition_ast, ttl_seconds, rate_limit, byte_budget를 가진다. production은 승인된 build와 최대 10개 probes/agent, probe당 1 event/s·64KiB/min, 기본 TTL 15분·최대 60분으로 제한한다. 숫자는 초기 안전 예산이며 부하 시험 후 축소할 수 있다.

### 배포와 복구

probe는 draft → validated → approved → canary → active → expired 순으로 진행한다. 먼저 agent 1개 또는 1%에 적용해 instrumentation error·CPU·request p99를 확인한다. CPU 상대 증가 3% 또는 p99 5% 초과가 2분 지속하면 자동 중단한다. capture 횟수와 suppressed 횟수를 사용자가 본다.

정책은 서명·revision·nonce·만료를 포함하고 agent는 last known safe config만 허용한다. server와 연결이 끊겨도 TTL 후 로컬에서 제거한다. 클래스 재변환 실패 시 agent 전체를 재시작하지 않고 해당 probe만 거절한다. 제거가 완전히 불가능한 runtime에는 재시작 필요를 명시한다.

### 민감정보

식별자 denylist와 값 scrub를 agent 내부에서 적용한 뒤 buffer에 넣는다. 포맷 문자열의 placeholder는 allowlist scalar만 허용하고 toString 호출로 비밀이 노출되지 않도록 한다. server는 한 번 더 scrub한다. 잠재 민감값이 들어간 expression·probe spec은 감사 로그에도 원문 대신 안전한 구조를 남긴다. \[B08\]

### 인수 조건

미승인 probe 거절, 만료 후 60초 이내 실행 중지, 이전 revision 재전송 무시, agent restart 후 미승인 재활성화 방지, nonmatching build 차단, 압축 이전 byte budget, 위장된 sensitive field fixture를 시험한다. 고장 난 probe가 고객 요청을 실패시키면 P0 release blocker다.

## 09 Database Monitoring

### 신호와 권한

G2는 PostgreSQL과 MySQL부터 query digest별 calls·total duration·rows·wait·lock·connection saturation을 수집한다. agent connector는 read-only 최소 권한을 사용하고 데이터 테이블 본문 조회는 하지 않는다. 사전에 승인된 시스템 view와 제한된 statement timeout 2초, 15초 수집 interval을 시작값으로 둔다.

APM DB span은 고객 애플리케이션이 본 시간이며 DBM은 서버에서 본 query 활동이다. 둘의 차이는 pool wait·network·serialization일 수 있다. 같은 fingerprint와 instance·시간이 맞아도 개별 실행과 정확히 연결되지 않으면 correlated로만 표시한다. query comment propagation은 고객 opt-in이며 내부 tenant ID·PII를 주입하지 않는다.

| **레코드** | **Identity와 주요 필드** | **보관** |
|----|----|----|
| db_instance | tenant, connector, stable instance ID, engine | inventory 30일 |
| query_stats | instance, fingerprint, window, reset_epoch | 원본 15일, 1분 90일 |
| query_sample | sample_id, fingerprint, duration, wait_type | 7일 |
| query_plan | fingerprint, plan_hash, redacted plan tree | 7일 |

### Plan 정책

기본은 이미 수집된 plan의 조회만 제공한다. 능동 EXPLAIN은 별도 opt-in에서 제한된 SELECT template, read-only transaction, timeout·rate limit으로 실행하며 EXPLAIN ANALYZE는 금지한다. DDL·DML과 다중 statement를 차단한다. plan 자체의 filter literal도 scrub한다. 추정 rows·cost와 실제 시간·rows를 명확히 구분한다.

query fingerprint는 dialect와 정규화 AST를 포함하고 normalizer_version을 기록한다. 형식만 바뀐 query가 같은 signature를 얻고 의미 다른 query가 합쳐지지 않는 fixture가 필요하다. DB stats reset 이후 음수 차분은 reset으로 처리한다. Top 100 query 밖의 누락분은 other와 coverage로 공개한다.

### API와 실패 상태

POST /api/v1/db/queries/search, GET /api/v1/db/instances/{id}, GET /api/v1/db/plans/{id}를 제공한다. permission 부족, view 미설치, stale connector, unsupported engine을 구분하고 계측을 위해 슈퍼유저 권한을 요구하지 않는다. 수집용 DB query로 운영 DB 부하가 커지면 해당 connector interval을 늘리고 중지할 수 있어야 한다. \[B09\]

## 10 Browser RUM과 Session Replay

### RUM 수집

G2 Browser SDK는 view, action, resource, error, long task, LCP·INP·CLS를 수집한다. user ID는 기본 미수집이고 tenant별 가명 session ID는 최대 30분 비활동 또는 4시간에서 회전한다. 공개 client token은 비밀이 아니며 app·origin·signal 제한, bot·rate limit과 schema 검증을 적용한다. CORS만으로 악의적 비브라우저 클라이언트를 막을 수 있다고 주장하지 않는다.

동의가 필요한 배포에서는 consent=pending 상태에 네트워크와 local persistence를 모두 중지한다. consent grant 이후만 수집하고 revoke 시 queue·cookie·local state를 제거한다. SDK가 이미 보낸 데이터 삭제는 별도 deletion flow다. URL query·fragment·입력값·DOM text는 기본 제외한다.

### Backend 연결

허용한 first-party origin에만 traceparent를 주입한다. third-party 요청으로 내부 tracing 정보가 새지 않게 하고 CORS preflight 변화도 문서화한다. RUM sample과 backend trace sample 정책은 독립이며 연결 실패 이유를 not_sampled, expired, denied, unavailable로 나눈다. session_id는 metric label로 쓰지 않는다. \[B10\]

### Replay 별도 Gate

G3 replay는 기본 off, DOM text와 모든 input을 마스킹하고 password·payment·canvas·cross-origin iframe은 capture하지 않는다. 명시적으로 안전한 요소만 allowlist한다. 초기 DOM snapshot과 incremental mutation은 클라이언트에서 scrub한 후 전송하며 원문을 서버에서만 가리는 방식은 금지한다. \[B12\]

chunk key는 tenant/app/session/sequence, 각 chunk 최대 512KiB, 세션 최대 10MiB를 시작값으로 한다. 7일 보존, replay.read 별도 권한, 조회 audit, download 기본 금지를 적용한다. player는 격리 origin의 sandbox에서 실행하고 캡처 HTML의 script·form·network 실행을 차단한다.

### 성능과 인수

RUM gzipped SDK 목표 35KiB, main thread 처리 p95 5ms 이하의 소배치, offline queue 최대 1MiB를 초기 예산으로 둔다. device·browser별 실측 후 확정한다. hidden tab·SPA navigation·bfcache·unload·동의 철회·iframe·민감 fixture를 검증한다. browser가 지원하지 않는 Web Vital을 0으로 표시하지 않는다.

## 11 Synthetic와 비동기 메시지 관측

### Synthetic 제품

G2 API test는 HTTP와 multi-step HTTP, TLS 만료, DNS·connect·TTFB를 제공한다. G3 browser test는 격리된 Playwright runner와 선언형 step을 사용한다. 테스트 대상 소유권 확인, 위험 URL denylist, metadata·loopback·사설 주소 차단을 적용한다. private location만 고객이 승인한 사설 CIDR에 접근하며 public runner와 credential을 공유하지 않는다.

test_run_id는 schedule slot·test revision·location으로 결정하고 lease로 중복 실행을 제어한다. 기본 API interval 1분, browser 5분, timeout 60초·180초, worker당 browser concurrency 2로 시작한다. retry 결과와 최초 실패를 모두 남기며 location quorum과 runner_missing을 target_failure와 분리한다.

secret은 vault에서 runner의 단기 token으로 얻고 step log·screenshot에서 마스킹한다. screenshot 실패 시 원문을 자동 보관하지 않는다. 브라우저 프로필·임시 파일은 run 종료 후 폐기한다. 고객별 격리 container와 egress proxy, resource limit, 이미지 patch를 운영한다. \[B11\]

### 메시지 관측

Kafka 첫 버전은 consumer group lag·produce/consume rate·error·processing duration을 수집한다. pathway ID는 허용한 service·topic alias·consumer group의 hash로 구성하고 message payload는 수집하지 않는다. 동일 메시지 재전달은 delivery attempt와 logical message를 구분한다.

producer timestamp부터 consumer 시작까지는 clock skew와 broker timestamp semantics 영향을 받는다. 알려진 clock offset 없이 정확한 queue latency라 단정하지 않는다. batch는 span link로 표현하고 한 trace tree로 강제 합치지 않는다. fan-out 경로를 합할 때 retry·branch 중복을 별도 계수한다. \[B13\]

### API와 검증

POST /api/v1/synthetic-tests, POST /api/v1/synthetic-tests/{id}/runs, GET /api/v1/synthetic-runs/{id}, POST /api/v1/messaging/pathways/search를 제공한다. 모든 create는 revision과 idempotency를 적용한다. SSRF·DNS rebinding·비밀 fixture·중복 schedule·partition 장애·clock skew·메시지 재시도를 release gate에 포함한다.

## 12 Infrastructure와 확장 통합

### 자원 식별

host·container·pod·workload·node·cluster inventory를 provider resource ID와 Kubernetes UID로 식별한다. hostname·pod name만으로 재시작 전후를 같은 instance라 간주하지 않는다. agent service.instance.id와 resource UID를 통해 metric·trace를 연결하며 metadata 삭제 후에는 tombstone을 사용한다.

Kubernetes watch는 resourceVersion 재연결과 relist를 지원한다. RBAC는 pods·nodes·workloads의 필요한 read만 부여하고 Secret 본문 조회는 하지 않는다. label allowlist·값 길이·활성 series 제한을 적용한다. 수집이 멈춘 자원과 삭제된 자원을 last_seen·deleted_at으로 구분한다.

### 통합 팩

integration pack은 manifest, config schema, secret refs, metric dictionary, dashboard template, monitor template, fixture와 권한 목록을 포함한다. signature와 digest로 배포하고 업데이트 시 추가 권한을 자동 승인하지 않는다. MVP pack은 Linux host, Kubernetes, PostgreSQL, Redis, Kafka를 후보로 하며 기능별 실제 지원을 capability registry에서 확인한다.

G3 cloud connector는 AWS부터 assume-role/external ID 방식의 최소 read scope를 검증하고 API rate budget·credential expiry·backfill cursor를 가진다. serverless는 cold start와 invocation ID·resource ARN을 보존하고 짧은 수명 프로세스의 flush 손실을 표시한다. eBPF service discovery는 별도 opt-in이며 kernel 권한·호환성 시험 후 도입한다.

### 배포와 CI와 LLM 확장

deployment event는 service·environment·version·commit SHA·occurred_at·source를 포함한다. CI run, job, test case는 repository·pipeline·run·attempt 키로 저장한다. flaky 분류는 같은 revision에서 동일 test가 pass/fail로 바뀐 이력이며 제품 결함 부재를 뜻하지 않는다. source code·test secret은 기본 수집하지 않는다.

LLM 관측은 provider·model alias·token 수·time to first token·latency·error와 cost_price_version을 수집한다. prompt·response·tool argument는 기본 미수집이다. 추정 token 비용과 실제 공급자 invoice를 분리하고 모델 출력의 비결정성을 오류율 하나로 표현하지 않는다.

### RCA 기능 경계

G3 anomaly detector는 seasonality 기준 데이터 14일, 변화점·배포 연관·의존성 경로로 후보를 만든다. 가설에 근거 query·시간·coverage·confidence를 붙이며 인과관계 확정이나 자동 remediation을 하지 않는다. LLM 요약 도입 시 고객 opt-in·지역·redaction·권한 필터를 적용하고 untrusted log의 명령을 실행하지 않는다.
