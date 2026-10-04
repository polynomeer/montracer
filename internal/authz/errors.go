package authz

import "errors"

// 권한 판단 결과. HTTP·gRPC 응답 매핑은 internal/apierr에서 한다.
var (
	// ErrUnauthenticated: 자격 증명이 없거나 유효하지 않다. 원인(미존재·폐기·만료)을 구분해 노출하지 않는다.
	ErrUnauthenticated = errors.New("authz: unauthenticated")
	// ErrForbidden: 같은 tenant 안에서 작업 권한이 없다.
	ErrForbidden = errors.New("authz: forbidden")
	// ErrStepUpRequired: 권한은 있으나 MFA step-up이 필요하다 (D04 §02).
	ErrStepUpRequired = errors.New("authz: step-up required")
	// ErrNotFound: 다른 tenant의 resource다. 존재 여부를 숨기기 위해 not found로 표현한다.
	ErrNotFound = errors.New("authz: not found")
	// ErrBackendUnavailable: 인증 정보 저장소(제어 DB 등)에 닿지 못했다. fail closed 하되
	// 의존 서비스 실패(503)로 보고한다 (D02 §19).
	ErrBackendUnavailable = errors.New("authz: auth backend unavailable")
)
