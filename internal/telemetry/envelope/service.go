package envelope

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"

	"go.opentelemetry.io/collector/pdata/pcommon"

	"github.com/polynomeer/montracer/internal/authz"
)

// ServiceKey는 서비스 자연 키다 (D02 §08: tenant, environment, service.namespace, service.name).
type ServiceKey struct {
	Environment, Namespace, Name string
}

// ServiceKeyOf는 resource 속성에서 자연 키를 읽는다. 없는 이름은 OTel SDK 기본값 unknown_service다.
func ServiceKeyOf(resource pcommon.Map) ServiceKey {
	str := func(k, def string) string {
		if v, ok := resource.Get(k); ok && v.Type() == pcommon.ValueTypeStr && v.Str() != "" {
			return v.Str()
		}
		return def
	}
	return ServiceKey{
		Environment: str("deployment.environment.name", ""),
		Namespace:   str("service.namespace", ""),
		Name:        str("service.name", "unknown_service"),
	}
}

// ServiceID는 서비스 자연 키의 결정적 UUID다 (D02 §08, ADR 0038). worker가 원본 행의 service_id로, ingress가 catalog 키로 쓴다.
// SHA-256 앞 128비트에 UUID version 8·variant 비트를 넣는다(RFC 9562 §5.8).
func ServiceID(tenant authz.TenantID, resource pcommon.Map) string {
	return ServiceIDOf(tenant, ServiceKeyOf(resource))
}

// ServiceIDOf는 자연 키에서 service_id를 만든다.
func ServiceIDOf(tenant authz.TenantID, k ServiceKey) string {
	h := sha256.New()
	for _, s := range []string{"montracer.service.v1", tenant.String(), k.Environment, k.Namespace, k.Name} {
		_, _ = fmt.Fprintf(h, "%d:%s", len(s), s)
	}
	var b [16]byte
	copy(b[:], h.Sum(nil))
	b[6] = (b[6] & 0x0f) | 0x80
	b[8] = (b[8] & 0x3f) | 0x80
	x := hex.EncodeToString(b[:])
	return x[0:8] + "-" + x[8:12] + "-" + x[12:16] + "-" + x[16:20] + "-" + x[20:32]
}
