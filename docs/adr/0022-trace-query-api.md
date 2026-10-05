# ADR 0022: trace 단건 조회 API — 인가 범위, 구조 완결성, 응답 meta

- 상태: 승인 (결정 위임)
- Owner: API lead
- 승인자: polynomeer — 결정 사항은 빅테크 서비스 사례를 기준으로 정하고 근거를 기록하라는 지시 (2026-10-05, ADR 0021과 같은 위임). 근거는 §7
- 날짜: 2026-10-05 (제안·결정)
- 관련: F02, E03 · D02 §12~13, §15, §19, §22 · D04 §01~02 · ADR 0014, 0015, 0018, 0021

## 배경

D02 §13이 정한 계약은 `GET /api/v1/traces/{trace_id}`이다.

- 입력: from, to, 선택 service_id
- 응답: data.spans, complete, meta
- trace ID 단건 조회도 최대 7일의 명시적 경계를 요구한다.
- 권한 밖 span과 그 구조는 노출하지 않는다.

D02 §22는 root 누락, parent 누락, truncation, late arrival을 검사하고 `reasons[]`와 `last_updated_at`을 반환하라고 한다.

다음은 정하지 않았다.

- 보이는 span이 없을 때의 status
- environment 범위가 제한된 key의 처리
- 아직 계산하지 않는 meta 값의 표현
- span 상한
- 사람(session) 인증이 없는 지금의 인증 경로

## 결정

### 1. 경로와 인증

- `GET /api/v1/traces/{trace_id}?from=<RFC3339>&to=<RFC3339>[&service_id=<UUID>]`를 `cmd/query-api`가 제공한다.
- **인증은 지금 API key(bearer)만 받는다.** ingest key는 401이다(D02 §12). 브라우저 session cookie와 CSRF는 OIDC를 붙이는 control-api 작업에서 추가한다.
- 인증 저장소 장애는 503으로 거절한다(fail closed, D02 §02).
- from·to는 필수이고 최대 7일이다(D02 §13). 범위를 넘으면 `QUERY_BUDGET_EXCEEDED`(422)이다.

### 2. 보이는 span이 없으면 404

- 다음 경우를 **모두 같은 404 `NOT_FOUND`로** 돌려준다.
  - 다른 tenant의 trace
  - environment 범위 밖 trace
  - 존재하지 않는 trace
  - 보존 기간이 지난 trace
- 응답 본문, status, 소요 시간 구조 어디로도 존재 여부를 구별할 수 없게 한다(D02 §12 "존재 여부 누출 금지", D04 §01).

### 3. environment 범위 (D04 §02)

- environment가 제한된 API key는 범위 밖 span을 응답에서 뺀다. resource에 environment가 없는 span도 범위 안임을 증명할 수 없으므로 뺀다.
- 제한이 없는 key와 사람은 모두 본다(MVP 조직 단위 RBAC).
- 뺀 span의 자식은 `missing_parent`로 보인다. 이것은 미도착·sampling 누락과 **구별하지 않는다.** 구별하면 범위 밖 span의 존재가 드러나기 때문이다.
- `authz.Principal.EnvironmentRestricted()`를 추가해 이 판단을 authz에 둔다.

### 4. 구조 완결성 (D02 §22)

`complete = (reasons가 비어 있음)`이다. 관측 범위 안의 구조만 판단하며, source의 모든 span이 도착했다고 증명하지 않는다.

| reason | 조건 |
|---|---|
| `missing_root` | parent가 없는 span이 보이지 않음 |
| `missing_parent` | parent_span_id가 가리키는 span이 보이지 않음 (미도착·sampling·권한 범위 밖) |
| `span_limit_reached` | span 상한 10,000에 걸림 |
| `payload_undecodable` | 저장된 payload를 해석하지 못한 span이 있음 (그 span은 응답에서 뺌) |

- `last_updated_at`은 보이는 span 중 가장 늦은 ingress 수신 시각이다. 늦게 도착하는 span이 있을 수 있음을 UI가 표시하는 기준이다.
- late arrival 자체는 판정하지 않는다. 판정하려면 trace 종료 기준이 필요하고, 이는 tail sampling의 decision_wait와 함께 정한다(ADR 005).

### 5. 응답 형식

- span은 다음을 담는다: 저장된 OTLP payload에서 복원한 **타입 있는** 속성, resource 속성, events, links, status message, kind(문자열), start·end 시각(RFC3339 UTC), `duration_ns`.
- `parent_span_id`와 `environment`는 없으면 null이다.
- JSON으로 표현할 수 없는 NaN·±Inf 속성 값은 문자열 `"NaN"`·`"+Inf"`·`"-Inf"`로 쓴다. 0이나 null로 바꾸지 않는다(계약 6).
- **meta:** request_id, schema_version 1, partial=false, failed_shards=[], warnings=[]를 둔다. 아직 계산하지 않는 값은 **null**이다. 0·true·100%로 채우지 않는다(D02 §19 "coverage의 분모를 모르면 null", 계약 6).
  - `watermark`: 수집 watermark가 아직 없다.
  - `sampled`: sampling 정책을 아직 추적하지 않는다.
  - `coverage`: 분모를 모른다.
  - `resolution_seconds`: trace에는 해당 없다.
  - `scan_bytes`: 실행 통계를 아직 수집하지 않는다.
