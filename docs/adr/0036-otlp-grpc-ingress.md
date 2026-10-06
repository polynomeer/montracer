# ADR 0036: OTLP/gRPC 수신

- 상태: 승인 (결정 위임)
- Owner: Data lead
- 승인자: polynomeer — 결정 사항은 빅테크 서비스 사례를 기준으로 정하고 근거를 기록하라는 지시 (2026-10-05, ADR 0021~0035와 같은 위임). 근거는 §5
- 날짜: 2026-10-06 (제안·결정)
- 관련: D02 §04(요청 계약·결과 표), ADR 0002(ACK), 0017(decode 한도), 0020(ingress·envelope), 0024(quota)

## 배경

D02 §04는 "OTLP/gRPC 및 OTLP/HTTP"를 요구한다. 지금 ingress는 OTLP/HTTP만 받는다.

OTel SDK와 Collector의 OTLP exporter는 gRPC를 많이 쓴다. 그래서 gRPC 수신이 없으면 고객이 exporter 설정을 바꾸거나 중간에 Collector를 둬야 한다.

## 결정

### 1. 한 처리 경로

- **처리 core:** HTTP handler 안에 있던 처리를 transport와 무관한 `process`로 뺐다. 순서는 인증 → decode → 검증 → environment 범위 → redaction → quota → envelope → Kafka append다. HTTP와 gRPC가 같은 `process`를 부른다.
- **transport별 차이는 decode 함수 하나뿐이다.**
  - HTTP: content-type, 전송 크기, 압축 해제
  - gRPC: 이미 받은 byte
- 그래서 tenant 주입(계약 1), ACK 시점(계약 2), PII 제거 순서(계약 3), quota, 로그 금지 항목이 두 transport에서 같다.

### 2. 인증 뒤에 본문을 받는다, raw codec으로 byte를 받는다

- **Export는 server에 stream method로 등록한다**(wire 형식은 unary와 같아 표준 OTLP client가 그대로 쓴다).
  - grpc-go는 unary method의 메시지를 handler 호출 **전에** 끝까지 받아 압축을 푼다. 처음 구현은 이 때문에 인증 없는 client가 연결마다 stream을 열어 8MiB 수신·압축 해제를 강제할 수 있었다(리뷰에서 발견).
  - stream handler는 메시지를 직접 `RecvMsg`한다. 그래서 HTTP와 같이 **인증 → in-flight slot → 본문 수신** 순서다.
  - 인증 없는 oversize 요청은 크기 오류가 아니라 UNAUTHENTICATED다(시험).
  - 인증을 `InTapHandle`에서 하지 않는 이유: tap은 연결의 I/O goroutine에서 돌아 blocking 작업(제어 DB key 조회)을 하면 그 연결의 모든 RPC가 멈춘다(grpc-go 문서).
- **수신 상한 20초**(HTTP `ReadTimeout`과 같다). 느린 송신이 in-flight slot을 붙잡지 않는다. 넘으면 재시도 가능한 읽기 실패(UNAVAILABLE)다.
- gRPC server는 `grpc.ForceServerCodecV2(RawCodec{})`로 메시지를 **protobuf struct로 풀지 않고 byte로** 받는다.
- 그 byte를 HTTP와 같은 `otlp.Decode`(protobuf)로 푼다. 따라서 decode 전 복잡도 pre-scan(ADR 0017)과 malformed 판정이 같다.
- **압축 해제 후 크기 상한**은 gRPC server의 `MaxRecvMsgSize`이고, HTTP의 `MaxDecodedBytes`(8MiB)와 같은 값이다.
  - grpc-go는 압축 해제 뒤 크기로 이 값을 검사한다. 그래서 gzip 폭탄도 decode 전에 막힌다(시험). 이 검사도 인증 뒤다.
  - 지원하지 않는 압축(`grpc-encoding`, 예: zstd)은 415와 같은 INVALID_ARGUMENT다.
