package domain

import "testing"

// TestMemoryRoleOutranks pins the precedence order the SQL CASE
// expressions in both store dialects mirror: primary > produced >
// touched. A garbage role ranks below every known role so it never
// outranks (and therefore never overwrites) a row already carrying a
// real classification.
func TestMemoryRoleOutranks(t *testing.T) {
	cases := []struct {
		candidate, current string
		want               bool
	}{
		{MemoryRoleProduced, MemoryRoleTouched, true},
		{MemoryRolePrimary, MemoryRoleProduced, true},
		{MemoryRolePrimary, MemoryRoleTouched, true},
		{MemoryRoleTouched, MemoryRoleProduced, false},
		{MemoryRoleTouched, MemoryRolePrimary, false},
		{MemoryRoleProduced, MemoryRolePrimary, false},
		{MemoryRoleTouched, MemoryRoleTouched, false},
		{MemoryRoleProduced, MemoryRoleProduced, false},
		{MemoryRolePrimary, MemoryRolePrimary, false},
		{MemoryRoleTouched, "garbage", true},
		{"garbage", MemoryRoleTouched, false},
	}
	for _, tc := range cases {
		if got := MemoryRoleOutranks(tc.candidate, tc.current); got != tc.want {
			t.Errorf("MemoryRoleOutranks(%q, %q) = %v, want %v", tc.candidate, tc.current, got, tc.want)
		}
	}
}
