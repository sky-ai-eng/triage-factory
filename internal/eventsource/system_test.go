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

// resolveSystem runs the system resolve against the rig's stores — no claims
// transaction, which is the whole point of the door under test.
func (r *rig) resolveSystem(t *testing.T) eventsource.Availability {
	t.Helper()
	avail, err := eventsource.ResolveSystem(t.Context(), r.stores, runmode.LocalDefaultOrgID)
	if err != nil {
		t.Fatalf("ResolveSystem: %v", err)
	}
	return avail
}

// availableKindsSystem is the system answer's available kinds, in wire order.
func (r *rig) availableKindsSystem(t *testing.T) []string {
	t.Helper()
	var kinds []string
	for _, src := range r.resolveSystem(t) {
		if src.State == eventsource.StateAvailable {
			kinds = append(kinds, src.Kind)
		}
	}
	return kinds
}

// TestResolveSystem_AgreesWithTheClaimsResolve pins the property an
// engagement's source manifest rests on: for core's built sources, the
// claims-free door answers exactly what AvailableKinds answers under claims —
// same derivation, different pool.
func TestResolveSystem_AgreesWithTheClaimsResolve(t *testing.T) {
	r := newRig(t)
	r.saveGitHubPAT(t)
	r.saveJira(t, "https://jira.example.com", auth.Credentials{JiraPAT: "jira-pat"})
	r.bindLinear(t, "api_key", "lin_api_x")

	var claims []string
	r.withTx(t, func(tx db.TxStores) error {
		var e error
		claims, e = eventsource.AvailableKinds(t.Context(), tx, runmode.LocalDefaultOrgID)
		return e
	})
	system := r.availableKindsSystem(t)

	if !slices.Equal(system, claims) {
		t.Errorf("ResolveSystem available kinds = %v, AvailableKinds = %v; the two doors must agree", system, claims)
	}
	for _, kind := range []string{eventsource.KindGitHub, eventsource.KindJira, eventsource.KindLinear} {
		if !slices.Contains(system, kind) {
			t.Errorf("ResolveSystem available kinds = %v, want every configured core source, %s included", system, kind)
		}
	}
}

// TestResolveSystem_LinearFollowsItsCredential pins the state a run's source
// manifest starts from for Linear: unconfigured until the org binds a key,
// available once it does, and disabled while an admin has the source paused.
func TestResolveSystem_LinearFollowsItsCredential(t *testing.T) {
	r := newRig(t)
	if got := r.availableKindsSystem(t); slices.Contains(got, eventsource.KindLinear) {
		t.Errorf("ResolveSystem available kinds = %v with no Linear credential, want linear absent", got)
	}
	r.bindLinear(t, "api_key", "lin_api_x")
	if got := r.availableKindsSystem(t); !slices.Contains(got, eventsource.KindLinear) {
		t.Errorf("ResolveSystem available kinds = %v with a Linear key bound, want linear present", got)
	}
	r.pause(t, eventsource.KindLinear)
	if got := r.availableKindsSystem(t); slices.Contains(got, eventsource.KindLinear) {
		t.Errorf("ResolveSystem available kinds = %v while linear is paused, want linear absent", got)
	}
	if st, _ := r.resolveSystem(t).State(eventsource.KindLinear); st != eventsource.StateDisabled {
		t.Errorf("linear state while paused = %q, want disabled", st)
	}
}

// TestResolveSystem_PauseExcludes pins that the admin's off switch
// reaches the system door: a configured-but-paused source is not available,
// so it never reaches a claim's source manifest as anything but disabled.
func TestResolveSystem_PauseExcludes(t *testing.T) {
	r := newRig(t)
	r.saveGitHubPAT(t)
	r.saveJira(t, "https://jira.example.com", auth.Credentials{JiraPAT: "jira-pat"})
	r.pause(t, eventsource.KindJira)

	got := r.availableKindsSystem(t)
	if slices.Contains(got, eventsource.KindJira) {
		t.Errorf("ResolveSystem available kinds = %v, want jira excluded while paused", got)
	}
	if !slices.Contains(got, eventsource.KindGitHub) {
		t.Errorf("ResolveSystem available kinds = %v, want github unaffected by jira's pause", got)
	}
}

// TestResolveSystem_RegisteredSources pins the registration seam's two
// arms: a registered source with a SystemProbe answers through it, and one
// without a SystemProbe is absent from the answer even when its claims Probe
// says available — never advertised on a probe nobody wrote.
func TestResolveSystem_RegisteredSources(t *testing.T) {
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

	if got := r.availableKindsSystem(t); !slices.Contains(got, "chat") {
		t.Errorf("ResolveSystem available kinds = %v, want the system-probed registered source included", got)
	}
	if _, ok := r.resolveSystem(t).State("zulu"); ok {
		t.Errorf("ResolveSystem answered for zulu, want the source with no SystemProbe absent")
	}
}

// TestResolveSystem_RegisteredPauseExcludes pins that the pause policy
// resolves in the system pass for registered sources too — a SystemProbe never
// has to check it itself, mirroring the claims resolver's contract.
func TestResolveSystem_RegisteredPauseExcludes(t *testing.T) {
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
		t.Errorf("ResolveSystem available kinds = %v, want the paused registered source excluded", got)
	}
}

// TestResolveSystem_ProbeFailureFailsTheRead pins the package's error
// contract on the system door: a probe that cannot answer fails the whole
// read rather than shrinking the answer — the caller decides how to degrade.
func TestResolveSystem_ProbeFailureFailsTheRead(t *testing.T) {
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

	_, err := eventsource.ResolveSystem(t.Context(), r.stores, runmode.LocalDefaultOrgID)
	if !errors.Is(err, boom) {
		t.Fatalf("ResolveSystem error = %v, want it to wrap the probe's failure", err)
	}
}
