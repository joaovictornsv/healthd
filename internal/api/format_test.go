package api

import "testing"

func TestThousands(t *testing.T) {
	for n, want := range map[int]string{0: "0", 999: "999", 1536: "1,536", -1536: "-1,536", 1234567: "1,234,567"} {
		if got := Thousands(n); got != want {
			t.Errorf("Thousands(%d) = %q, want %q", n, got, want)
		}
	}
	if got := FormatValue("errors", 1536); got != "1,536 errors" {
		t.Errorf("FormatValue = %q", got)
	}
}
