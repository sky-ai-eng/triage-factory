package app

import (
	"errors"
	"fmt"
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

// TestLocalSandboxRefusal_NamesTheFixForTheFailureThatHappened is the
// regression for a real misdirection: the refusal used to carry both fixes
// with "install bubblewrap" first, so an operator who installed it and hit the
// namespace block a second time was told, again, to do the one thing that
// could not have helped.
//
// The two branches are asserted for what they must say AND what they must not.
func TestLocalSandboxRefusal_NamesTheFixForTheFailureThatHappened(t *testing.T) {
	cases := []struct {
		name    string
		err     error
		want    []string
		exclude []string
	}{
		{
			name: "not installed names the install",
			err:  fmt.Errorf("%w: not on PATH", localsandbox.ErrNotInstalled),
			want: []string{"refusing to start", "apt install bubblewrap", "TF_LOCAL_SANDBOX=off"},
		},
		{
			name: "namespace refused never says install it again",
			err:  fmt.Errorf("%w: bwrap: setting up uid map: Permission denied", localsandbox.ErrNamespaceRefused),
			want: []string{
				"re-installing will not help",
				"AppArmor",
				"systemctl reload apparmor",
				"TF_LOCAL_SANDBOX=off",
			},
			// The exact misdirection that produced this test. `apt install`
			// must not appear on the branch where the package is already
			// there, and root must not be suggested on either.
			exclude: []string{"apt install bubblewrap"},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			msg := localSandboxRefusal(c.err).Error()
			for _, want := range c.want {
				if !strings.Contains(msg, want) {
					t.Errorf("refusal is missing %q:\n%s", want, msg)
				}
			}
			for _, bad := range c.exclude {
				if strings.Contains(msg, bad) {
					t.Errorf("refusal wrongly suggests %q:\n%s", bad, msg)
				}
			}
		})
	}
}

// TestLocalSandboxRefusal_NeverSuggestsRoot pins the one remedy that must
// never appear on ANY branch. bubblewrap creating an unprivileged user
// namespace is the design; running the whole server as root to satisfy a host
// policy that refuses one would hand the agent far more than it takes away.
func TestLocalSandboxRefusal_NeverSuggestsRoot(t *testing.T) {
	for _, err := range []error{
		fmt.Errorf("%w: not on PATH", localsandbox.ErrNotInstalled),
		fmt.Errorf("%w: denied", localsandbox.ErrNamespaceRefused),
		errors.New("an unclassified probe failure"),
	} {
		msg := localSandboxRefusal(err).Error()
		for _, bad := range []string{"sudo ./triagefactory", "run as root", "as superuser", "sudo triagefactory"} {
			if strings.Contains(msg, bad) {
				t.Errorf("refusal suggests privilege escalation (%q):\n%s", bad, msg)
			}
		}
	}
}

// TestLocalSandboxRefusal_UnclassifiedStillCarriesTheOptOut pins the fallback
// arm: a probe failure we did not classify gets reported as-is rather than
// guessed at, but the opt-out is true of every failure and must survive.
func TestLocalSandboxRefusal_UnclassifiedStillCarriesTheOptOut(t *testing.T) {
	msg := localSandboxRefusal(errors.New("something new")).Error()
	if !strings.Contains(msg, "something new") || !strings.Contains(msg, "TF_LOCAL_SANDBOX=off") {
		t.Errorf("unclassified refusal lost the detail or the opt-out:\n%s", msg)
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
