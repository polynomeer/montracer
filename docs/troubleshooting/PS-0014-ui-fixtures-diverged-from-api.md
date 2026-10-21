# PS-0014: 화면 시험 fixture가 실제 API 응답과 달라도 시험이 통과함

- 날짜: 2026-10-19 (발견) / 2026-10-19 (해결)
- 영역: web · query · control-api · CI
- 영향: 화면이 실제 응답에서 깨질 수 있는 상태를 시험이 보지 못함 — 개발 생산성·정확성 (P2)
- 수정: polynomeer/montracer#58
- 관련: ADR 0059, D02 §12·§19, D06 §11

## 증상

OpenAPI 원천을 만들고 화면 시험(`apps/web`)의 fetch 대역 응답을 원천으로 검사하자, 263개 중 83개 시험이 fixture 불일치로 실패했다. 그때까지 화면 시험은 모두 통과하고 있었다.

- 조회 `meta`에서 null 필드(watermark·sampled·coverage·resolution_seconds·scan_bytes)가 빠짐
- metric 조회 data에 `source_window_seconds`·`range`가 없음
- 서비스 항목의 service_id가 UUID가 아니고 first_seen·last_seen이 빈 문자열, 필수 필드 누락
- notification policy 목록 항목에 created_at 등 필드 누락
- dashboard 응답에 조회 API의 meta를 씀

서버 쪽에서도 같은 종류가 나왔다.
- control-api `meta`의 `warnings` 유무가 응답마다 달랐다.
- handler 시험의 log 행 service_id가 빈 값이었다.
- 요청에 `Content-Type`이 없었다(격리 suite 포함).

## 발견 경위

ADR 0059의 consumer 검사(`src/test/setup.ts`가 `vi.stubGlobal('fetch')`를 감쌈)와 provider strict 검사(`tests/contract`)를 처음 연결할 때 드러났다. 늦게 발견된 이유는 fixture를 원천 없이 화면 type(`api/types.ts`)을 보고 손으로 썼기 때문이다. 그 type도 손으로 옮긴 것이라, 둘이 함께 실제 응답과 어긋날 수 있었다.

## 원인

- API 모양의 기계 원천이 없었다. 서버 응답, 화면 type, 시험 fixture가 각자 손으로 유지됐다.
- 화면 코드가 빠진 필드를 대개 견뎌서(옵셔널 접근) 시험이 실패하지 않았다. 대신 null과 빠짐의 구분(계약 6) 같은 경계는 시험되지 않았다.

## 해결

- fixture를 실제 응답 모양으로 고쳤다. 공용 builder(`src/test/fixtures.ts`의 `queryMeta`·`controlMeta`·`metricData`·`serviceFixture`)를 쓴다.
- control-api 모든 응답 `meta`에 `warnings`를 넣었다(additive).
- 버린 대안: 원천을 느슨하게(필드 선택, `additionalProperties` 허용) 써서 기존 fixture를 통과시키기. 그러면 원천이 실제 응답을 설명하지 못한다.

## 재발 방지

- 시험: `apps/web/src/test/setup.ts`가 모든 시험의 fetch 대역 교환을 원천으로 검사한다. 선언 안 된 field, 빠진 필수 field, status별 error.code를 본다. 검사기 자체 시험은 `src/test/contract.test.ts`다.
- 시험: `internal/query`·`internal/controlapi`의 `TestMain`(`contract.Run`)이 handler 교환을 strict하게 검사하고, operation마다 성공 응답 coverage를 본다. `tests/isolation`도 같은 검사를 한다.
- 검사: CI `OpenAPI breaking change` 단계(`scripts/dev/apidiff`)

## 교훈

손으로 쓴 fixture는 그 자체로 "두 번째 원천"이 된다. 같은 원천으로 provider와 consumer를 둘 다 검사해야 fixture가 실제와 같다는 것을 믿을 수 있다.
