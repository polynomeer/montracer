# ADR 0001: OpenTelemetry 우선 계측

- 상태: 승인
- Owner: Agent lead
- 승인자: polynomeer
- 날짜: 2026-10-03 (제안: D06 §08) / 2026-10-03 (결정)
- 관련: F01, F13, F14, E02, E06 · D02 §01, §04 · D03 §01~03 · ADR 0009(예정)

## 배경

수집 표준을 먼저 고정해야 ingress 프로토콜, canonical schema, SDK 지원 범위를 정할 수 있다. 자체 SDK·agent를 만들면 언어별 유지비가 커지고, 고객의 기존 OTel 계측을 재사용할 수 없다. 반면 Pinpoint 수준의 Java 진단(Inspector, call tree, thread dump)은 OTel 표준만으로 제공되지 않는다.

## 결정

- 수신 프로토콜은 **OTLP/gRPC·OTLP/HTTP** (protobuf, OTLP JSON, gzip)로 한다. 외부는 TLS 443, `/v1/traces|metrics|logs` (D02 §04).
- 고객 측 계측은 **OTel 공식 SDK와 Collector contrib 검증 배포판**을 기본으로 한다. 전파는 W3C Trace Context.
- Java 고급 진단은 OTel Java agent 위의 **opt-in extension**으로 추가한다 (상세는 ADR 0009에서 확정).
- Datadog·Pinpoint agent wire protocol drop-in 호환은 제공하지 않는다 (D01 §06).

## 후보

| 후보 | 이점 | 비용·위험 |
|---|---|---|
| A. OTel 우선 (채택) | 표준 생태계·고객 기존 계측 재사용, 언어 커버리지, 벤더 중립 | custom 기능은 extension·context bridge 유지비 |
| B. 자체 SDK/agent | 기능·성능 완전 통제 | 언어별 구현·유지 인력 과다, 고객 이관 비용 |
| C. Pinpoint fork | Java 진단 즉시 확보 | 라이선스·호환 의무, 다언어 확장 어려움, 이중 bytecode 변환 위험 |

## 결과

- 이점: ingress·schema를 OTLP 기준으로 바로 설계 가능. Go/Node/Python 지원을 OTel SDK로 확보.
- 비용: OTel semantic convention·Collector component 버전 변화 추적, 고정 버전별 validate·통합 시험 필요.
- 영향 받는 계약: `api/proto`에 OTLP proto pin, golden OTLP fixture (`tests/fixtures/otlp`), OTLP contract test.

## Rollback

표준 tracing은 유지한 채 특정 언어에 한해 자체 계측 모듈을 추가하는 방식으로 부분 전환한다. OTLP 수신 경로는 제거하지 않는다.

## 재검토 조건

- 지원 고객의 필수 계측(call-stack coverage 등)이 OTel + extension으로 충족되지 않음이 입증될 때.
- 기본 계측 부하가 예산(CPU +3%, p99 +5%)을 지원 matrix에서 반복 초과할 때.

## 증거

- 설계 근거: D02 §01, §04, §23 [R1, R2, R3, R6, R7, R11], D06 §08.
- 실측 증거: `tests/fixtures/otlp` golden fixture와 `internal/telemetry/otlp` 테스트(JSON·protobuf 경로 일치, hex ID). OTLP 모델은 pdata v1.68.0으로 고정. contract test는 ingress 구현 시 추가.
