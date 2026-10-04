// Package authz는 인증된 principal, tenant 경계, 조직 단위 RBAC를 정의한다.
//
// 변경 불가 계약 (D01 §02, D04 §01):
//   - tenant는 인증 결과(Principal)에서만 얻는다. payload, X-Tenant-ID, URL 값은
//     권한 근거가 아니며 Principal의 tenant와 비교 대상일 뿐이다.
//   - 다른 tenant의 resource 접근은 존재 여부를 숨기기 위해 ErrNotFound로 거절한다.
//   - deny가 allow보다 우선하고, API key는 발급자 권한보다 강해질 수 없다.
//
// MVP(M0)는 조직 단위 role만 지원한다. team·environment scope는 G1(F08)에서 추가한다.
package authz
