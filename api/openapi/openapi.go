// Package openapi는 관리·조회 API의 OpenAPI 3.1 원천(montracer.yaml)을 담는다 (D02 §12, §20, ADR 0059).
// 계약 시험(tests/contract)과 도구가 이 파일을 읽는다. 원천은 YAML이고 Go 코드는 embed뿐이다.
package openapi

import _ "embed"

// Spec은 OpenAPI 3.1 문서(YAML)다.
//
//go:embed montracer.yaml
var Spec []byte
