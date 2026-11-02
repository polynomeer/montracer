package queryplan

import (
	"strings"
	"testing"
)

// resource 열(ADR 0075 §3): 값 없는 행(”)은 어떤 값 비교와도 맞지 않는다(neq 포함). exists는 값이 있음. 빈 값·256 byte는 400.
func TestResourceFields(t *testing.T) {
	cases := map[string]string{
		`{"field":"service.version","op":"eq","value":"1.2.0"}`:       "service_version != '' AND service_version = {f0:String}",
		`{"field":"k8s.pod.name","op":"neq","value":"redis-0"}`:       "k8s_pod_name != '' AND k8s_pod_name != {f0:String}",
		`{"field":"service.instance.id","op":"in","value":["a","b"]}`: "service_instance_id != '' AND has({f0:Array(String)}, service_instance_id)",
		`{"field":"k8s.namespace.name","op":"exists"}`:                "k8s_namespace_name != ''",
	}
	for in, want := range cases {
		c, err := Compile(parse(t, in), LogCatalog)
		if err != nil || c.SQL != want {
			t.Errorf("%s: %q %v, want %q", in, c.SQL, err, want)
		}
		if _, err := Compile(parse(t, in), TraceSpanCatalog); err != nil {
			t.Errorf("trace span catalog: %s: %v", in, err)
		}
	}
	for _, bad := range []string{
		`{"field":"service.version","op":"eq","value":""}`,
		`{"field":"service.version","op":"eq","value":"` + strings.Repeat("v", MaxResourceValueLen+1) + `"}`,
		`{"field":"k8s.pod.name","op":"contains","value":"redis"}`,
		`{"field":"k8s.pod.name","op":"in","value":["ok",""]}`,
	} {
		if _, err := Compile(parse(t, bad), LogCatalog); err == nil {
			t.Errorf("accepted %s", bad)
		}
	}
	// trace 요약: root_service_version은 root가 없으면 ''이라 맞지 않는다
	c, err := Compile(parse(t, `{"field":"root_service_version","op":"in","value":["1.2.0"]}`), TraceSummaryCatalog)
	if err != nil || !strings.Contains(c.SQL, "if(roots > 0, root_service_version, '') != ''") {
		t.Fatalf("root version: %q %v", c.SQL, err)
	}
	if !IsTraceSummaryField("root_service_version") || IsTraceSummaryField("service.version") {
		t.Error("stage of version fields")
	}
}
