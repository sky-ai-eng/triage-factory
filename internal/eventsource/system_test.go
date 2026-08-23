package eventsource_test

import (
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/auth"
	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/eventsource"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// availableKindsSystem runs the system resolve against the rig's stores — no
// claims transaction, which is the whole point of the door under test.
func (r *rig) availableKindsSystem(t *testing.T) []string {
	t.Helper()
	kinds, err := eventsource.AvailableKindsSystem(t.Context(), r.stores, runmode.LocalDefaultOrgID)
	if err != nil {
		t.Fatalf("AvailableKindsSystem: %v", err)
	}
	return kinds
}

// TestAvailableKindsSystem_AgreesWithTheClaimsResolve pins the property the
// include_tools stamp rests on: for core's two sources, the claims-free door
// answers exactly what AvailableKinds answers under claims — same derivation,
// different pool.
func TestAvailableKindsSystem_AgreesWithTheClaimsResolve(t *testing.T) {
	r := newRig(t)
	r.saveCreds(t, auth.Credentials{
		GitHubPAT: "ghp_test",
		JiraURL:   "https://jira.example.com", JiraPAT: "jira-pat",
	})

	var claims []string
	r.withTx(t, func(tx db.TxStores) error {
		var e error
		claims, e = eventsource.AvailableKinds(t.Context(), tx, runmode.LocalDefaultOrgID)
		return e
	})
	system := r.availableKindsSystem(t)

	if !slices.Equal(system, claims) {
		t.Errorf("AvailableKindsSystem = %v, AvailableKinds = %v; the two doors must agree", system, claims)
	}
	if !slices.Contains(system, eventsource.KindGitHub) || !slices.Contains(system, eventsource.KindJira) {
		t.Errorf("AvailableKindsSystem = %v, want both configured core sources", system)
	}
}

// TestAvailableKindsSystem_PauseExcludes pins that the admin's off switch
// reaches the system door: a configured-but-paused source is not available,
// so it is never stamped into a claim's tools manifest.
func TestAvailableKindsSystem_PauseExcludes(t *testing.T) {
	r := newRig(t)
	r.saveCreds(t, auth.Credentials{
		GitHubPAT: "ghp_test",
		JiraURL:   "https://jira.example.com", JiraPAT: "jira-pat",
	})
	r.pause(t, eventsource.KindJira)

	got := r.availableKindsSystem(t)
	if slices.Contains(got, eventsource.KindJira) {
		t.Errorf("AvailableKindsSystem = %v, want jira excluded while paused", got)
	}
	if !slices.Contains(got, eventsource.KindGitHub) {
		t.Errorf("AvailableKindsSystem = %v, want github unaffected by jira's pause", got)
	}
}

// TestAvailableKindsSystem_RegisteredSources pins the registration seam's two
// arms: a registered source with a SystemProbe answers through it, and one
// without a SystemProbe is absent from the answer even when its claims Probe
// says available — never advertised on a probe nobody wrote.
func TestAvailableKindsSystem_RegisteredSources(t *testing.T) {
	r := newRig(t)
	available := func(context.Context, db.TxStores, string) (eventsource.State, error) {
		return eventsource.StateAvailable, nil
	}
	eventsource.Register("chat", eventsource.Registration{
		Probe: available,
		SystemProbe: func(context.Context, db.Stores, string) (eventsource.State, error) {
			return eventsource.StateAvailable, nil
		},
	})
	eventsource.Register("zulu", eventsource.Registration{Probe: available})

	got := r.availableKindsSystem(t)
	if !slices.Contains(got, "chat") {
		t.Errorf("AvailableKindsSystem = %v, want the system-probed registered source included", got)
	}
	if slices.Contains(got, "zulu") {
		t.Errorf("AvailableKindsSystem = %v, want the source with no SystemProbe absent", got)
	}
}

// TestAvailableKindsSystem_RegisteredPauseExcludes pins that the pause policy
// resolves in the system pass for registered sources too — a SystemProbe never
// has to check it itself, mirroring the claims resolver's contract.
func TestAvailableKindsSystem_RegisteredPauseExcludes(t *testing.T) {
	r := newRig(t)
	eventsource.Register("chat", eventsource.Registration{
		Probe: func(context.Context, db.TxStores, string) (eventsource.State, error) {
			return eventsource.StateAvailable, nil
		},
		SystemProbe: func(context.Context, db.Stores, string) (eventsource.State, error) {
			return eventsource.StateAvailable, nil
		},
	})
	r.pause(t, "chat")

	if got := r.availableKindsSystem(t); slices.Contains(got, "chat") {
		t.Errorf("AvailableKindsSystem = %v, want the paused registered source excluded", got)
	}
}

// TestAvailableKindsSystem_ProbeFailureFailsTheRead pins the package's error
// contract on the system door: a probe that cannot answer fails the whole
// read rather than shrinking the answer — the caller decides how to degrade.
func TestAvailableKindsSystem_ProbeFailureFailsTheRead(t *testing.T) {
	r := newRig(t)
	boom := errors.New("workspace store unreachable")
	eventsource.Register("chat", eventsource.Registration{
		Probe: func(context.Context, db.TxStores, string) (eventsource.State, error) {
			return eventsource.StateAvailable, nil
		},
		SystemProbe: func(context.Context, db.Stores, string) (eventsource.State, error) {
			return "", boom
		},
	})

	_, err := eventsource.AvailableKindsSystem(t.Context(), r.stores, runmode.LocalDefaultOrgID)
	if !errors.Is(err, boom) {
		t.Fatalf("AvailableKindsSystem error = %v, want it to wrap the probe's failure", err)
	}
}
