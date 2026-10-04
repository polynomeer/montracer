package authz

import (
	"encoding/hex"
	"errors"
)

// TenantID는 조직 식별자(UUID)다.
//
// 값 자체는 신뢰 근거가 아니다. 요청 처리에서 권한 판단에 쓰는 TenantID는
// 반드시 Principal.Tenant()에서 얻어야 한다. ParseTenantID는 저장소·설정에서
// 읽은 값을 다루거나, 외부 입력을 Principal의 tenant와 비교할 때만 쓴다.
type TenantID struct {
	b [16]byte
}

var errInvalidTenantID = errors.New("authz: invalid tenant id")

// ParseTenantID는 canonical UUID 문자열(8-4-4-4-12, 대소문자 무관)을 파싱한다.
// nil UUID(모두 0)는 거절한다.
func ParseTenantID(s string) (TenantID, error) {
	var t TenantID
	if len(s) != 36 || s[8] != '-' || s[13] != '-' || s[18] != '-' || s[23] != '-' {
		return t, errInvalidTenantID
	}
	compact := s[0:8] + s[9:13] + s[14:18] + s[19:23] + s[24:36]
	if _, err := hex.Decode(t.b[:], []byte(compact)); err != nil {
		return TenantID{}, errInvalidTenantID
	}
	if t.IsZero() {
		return TenantID{}, errInvalidTenantID
	}
	return t, nil
}

// IsZero는 값이 설정되지 않았는지 보고한다.
func (t TenantID) IsZero() bool { return t == TenantID{} }

// String은 소문자 canonical UUID 표현을 반환한다.
func (t TenantID) String() string {
	var buf [36]byte
	hex.Encode(buf[0:8], t.b[0:4])
	buf[8] = '-'
	hex.Encode(buf[9:13], t.b[4:6])
	buf[13] = '-'
	hex.Encode(buf[14:18], t.b[6:8])
	buf[18] = '-'
	hex.Encode(buf[19:23], t.b[8:10])
	buf[23] = '-'
	hex.Encode(buf[24:36], t.b[10:16])
	return string(buf[:])
}
