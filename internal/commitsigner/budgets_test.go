package commitsigner

import (
	"strings"
	"testing"
)

func TestParseBudgets(t *testing.T) {
	for _, tc := range []struct {
		name    string
		env     map[string]string
		want    Budgets
		wantErr string
	}{
		{name: "unset is the defaults", want: Budgets{PerOrgMinute: 600, PerClaim: 5000}},
		{
			name: "blank is the defaults",
			env:  map[string]string{envMaxPerOrgMinute: " ", envMaxPerClaim: ""},
			want: Budgets{PerOrgMinute: 600, PerClaim: 5000},
		},
		{
			name: "both set",
			env:  map[string]string{envMaxPerOrgMinute: "60", envMaxPerClaim: " 300 "},
			want: Budgets{PerOrgMinute: 60, PerClaim: 300},
		},
		{name: "zero is refused", env: map[string]string{envMaxPerOrgMinute: "0"}, wantErr: envMaxPerOrgMinute},
		{name: "negative is refused", env: map[string]string{envMaxPerClaim: "-5"}, wantErr: envMaxPerClaim},
		{name: "a non-number is refused", env: map[string]string{envMaxPerClaim: "lots"}, wantErr: envMaxPerClaim},
		{name: "a unit is refused", env: map[string]string{envMaxPerOrgMinute: "600/min"}, wantErr: envMaxPerOrgMinute},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ParseBudgets(func(k string) string { return tc.env[k] })
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("ParseBudgets = %+v, %v; want an error naming %s", got, err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseBudgets: %v", err)
			}
			if got != tc.want {
				t.Errorf("ParseBudgets = %+v, want %+v", got, tc.want)
			}
		})
	}
}
