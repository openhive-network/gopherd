package evaluator

import "testing"

// TestDelegateRCMaxRCNumberForms pins fc's int64 reading of max_rc: a fraction
// truncates toward zero (mainnet block 108614906 delegates 8082000000 from
// "max_rc":8082000000.000001), and an exponent is refused.
func TestDelegateRCMaxRCNumberForms(t *testing.T) {
	for _, tc := range []struct {
		maxRC string
		want  int64
		ok    bool
	}{
		{`8082000000`, 8082000000, true},
		{`8082000000.000001`, 8082000000, true},
		{`8154999999.999999`, 8154999999, true},
		{`"5000"`, 5000, true},
		{`-1.5`, -1, true},
		{`1e10`, 0, false},
		{`"1.5"`, 0, false},
	} {
		payload := `["delegate_rc",{"from":"alice","delegatees":["bob"],"max_rc":` + tc.maxRC + `}]`
		ps, err := ParseRCCustomJSON(payload, true)
		if !tc.ok {
			if err == nil {
				t.Errorf("max_rc %s: accepted as %d, want refused", tc.maxRC, ps[0].MaxRC)
			}
			continue
		}
		if err != nil {
			t.Errorf("max_rc %s: %v", tc.maxRC, err)
			continue
		}
		if ps[0].MaxRC != tc.want {
			t.Errorf("max_rc %s: got %d want %d", tc.maxRC, ps[0].MaxRC, tc.want)
		}
	}
}
