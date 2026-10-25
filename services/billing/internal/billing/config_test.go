package billing

import "testing"

func TestParseFixedAmounts(t *testing.T) {
	m, err := ParseFixedAmounts("USD:2500, EUR:2500,COP:10000000")
	if err != nil || m["USD"] != 2500 || m["EUR"] != 2500 || m["COP"] != 10000000 || len(m) != 3 {
		t.Fatalf("%v %v", m, err)
	}
	for _, bad := range []string{"USD", "USD:x", "US:1", "USD:-1"} {
		if _, err := ParseFixedAmounts(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}
