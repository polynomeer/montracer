# ADR 0061: 화면 API 타입을 OpenAPI 원천에서 생성 — `@montracer/query-schema`

- 상태: 승인 (결정 위임 — polynomeer, 2026-10-05 "빅테크 사례 기준으로 결정하고 근거를 기록". 2026-10-19 "merge하고 이어서 진행해줘", ADR 0059 §1 후속)
- Owner: Frontend lead
- 날짜: 2026-10-19 (제안·결정)
- 관련: D02 §12("OpenAPI 3.1 원천으로 코드 생성"), D06 §11 · `packages/README.md`(`query-schema`: `api/openapi`에서 생성·검증) · ADR 0013, 0041, 0059, 0060

## 배경

- ADR 0059는 원천(`api/openapi/montracer.yaml`)을 만들고 서버·화면 fixture를 그것으로 검사했다. 코드 생성은 후속으로 미뤘다.
- 화면 응답 타입(`apps/web/src/api/types.ts`)은 손으로 옮긴 것이라 실제 응답과 달랐다(리뷰에서 발견). 대표 예가 `Meta`다.
  - 한 타입이 조회·관리 API를 모두 덮었다.
  - 관리 API에 없는 `partial`·`failed_shards`가 필수였다.
  - `watermark` 등은 optional이었지만 실제로는 늘 오고 null일 수 있다.
  - DELETE 204에 지어낸 meta에도 조회 API field를 넣었다.

## 결정

### 1. 생성 위치와 도구

- `packages/query-schema`가 원천을 openapi-typescript(7.13.0)로 `src/schema.gen.ts`에 생성한다. 생성 파일은 커밋한다(빌드 없이 화면이 바로 읽고, 리뷰에서 diff로 보인다).
  - `src/index.ts`가 `paths`·`components`·`operations`와 `Schema<'이름'>` helper를 내보낸다.
- **TypeScript 5.9는 생성 전용이다.** openapi-typescript는 TypeScript compiler API를 쓰는데, 레포의 TypeScript 7(native)에는 그 JS API가 없다(7로 돌리면 실패). 그래서 이 package에만 5.9.3을 devDependency로 두고, 화면은 TypeScript 7로 생성 결과(타입 선언)만 읽는다.
- **원천과 생성 파일이 다르면 실패한다.** `packages/query-schema`의 `test`가 다시 생성해 비교한다. CI js job(`pnpm run test`)이 돈다. 다시 만들기는 `pnpm --filter @montracer/query-schema run generate`다.

### 2. 화면 타입은 생성 타입의 별칭

- `apps/web/src/api/types.ts`의 응답 타입은 `Schema<…>` 별칭이다(Service·MetricSeries·MetricItem·Trace·TraceSpan 등).
- **`Meta`는 실제 응답의 합집합**(`QueryMeta | ControlMeta | MonitorGroupsMeta`)이다. API마다 다른 field는 helper(`metaPartial`·`metaWatermark`·`metaTruncated`)로 읽는다. 관리 API meta에 `partial`이 없다는 것을 타입이 강제한다.
- 검색 행(`LogItem`·`TraceSummary`)은 `Required<LogRow|TraceRow>`다. 원천의 행 schema는 projection 때문에 모두 선택이지만, 화면은 기본 projection(모든 field)을 쓴다.
- 오류 `ApiErrorBody`는 원천의 `Error`에 화면이 만드는 code(`NETWORK`·`BAD_RESPONSE`·`CLIENT`·`HTTP_<status>`)를 더한다.
- **요청 builder 타입은 화면 쪽에 둔다**(`MetricFilter`·`FilterNode` 등). 화면이 실제로 보내는 부분집합(예: metric filter는 `and`·`eq`만)이 원천보다 좁아야 하기 때문이다. 보낸 요청이 원천과 맞는지는 consumer 계약 검사(ADR 0059 §4)가 본다.
- 원천 보정: metric 사전의 `aggregations`를 `string[]`에서 `MetricAggregation` enum 배열로 좁혔다(서버는 그 enum만 준다). 응답 쪽을 좁힌 변경이라 호환을 깨지 않는다.

### 3. 범위 (이번 PR)

