package authz

import "testing"

func TestParseTenantID(t *testing.T) {
	valid := "4BF92F35-77B3-4DA6-A3CE-929D0E0E4736"
	got, err := ParseTenantID(valid)
	if err != nil {
		t.Fatalf("ParseTenantID(%q) error: %v", valid, err)
	}
	if got.String() != "4bf92f35-77b3-4da6-a3ce-929d0e0e4736" {
		t.Errorf("String() = %q, want lowercase canonical form", got.String())
	}

	for _, bad := range []string{
		"",
		"00000000-0000-0000-0000-000000000000", // nil UUID
		"4bf92f3577b34da6a3ce929d0e0e4736",     // dash 없음
		"4bf92f35-77b3-4da6-a3ce-929d0e0e473",  // 짧음
		"4bf92f35-77b3-4da6-a3ce-929d0e0e47366",
		"4bf92f35_77b3-4da6-a3ce-929d0e0e4736",
		"zbf92f35-77b3-4da6-a3ce-929d0e0e4736",
		"4bf92f35-77b3-4da6-a3ce-929d0e0e473\x00",
	} {
		if _, err := ParseTenantID(bad); err == nil {
			t.Errorf("ParseTenantID(%q) succeeded, want error", bad)
		}
	}
}
