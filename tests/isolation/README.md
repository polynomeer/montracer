# tests/isolation

cross-tenant 공격 시험이다 (D04 §01 필수 테스트, D06 §03, M0 Gate Q1). `make test-isolation`으로 돌리고, CI는 모든 PR의 통합 job(`-tags=integration ./...`)에서 함께 돌린다.

## 방식

- 실제 PostgreSQL(RLS)과 ClickHouse(row policy) 위에서 동작한다.
- `query-api`·`control-api` handler를 그대로 띄운다. 인증은 실제 key 발급·조회 경로다.
- 데이터는 수집과 같은 경로로 쓴다: envelope → pipeline batch → ClickHouse sink. Kafka만 생략한다.
- **대조군을 함께 둔다.** 공격자 tenant의 실패만 보면, 데이터가 아예 없을 때도 통과해 버린다. 그래서 피해 tenant가 자기 데이터를 실제로 읽는지 먼저 확인한다.
- **존재 누출을 본다.** 다른 tenant 자원에 대한 응답은 "없는 자원"과 status·오류 코드·문구가 같아야 한다(request_id 제외, D04 §01 "오류 메시지에 다른 조직의 존재가 누출되지 않는지").

## 시험

| 공격 | 기대 |
|---|---|
| B가 A의 trace ID로 조회(IDOR) | 없는 trace와 같은 404 |
| B가 `X-Tenant-ID`·`X-Scope-OrgID`·`X-Forwarded-Tenant` header나 `?tenant_id=`로 A를 지정 | 무시, 같은 404(tenant는 principal에서만, 계약 1) |
| ingest key로 조회·관리 API 호출 | 401 |
| 폐기된 key | 없는 key와 같은 401 |
| B가 A의 감사 행을 조회 | 보이지 않음 |
| B, 또는 A의 운영 범주 전용 key가 A의 감사 cursor를 재사용 | 400 `INVALID_ARGUMENT` |
| 운영 범주 전용 key로 security 감사 | 보이지 않음 |

## 다른 층의 격리 시험

같은 계약을 저장소 층에서도 본다.

- `internal/controldb`: RLS, tenant context 없으면 0행, break-glass 다른 tenant key
- `internal/telemetrystore`: row policy만으로도 0행, `merge()` 우회 차단
- `internal/apicursor`·`internal/controlapi`: cursor binding

## 아직 없는 것 (D04 §01 필수 테스트 중)

stream 재연결, export URL, dashboard ID, cache key 충돌, 삭제 job selector 변조, 쿼리 AST 변조, timing. 해당 기능이 생기면 여기에 더한다.