- 바꾼 것: query-api 응답 타입 전부(`types.ts`)와 `Meta`, client의 204 meta, 시험 fixture의 meta 타입
- control-api feature 타입(`api/monitors.ts`·`dashboards.ts`·`notificationPolicies.ts`)도 별칭으로 옮겼다(2026-10-19, 다음 PR). 편집기가 만드는 `DashboardSpec`·`Widget`·label 조건(`LabelFilter`)도 원천 타입이라 원천과 다른 정의를 만들 수 없다.
- 화면 쪽에 남는 타입은 요청 builder(`MetricQuery`·`MetricFilter`·`FilterNode`·`SearchRequest`·`PolicyInput`)와 화면 상태 타입(`RemoteState`·`PolicyWithSecret` 등)뿐이다.
- 서버 Go 코드 생성(server stub)은 하지 않는다(ADR 0059 §1). 서버 쪽 일치는 provider 계약 검사가 맡는다.
- **알려진 한계:** client는 런타임에 어떤 문자열 error code도 받지만, 타입은 원천 enum과 화면 code의 합집합으로 단정한다. 서버가 code를 더하면(응답 enum 확장은 호환으로 본다) 타입이 좁아진다. 화면이 code로 분기하기 시작할 때 모르는 code의 fallback 분기를 함께 둔다.

### 4. 외부 사례 근거 (2026-10-19 확인)

| 결정 | 사례 | 내용 | 채택 |
|---|---|---|---|
| API 설명에서 TypeScript 타입을 생성해 package로 배포·사용 | GitHub `@octokit/openapi-types` ([octokit/openapi-types.ts](https://github.com/octokit/openapi-types.ts)) | GitHub OpenAPI spec을 계속 TypeScript 정의로 바꿔 npm package로 낸다 | 채택: 원천에서 생성한 타입을 workspace package로 두고 화면이 그것만 쓴다 |

## 후보

| 결정 | 채택 | 대안과 기각 이유 |
|---|---|---|
| 생성기 | openapi-typescript | @hey-api/openapi-ts: client·SDK까지 만들어 기존 `apiRequest`와 겹치고, 같은 TS compiler API 제약이 있다. 직접 만든 생성기: 3.1 키워드(nullable union, patternProperties)를 다시 구현해야 한다 |
| TS 7 비호환 | 생성 package에만 TS 5.9 | 레포 전체 TS 5로 내리기: ADR 0041의 TypeScript 7 결정을 되돌린다 |
| 생성 파일 | 커밋 + 신선도 시험 | 빌드 때마다 생성: dev server·typecheck 앞에 생성 단계가 생기고, 리뷰에서 타입 변화가 안 보인다 |
| 요청 타입 | 화면 쪽 좁은 타입 유지 | 생성 타입 그대로: 화면이 보내지 않는 연산(or·in 등)까지 허용돼 타입이 화면 계약을 표현하지 못한다 |

## Rollback

- `apps/web/src/api/types.ts`를 이전 손 타입으로 되돌리고 `@montracer/query-schema` 의존을 지운다. 원천·계약 시험은 그대로다.

## 증거

- `packages/query-schema` `test`: 다시 생성한 결과와 커밋된 `schema.gen.ts`가 같다(파일을 바꾸면 실패를 확인)
- `apps/web` typecheck(TypeScript 7)·시험 267개 통과
  - 별칭으로 바꾸자 `Meta`를 쓰던 곳 10곳이 타입 오류로 드러났고 helper로 고쳤다.
  - 시험 fixture 2개(trace 행 `last_updated_at`, dashboard metric 결과의 `source_window_seconds`·`range`)도 오류로 드러났다.
- `pnpm audit --audit-level high`: 알려진 취약점 없음

## 변경 이력

- 2026-10-19: spec-reviewer 반영 — `metaPartial` 의미 주석, `MetricLabels`·fixture meta도 생성 타입, error code 타입 한계, package·web README와 architecture.
- 2026-10-19: control-api 응답·정의 타입(monitor·dashboard·notification policy)도 생성 타입 별칭으로 옮겼다. 원천의 dashboard widget `aggregation`을 `MetricAggregation` 참조로 정리(같은 enum).
