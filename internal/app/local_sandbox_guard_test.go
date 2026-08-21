package app

import (
	"errors"
	"strings"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/agentproc/localsandbox"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// TestCheckLocalSandbox_OffIsAlwaysFine pins that an operator who opted out
// (or a mode where the knob does not apply) never pays for the probe — the
// guard must not be able to block a boot nobody asked to sandbox.
func TestCheckLocalSandbox_OffIsAlwaysFine(t *testing.T) {
	runmode.SetLocalSandboxForTest(t, false)
	if err := (&App{}).checkLocalSandbox(); err != nil {
		t.Fatalf("checkLocalSandbox with the sandbox off: %v", err)
	}
}

// TestLocalSandboxRefusal_NamesBothFixes pins the wording of the refusal.
// Both ways out have to be there, because one of them (install bubblewrap) is
// impossible on exactly the hosts where the other (opt out) is the only
// answer — a container that blocks nested user namespaces, say. A refusal
// that only names the install would strand those operators.
func TestLocalSandboxRefusal_NamesBothFixes(t *testing.T) {
	msg := localSandboxRefusal(errors.New("bwrap: setting up uid map: Permission denied")).Error()
	for _, want := range []string{
		"refusing to start",
		"bubblewrap",
		"apt install bubblewrap",
		"TF_LOCAL_SANDBOX=off",
		// The probe's own words, so the operator sees WHICH failure they hit.
		"setting up uid map",
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("refusal %q is missing %q", msg, want)
		}
	}
}

// TestCheckLocalSandbox_OnProbesThisHost exercises the gate itself against
// whatever this machine is: a working bubblewrap must boot, a broken or
// absent one must refuse. Either outcome is an assertion — the one thing that
// may never happen is a nil error alongside an unusable sandbox.
func TestCheckLocalSandbox_OnProbesThisHost(t *testing.T) {
	runmode.SetLocalSandboxForTest(t, true)

	err := (&App{}).checkLocalSandbox()
	probeErr := localsandbox.Probe()
	switch {
	case probeErr == nil && err != nil:
		t.Errorf("checkLocalSandbox refused a host where the probe passes: %v", err)
	case probeErr != nil && err == nil:
		t.Errorf("checkLocalSandbox booted with an unusable sandbox (probe: %v)", probeErr)
	}
}
