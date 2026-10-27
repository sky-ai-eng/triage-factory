package operator

import (
	"strings"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

func TestCheckProcess(t *testing.T) {
	cases := []struct {
		name    string
		mode    runmode.Mode
		role    runmode.DeployRole
		wantErr string
	}{
		{"local", runmode.ModeLocal, runmode.RoleAll, "local mode has no deployment operators"},
		{"multi executor", runmode.ModeMulti, runmode.RoleExecutor, "managed from a control container"},
		{"multi control", runmode.ModeMulti, runmode.RoleControl, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			runmode.SetForTest(t, tc.mode)
			runmode.SetRoleForTest(t, tc.role)
			err := checkProcess()
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("checkProcess() = %v, want nil", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("checkProcess() = %v, want an error containing %q", err, tc.wantErr)
			}
		})
	}
}