- **RawCodec은 server 전체에 적용된다.** 그래서 이 server에는 OTLP 서비스 3개만 등록한다. grpc health·reflection은 넣지 않는다(시험으로 고정). health는 HTTP `/healthz`·`/readyz`를 쓴다.
- 서비스는 OTLP 표준 이름이다: `opentelemetry.proto.collector.{trace,metrics,logs}.v1.{Trace,Metrics,Logs}Service/Export`.
- 서비스마다 key scope를 따로 본다(`ingest.traces|metrics|logs`).
- gzip 압축을 받는다(OTLP 규격 필수).

### 3. 상태 코드 (OTLP 규격, D02 §04 표)

| 결과 | HTTP | gRPC | client 재시도 |
|---|---|---|---|
| 전체·일부 승인 | 200 (+partial_success) | OK (+partial_success) | 안 함 |
| 잘못된 데이터 | 400 | INVALID_ARGUMENT | 안 함 |
| 인증 실패 / scope 밖 | 401 / 403 | UNAUTHENTICATED / PERMISSION_DENIED | 안 함 |
| 본문·복잡도·burst 초과 | 413 | RESOURCE_EXHAUSTED, **RetryInfo 없음** | 안 함(같은 크기로 재시도 금지) |
| tenant rate 한도 | 429 + Retry-After | RESOURCE_EXHAUSTED + **RetryInfo** | 지연 뒤 |
| 과부하·Kafka append 실패·인증 저장소 장애 | 503 + Retry-After | UNAVAILABLE + RetryInfo | backoff |

- 메시지는 HTTP와 같은 고정 문구다(입력 내용 없음).

### 4. 배치와 운영

- **listener:** `MONTRACER_INGRESS_GRPC_ADDR`로 정하고 기본은 `:4317`(OTLP 기본 포트)이다. `off`면 끈다.
  - HTTP와 같은 process·Handler·in-flight 상한을 쓴다.
  - bind를 먼저 해서 포트 충돌을 기동 오류로 낸다.
- **종료:** HTTP `Shutdown`과 gRPC `GracefulStop`을 **같은 deadline(20초)으로 동시에** 시작한다. 진행 중인 요청이 Kafka append·응답을 마치게 한다. 시간 안에 못 끝난 gRPC stream은 강제로 닫는다. client는 재전송하고 dedup이 흡수한다.
- **연결 자원 상한**
  - 연결당 동시 stream 32개다. 전체 동시 처리는 HTTP와 같은 in-flight 상한(256)이다.
  - **연결 수명 2분(+정리 30초)**: HTTP/2 장기 연결이 한 replica에 고정되면 ADR 0024 §3의 전제("요청이 replica에 고르게 나뉜다")가 깨진다. 그러면 tenant가 rate 한도의 1/N만 쓰는 내부 원인 429가 생긴다. 연결을 주기적으로 다시 맺게 해 다시 나뉘게 한다.
  - 10초보다 잦은 keepalive ping은 끊고, 5분 idle 연결은 닫는다.
- **TLS·LB:** TLS는 LB에서 끝낸다(ADR 0020). gRPC는 LB → ingress 구간을 **HTTP/2 평문(h2c)**으로 보내야 한다. LB는 **HTTP/2를 아는 L7**이어야 요청 단위로 나뉜다. L4 LB면 연결 수명 상한에만 기댄다. 배포 템플릿(Helm)에서 정한다.
- **지표:** `montracer_ingress_requests_total`에 `transport`(`http`·`grpc`) label을 더했다(고정 enum). 경보는 signal별 합이라 그대로다.
- **로그:** `otlp request` 로그에 `transport`를 남긴다.
- **집계되지 않는 거절:** 등록되지 않은 method(UNIMPLEMENTED)는 handler 전에 grpc가 거절하므로 지표·로그에 없다. 그 밖의 거절(인증·크기·압축·quota)은 handler 안이라 `transport="grpc"`로 센다.
- **로컬:** `make dev`는 gRPC를 `127.0.0.1:18317`에서 연다. `make seed`는 globex trace를 gRPC로 보내고, `make smoke`가 그 trace를 조회한다. CI 통합 job에서 전 구간을 확인한다.

