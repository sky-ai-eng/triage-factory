package app

import (
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// TestPoolMaxConnsForRole pins the per-role Postgres pool ceilings and the
// TF_DB_MAX_OPEN_CONNS override + floor.
func TestPoolMaxConnsForRole(t *testing.T) {
	cases := []struct {
		name string
		role runmode.DeployRole
		env  string // "" means unset
		want int
	}{
		{"control default", runmode.RoleControl, "", defaultPoolMaxConns},
		{"all default", runmode.RoleAll, "", defaultPoolMaxConns},
		{"executor default is much smaller", runmode.RoleExecutor, "", defaultExecutorPoolMaxConns},
		{"env override wins for executor", runmode.RoleExecutor, "40", 40},
		{"env override wins for control", runmode.RoleControl, "50", 50},
		{"one floors to the minimum", runmode.RoleExecutor, "1", minPoolMaxConns},
		{"zero floors to the minimum (not database/sql unlimited)", runmode.RoleExecutor, "0", minPoolMaxConns},
		{"negative is invalid, falls back to role default", runmode.RoleExecutor, "-5", defaultExecutorPoolMaxConns},
		{"non-numeric is invalid, falls back to role default", runmode.RoleExecutor, "banana", defaultExecutorPoolMaxConns},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.env == "" {
				t.Setenv("TF_DB_MAX_OPEN_CONNS", "")
			} else {
				t.Setenv("TF_DB_MAX_OPEN_CONNS", tc.env)
			}
			if got := poolMaxConnsForRole(tc.role); got != tc.want {
				t.Errorf("poolMaxConnsForRole(%s) = %d, want %d", tc.role, got, tc.want)
			}
		})
	}
	// Sanity: the executor default really is much smaller than the API tier's.
	if defaultExecutorPoolMaxConns >= defaultPoolMaxConns {
		t.Errorf("executor pool default %d should be much smaller than the API tier's %d",
			defaultExecutorPoolMaxConns, defaultPoolMaxConns)
	}
}
