// Package isolation은 cross-tenant 공격 시험이다 (D04 §01 필수 테스트, D06 §03, `make test-isolation`).
//
// 실제 PostgreSQL(RLS)·ClickHouse(row policy) 위에서 query-api·control-api handler를 그대로 띄우고,
// tenant B가 tenant A의 ID·cursor·header·key 종류를 재사용하는 요청이 A의 존재조차 드러내지 않는지 본다.
// 시험은 integration build tag 아래에 있다(CI 통합 job이 모든 PR에서 실행).
package isolation