### 5. 근거 (2026-10-06 확인)

| 결정 | 근거 | 내용 | 채택 |
|---|---|---|---|
| 재시도 코드, RESOURCE_EXHAUSTED + RetryInfo, 과부하는 UNAVAILABLE + RetryInfo, partial success 재시도 금지, 4317, gzip 필수 | OTLP 규격 ([opentelemetry.io/docs/specs/otlp](https://opentelemetry.io/docs/specs/otlp/)) | 재시도 가능 코드는 CANCELLED·DEADLINE_EXCEEDED·ABORTED·OUT_OF_RANGE·UNAVAILABLE·DATA_LOSS다. RESOURCE_EXHAUSTED는 RetryInfo가 있을 때만 재시도한다. 처리량을 못 따라가면 UNAVAILABLE + RetryInfo다 | 그대로 채택(§3) |
| 한 서비스가 gRPC·HTTP를 함께 받는다 | Honeycomb ([Send Data with OpenTelemetry](https://docs.honeycomb.io/send-data/opentelemetry)) | OTLP를 gRPC·HTTP/protobuf·HTTP/JSON으로 받는다. gRPC 엔드포인트는 `api.honeycomb.io:443`이고 인증은 header다 | 채택: 같은 key·같은 처리. 인증 header는 OTLP 관례인 `authorization: Bearer` |

## 후보

| 결정 | 채택 | 대안과 기각 이유 |
|---|---|---|
| 메시지 수신 | raw byte + 같은 `otlp.Decode` | pdata gRPC server(struct로 풀림): 복잡도 pre-scan이 decode 뒤가 되어 깊은 중첩·대량 요소 방어(ADR 0017)가 HTTP와 달라진다 |
| 처리 경로 | 공통 `process` | gRPC 전용 handler: quota·redaction·ACK 규칙이 두 곳에서 어긋날 위험 |
| 포트 | 같은 process 별도 listener | 별도 binary: 같은 key 조회·quota·producer를 두 번 운영하게 된다 |

## Rollback

`MONTRACER_INGRESS_GRPC_ADDR=off`로 끈다. HTTP 경로는 그대로다.

## 증거

- `internal/ingest` gRPC 시험(in-process bufconn, pdata OTLP gRPC client)
  - **인증 없는 oversize 요청은 UNAUTHENTICATED**(본문을 인증 전에 받지 않음), in-flight 상한이 차면 본문 전에 UNAVAILABLE + RetryInfo
  - 느린 송신은 수신 상한에서 끊김, server에는 OTLP 서비스 3개만
  - 승인과 tenant header, gzip, partial success(environment 밖 resource)
  - 인증 없음·없는 key는 UNAUTHENTICATED, scope 밖은 PERMISSION_DENIED, 같은 key로 다른 서비스는 됨
  - rate 한도는 RESOURCE_EXHAUSTED + RetryInfo 1초, burst 초과는 RetryInfo 없음, Kafka 실패는 UNAVAILABLE
  - 크기 초과·gzip 압축 해제 후 초과는 RESOURCE_EXHAUSTED이고 produce 없음
  - 로그에 payload·key 없음
- HTTP 시험 전부가 공통 `process` 분리 뒤에도 통과한다.
- CI 통합 job: `make seed`가 globex trace를 gRPC로 보내고 `make smoke`가 query-api로 3 span을 조회한다.
- spec-reviewer 지적 반영: 인증 전 수신·압축 해제(stream handler), 연결 수명·stream 상한과 LB 요구, 수신 상한, 동시 종료, RawCodec 범위 고정, TLS·h2c, 집계 공백 명시, demo 시도별 상한, 415 문구 원복