- `Cache-Control: no-store`. tenant 데이터이기 때문이다.

### 6. 저장소 조회

- `telemetrystore.TraceSpanRecords`가 lookup → 원본 순서로 읽는다(ADR 0018). 같은 span key는 최초 수신 행 하나만 남긴다(ADR 0021 §4).
- 결과는 시작 시각 순서이고 상한은 `MaxTraceSpans`(10,000)이다. 이 값은 query 계정의 기본 결과 행 예산(`max_result_rows`)과 같다.
- service_id 필터는 lookup 단계에서 적용하고, 사용자 값은 parameter binding으로만 넣는다(D02 §15).
- 요청당 저장소 조회 상한은 10초다. query 계정의 `max_execution_time` 5초가 먼저 걸리면 504 `QUERY_TIMEOUT`이다.

### 7. 외부 사례 근거 (2026-10-05 확인)

| 결정 | 사례 | 내용 | 채택 |
|---|---|---|---|
| §2 존재 비노출 404 | GitHub REST API ([Troubleshooting](https://docs.github.com/en/enterprise-server@3.17/rest/overview/troubleshooting)) | 권한 없는 private resource 요청에 403 대신 404를 돌려 존재를 숨김 | 채택: 다른 tenant·범위 밖·없음·만료를 같은 404로 |
| §1 시간 경계 | Grafana Tempo ([API docs](https://grafana.com/docs/tempo/latest/api_docs/)) | `GET /api/traces/<id>?start&end`. 범위를 주면 그 block만 찾고, 범위 밖이면 못 찾거나 일부만 나올 수 있다고 명시 | 부분 채택: D02 §13에 따라 범위를 **필수**로 한다(Tempo는 선택). 범위 밖이면 일부만 나올 수 있다는 점은 `missing_*` reason으로 드러난다 |
| §4 구조 경고 | Jaeger ([Troubleshooting](https://www.jaegertracing.io/docs/1.43/troubleshooting/)) | parent가 없는 span을 "invalid parent span IDs"로 표시하고 trace를 숨기지 않음 | 채택: 숨기지 않고 `missing_parent` reason으로 표시 |
| §3 범위 밖 span | GitHub(위)와 같은 원칙 | 권한 밖 resource의 존재를 응답 차이로 드러내지 않음 | 채택: 범위 밖 parent와 미도착 parent를 같은 reason으로 표시 |

- **meta null 원칙은 사례가 아니라 명세에서 왔다.** D02 §19가 "coverage 분모를 모르면 null, 100%를 임의로 채우지 않는다"고 하고, CLAUDE.md 계약 6과 같다.

## 후보

| 결정 | 채택 | 대안과 기각 이유 |
|---|---|---|
| 보이는 span 없음 | 404 | 200 + 빈 배열: D02 §19는 "missing data는 200의 빈 배열"이라고 하지만, 이 경로는 단일 resource 조회라 §19의 "404=존재하지 않거나 숨겨진 resource"에 해당한다 |
| 범위 밖 span 처리 | 뺀 뒤 missing_parent로 흡수 | 별도 reason(`hidden_by_scope`): 범위 밖 span이 있다는 사실이 드러난다 |
| environment 없는 span | 제한 key에서는 숨김 | 보여줌: 범위 안임을 증명할 수 없는데 노출하게 된다 |
| 모르는 meta | null | 0·true·기본값: 계약 6 위반 |
| span 상한 | 10,000 + reason | 상한 없음: 결과 행 예산 초과로 422가 나서 trace를 전혀 볼 수 없다. 더 작은 상한: 정상 대형 trace도 truncation된다 |

## 결과

- 수집에서 조회까지 한 줄로 이어진다: OTLP → ingress → Kafka → worker → ClickHouse → `GET /api/v1/traces/{id}`.
- 남은 일
  - OpenAPI 3.1 원천과 계약 테스트(E03)
  - 사람 session 인증(control-api)
  - waterfall UI mock(E04)
  - 조회 metric과 rate limit(60 req/min, D02 §12)
  - watermark·sampled 계산

## Rollback

`cmd/query-api` 배포를 되돌린다. 저장소와 schema 변경은 없다.

## 재검토 조건

- 대형 trace(10,000 span 초과)가 흔해질 때. 그때는 span 목록 pagination이나 요약 응답을 검토한다.
- session 인증이나 field ACL을 도입할 때. 그때는 environment 외 범위 필터를 같은 위치에 둔다.

## 증거

- `internal/query` 단위 테스트
  - 응답 형식: 타입 있는 속성, NaN 문자열, events, null parent, `no-store`
  - meta null 필드
  - completeness 3종, 상한·해석 불가 reason
  - environment 제한 key의 범위 밖·environment 없는 span 숨김과 전부 숨겨질 때 404
  - 401(토큰 없음·ingest key·잘못된 토큰), 403, 400(from·to), 404, 503(저장소·인증 저장소), 내부 문구 비노출
- 통합 테스트 `TestVerticalSliceIngestToQuery` (PostgreSQL + Kafka + ClickHouse)
  - 실제 key로 수집하고 재전송해도 span 4개, complete
  - service_id 필터 결과가 missing_root·missing_parent
  - 다른 tenant 404, staging 제한 key 404, ingest key 401
