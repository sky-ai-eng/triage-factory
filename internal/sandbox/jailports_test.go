package sandbox

import "testing"

func TestValidateJailPorts(t *testing.T) {
	tooMany := make([]int, MaxJailPorts+1)
	for i := range tooMany {
		tooMany[i] = 1000 + i
	}
	cases := []struct {
		name  string
		ports []int
		ok    bool
	}{
		{"none", nil, true},
		{"some", []int{443, 34567, 40001}, true},
		{"at_cap", tooMany[:MaxJailPorts], true},
		{"over_cap", tooMany, false},
		{"zero", []int{0}, false},
		{"negative", []int{-1}, false},
		{"above_range", []int{65536}, false},
		{"duplicate", []int{443, 443}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := ValidateJailPorts(c.ports)
			if (err == nil) != c.ok {
				t.Errorf("ValidateJailPorts(%v) = %v, want ok=%v", c.ports, err, c.ok)
			}
		})
	}
}
