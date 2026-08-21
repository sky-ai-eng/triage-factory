package eventsource_test

import (
	"context"
	"database/sql"
	"testing"

	"github.com/zalando/go-keyring"
	_ "modernc.org/sqlite"

	"github.com/sky-ai-eng/triage-factory/internal/auth"
	"github.com/sky-ai-eng/triage-factory/internal/db"
	sqlitestore "github.com/sky-ai-eng/triage-factory/internal/db/sqlite"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/eventsource"
	"github.com/sky-ai-eng/triage-factory/internal/integrations"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// rig is an in-memory local deployment: the schema, a mock keychain, and the
// stores every probe reads through. Registrations are cleared on cleanup so a
// synthetic source never leaks into a sibling test.
type rig struct{ stores db.Stores }

func newRig(t *testing.T) *rig {
	t.Helper()
	keyring.MockInit()
	conn, err := sql.Open("sqlite", db.TestDSNMemory)
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	conn.SetMaxOpenConns(1)
	conn.SetMaxIdleConns(1)
	t.Cleanup(func() { conn.Close() })
	if err := db.BootstrapSchemaForTest(conn); err != nil {
		t.Fatalf("bootstrap schema: %v", err)
	}
	t.Cleanup(eventsource.Reset)
	return &rig{stores: sqlitestore.New(conn)}
}

func (r *rig) withTx(t *testing.T, fn func(tx db.TxStores) error) {
	t.Helper()
	if err := r.stores.Tx.WithTx(t.Context(), runmode.LocalDefaultOrgID, runmode.LocalDefaultUserID, fn); err != nil {
		t.Fatalf("tx: %v", err)
	}
}

func (r *rig) resolve(t *testing.T) eventsource.Availability {
	t.Helper()
	var out eventsource.Availability
	r.withTx(t, func(tx db.TxStores) error {
		var e error
		out, e = eventsource.Resolve(t.Context(), tx, runmode.LocalDefaultOrgID)
		return e
	})
	return out
}

func (r *rig) saveCreds(t *testing.T, creds auth.Credentials) {
	t.Helper()
	if err := integrations.Save(t.Context(), r.stores.Secrets, runmode.LocalDefaultOrgID, creds); err != nil {
		t.Fatalf("save credentials: %v", err)
	}
}

// registerApp binds the org to a GitHub App of the given activation, the
// credential class included — the class is what decides whether an App counts
// at all, so a test that only wrote the row would be testing nothing.
func (r *rig) registerApp(t *testing.T, active bool) {
	t.Helper()
	if _, err := r.stores.Orgs.SetGitHubCredentialClass(t.Context(), runmode.LocalDefaultOrgID, domain.GitHubCredentialClassBYOApp); err != nil {
		t.Fatalf("set credential class: %v", err)
	}
	if _, err := r.stores.GitHubApps.CreateForOrg(t.Context(), domain.OrgGitHubApp{
		OrgID:    runmode.LocalDefaultOrgID,
		AppID:    "123",
		Slug:     "acme-tf",
		ClientID: "Iv1.deadbeef",
		Active:   active,
	}); err != nil {
		t.Fatalf("register app: %v", err)
	}
}

// pause records an org admin's disable for kind, the way the PATCH route does.
func (r *rig) pause(t *testing.T, kind string) {
	t.Helper()
	if _, err := r.stores.OrgEventSources.SetDisabled(
		t.Context(), runmode.LocalDefaultOrgID, kind, true, runmode.LocalDefaultUserID); err != nil {
		t.Fatalf("pause %s: %v", kind, err)
	}
}

// state pulls one kind's answer, failing the test when the kind is missing —
// an absent source and an unconfigured one are different answers and a test
// that conflates them proves nothing.
func state(t *testing.T, a eventsource.Availability, kind string) eventsource.State {
	t.Helper()
	st, ok := a.State(kind)
	if !ok {
		t.Fatalf("source %q absent from %v", kind, a)
	}
	return st
}

func kinds(a eventsource.Availability) []string {
	out := make([]string, 0, len(a))
	for _, s := range a {
		out = append(out, s.Kind)
	}
	return out
}

// TestResolve_StatePerSource walks the states each built-in source can report
// and the setup that produces them.
func TestResolve_StatePerSource(t *testing.T) {
	dcJira := auth.Credentials{JiraURL: "https://jira.example.com", JiraPAT: "jira-pat"}

	cases := map[string]struct {
		setup func(t *testing.T, r *rig)
		want  map[string]eventsource.State
	}{
		"nothing configured": {
			setup: func(*testing.T, *rig) {},
			want: map[string]eventsource.State{
				eventsource.KindGitHub:   eventsource.StateUnconfigured,
				eventsource.KindJira:     eventsource.StateUnconfigured,
				eventsource.KindLinear:   eventsource.StateWIP,
				eventsource.KindSchedule: eventsource.StateWIP,
			},
		},
		"github pat bound": {
			setup: func(t *testing.T, r *rig) {
				r.saveCreds(t, auth.Credentials{GitHubURL: "https://github.com", GitHubPAT: "ghp-x"})
			},
			want: map[string]eventsource.State{
				eventsource.KindGitHub: eventsource.StateAvailable,
				eventsource.KindJira:   eventsource.StateUnconfigured,
			},
		},
		"github app active, no pat": {
			setup: func(t *testing.T, r *rig) { r.registerApp(t, true) },
			want:  map[string]eventsource.State{eventsource.KindGitHub: eventsource.StateAvailable},
		},
		"github app discarded": {
			setup: func(t *testing.T, r *rig) { r.registerApp(t, false) },
			want:  map[string]eventsource.State{eventsource.KindGitHub: eventsource.StateUnconfigured},
		},
		"jira data center pat": {
			setup: func(t *testing.T, r *rig) { r.saveCreds(t, dcJira) },
			want: map[string]eventsource.State{
				eventsource.KindJira:   eventsource.StateAvailable,
				eventsource.KindGitHub: eventsource.StateUnconfigured,
			},
		},
		// A Cloud org authenticates with email + API token and has no PAT at
		// all, so a key-presence probe would report it unconfigured. The marker
		// is what makes this available.
		"jira cloud api token": {
			setup: func(t *testing.T, r *rig) {
				r.saveCreds(t, auth.Credentials{
					JiraURL:        "https://acme.atlassian.net",
					JiraEmail:      "bot@acme.example",
					JiraAPIToken:   "cloud-token",
					JiraAuthMethod: "cloud_api_token",
				})
			},
			want: map[string]eventsource.State{eventsource.KindJira: eventsource.StateAvailable},
		},
		"jira host with no credential": {
			setup: func(t *testing.T, r *rig) {
				r.saveCreds(t, auth.Credentials{JiraURL: "https://jira.example.com"})
			},
			want: map[string]eventsource.State{eventsource.KindJira: eventsource.StateUnconfigured},
		},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			r := newRig(t)
			tc.setup(t, r)
			got := r.resolve(t)
			for kind, want := range tc.want {
				if st := state(t, got, kind); st != want {
					t.Errorf("%s = %q, want %q", kind, st, want)
				}
			}
		})
	}
}

// TestResolve_Order pins the wire order: the sources TF has built first, then
// the ones it has only declared, each group by kind. A registered source sorts
// into the built group rather than onto the end.
func TestResolve_Order(t *testing.T) {
	r := newRig(t)
	eventsource.Register("zulu", eventsource.Registration{
		Probe: func(context.Context, db.TxStores, string) (eventsource.State, error) {
			return eventsource.StateUnlicensed, nil
		},
	})

	got := kinds(r.resolve(t))
	want := []string{eventsource.KindGitHub, eventsource.KindJira, "zulu", eventsource.KindLinear, eventsource.KindSchedule}
	if len(got) != len(want) {
		t.Fatalf("kinds = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("kinds = %v, want %v", got, want)
		}
	}
}

// TestResolve_MultiOnlySourceOmittedInLocal pins D6: a source that cannot
// exist in local mode is left OUT of the read there rather than reported off.
// Reporting a state would be a claim about setup or licensing in a mode where
// the source structurally cannot run.
func TestResolve_MultiOnlySourceOmittedInLocal(t *testing.T) {
	for _, mode := range []runmode.Mode{runmode.ModeLocal, runmode.ModeMulti} {
		t.Run(string(mode), func(t *testing.T) {
			runmode.SetForTest(t, mode)
			r := newRig(t)
			eventsource.Register("chat", eventsource.Registration{
				MultiOnly: true,
				Probe: func(context.Context, db.TxStores, string) (eventsource.State, error) {
					return eventsource.StateUnconfigured, nil
				},
			})

			st, ok := r.resolve(t).State("chat")
			wantListed := mode == runmode.ModeMulti
			if ok != wantListed {
				t.Fatalf("chat listed = %v (state %q), want listed = %v", ok, st, wantListed)
			}
			if ok && st != eventsource.StateUnconfigured {
				t.Errorf("chat = %q, want %q", st, eventsource.StateUnconfigured)
			}
		})
	}
}

// TestCanProduce_UnknownKindMakesNoClaim: the flag explains a handler that
// cannot fire for a reason the reader can act on. A prefix the vocabulary does
// not carry — system:, a source this build omits — is not such a reason, so it
// must not read as unavailable.
func TestCanProduce_UnknownKindMakesNoClaim(t *testing.T) {
	a := eventsource.Availability{
		{Kind: eventsource.KindGitHub, State: eventsource.StateAvailable},
		{Kind: eventsource.KindJira, State: eventsource.StateUnconfigured},
		{Kind: eventsource.KindLinear, State: eventsource.StateWIP},
	}
	for kind, want := range map[string]bool{
		eventsource.KindGitHub: true,
		eventsource.KindJira:   false,
		eventsource.KindLinear: false,
		"system":               true,
		"slack":                true,
	} {
		if got := a.CanProduce(kind); got != want {
			t.Errorf("CanProduce(%q) = %v, want %v", kind, got, want)
		}
	}
}

// TestStateFor_ReadsOnlyItsOwnSource is the reason the create gate calls
// StateFor rather than Resolve: a source the caller is not asking about must
// not be able to fail (or slow) their write.
func TestStateFor_ReadsOnlyItsOwnSource(t *testing.T) {
	r := newRig(t)
	probed := false
	eventsource.Register("chat", eventsource.Registration{
		Probe: func(context.Context, db.TxStores, string) (eventsource.State, error) {
			probed = true
			return "", context.DeadlineExceeded
		},
	})

	var (
		st    eventsource.State
		known bool
	)
	r.withTx(t, func(tx db.TxStores) error {
		var e error
		st, known, e = eventsource.StateFor(t.Context(), tx, runmode.LocalDefaultOrgID, eventsource.KindGitHub)
		return e
	})
	if !known || st != eventsource.StateUnconfigured {
		t.Errorf("github = (%q, %v), want (unconfigured, true)", st, known)
	}
	if probed {
		t.Error("resolving github ran another source's probe")
	}
}

// TestStateFor_UnknownKind: a prefix outside the vocabulary is answered
// ok=false, never a state — which is what keeps the create gate from refusing
// a system: handler in the name of a source that does not exist.
func TestStateFor_UnknownKind(t *testing.T) {
	r := newRig(t)
	var known bool
	r.withTx(t, func(tx db.TxStores) error {
		var e error
		_, known, e = eventsource.StateFor(t.Context(), tx, runmode.LocalDefaultOrgID, "system")
		return e
	})
	if known {
		t.Error("system reported as a known source")
	}
}

func TestKindOf(t *testing.T) {
	for in, want := range map[string]string{
		"jira:issue:assigned": "jira",
		"github:pr:opened":    "github",
		"slack:message":       "slack",
		"system":              "system",
		"":                    "",
	} {
		if got := eventsource.KindOf(in); got != want {
			t.Errorf("KindOf(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestRegister_RejectsWiringBugs: every one of these is a mistake that would
// otherwise show up as a source silently answering for another.
func TestRegister_RejectsWiringBugs(t *testing.T) {
	ok := func(context.Context, db.TxStores, string) (eventsource.State, error) {
		return eventsource.StateAvailable, nil
	}
	cases := map[string]func(){
		"empty kind":     func() { eventsource.Register("", eventsource.Registration{Probe: ok}) },
		"core kind":      func() { eventsource.Register(eventsource.KindJira, eventsource.Registration{Probe: ok}) },
		"nil probe":      func() { eventsource.Register("chat", eventsource.Registration{}) },
		"double-install": func() { eventsource.Register("chat", eventsource.Registration{Probe: ok}) },
	}
	for name, register := range cases {
		t.Run(name, func(t *testing.T) {
			t.Cleanup(eventsource.Reset)
			if name == "double-install" {
				eventsource.Register("chat", eventsource.Registration{Probe: ok})
			}
			defer func() {
				if recover() == nil {
					t.Error("expected a panic")
				}
			}()
			register()
		})
	}
}

// TestResolve_DisabledPrecedence pins D2's ordering — least-fixable first:
//
//	wip > unlicensed > disabled > unconfigured > available
//
// The two arms that matter are the ones where a pause is NOT the answer.
// Telling a reader to bind a credential for a source an admin turned off sends
// them to do useless work, so `disabled` beats `unconfigured`; but re-enabling
// a source nobody is entitled to, or one TF has not built, would change
// nothing, so it loses to both of those.
func TestResolve_DisabledPrecedence(t *testing.T) {
	cases := map[string]struct {
		setup func(t *testing.T, r *rig)
		kind  string
		want  eventsource.State
	}{
		"disabled beats available": {
			setup: func(t *testing.T, r *rig) {
				r.saveCreds(t, auth.Credentials{JiraURL: "https://jira.example.com", JiraPAT: "jira-pat"})
				r.pause(t, eventsource.KindJira)
			},
			kind: eventsource.KindJira,
			want: eventsource.StateDisabled,
		},
		// The one source that outranks its own row. Required beats everything,
		// including a row somebody wrote by hand: the state feeds the UI and
		// the handler gates, so reporting github disabled would explain away
		// the whole product as an admin's deliberate choice.
		"required beats disabled": {
			setup: func(t *testing.T, r *rig) {
				r.saveCreds(t, auth.Credentials{GitHubURL: "https://github.com", GitHubPAT: "ghp-x"})
				r.pause(t, eventsource.KindGitHub)
			},
			kind: eventsource.KindGitHub,
			want: eventsource.StateAvailable,
		},
		"disabled beats unconfigured": {
			setup: func(t *testing.T, r *rig) { r.pause(t, eventsource.KindJira) },
			kind:  eventsource.KindJira,
			want:  eventsource.StateDisabled,
		},
		"unlicensed beats disabled": {
			setup: func(t *testing.T, r *rig) {
				eventsource.Register("gated", eventsource.Registration{
					Probe: func(context.Context, db.TxStores, string) (eventsource.State, error) {
						return eventsource.StateUnlicensed, nil
					},
				})
				r.pause(t, "gated")
			},
			kind: "gated",
			want: eventsource.StateUnlicensed,
		},
		"wip beats disabled": {
			setup: func(t *testing.T, r *rig) { r.pause(t, eventsource.KindLinear) },
			kind:  eventsource.KindLinear,
			want:  eventsource.StateWIP,
		},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			r := newRig(t)
			tc.setup(t, r)
			if st := state(t, r.resolve(t), tc.kind); st != tc.want {
				t.Errorf("%s = %q, want %q", tc.kind, st, tc.want)
			}
		})
	}
}

// TestResolve_DisabledIsPerSource pins that a pause names one source and not
// the org: disabling Jira must not darken GitHub, which is the whole reason
// the row is keyed (org, kind) rather than being an org-level switch.
func TestResolve_DisabledIsPerSource(t *testing.T) {
	r := newRig(t)
	r.saveCreds(t, auth.Credentials{
		GitHubURL: "https://github.com", GitHubPAT: "ghp-x",
		JiraURL: "https://jira.example.com", JiraPAT: "jira-pat",
	})
	r.pause(t, eventsource.KindJira)

	got := r.resolve(t)
	if st := state(t, got, eventsource.KindJira); st != eventsource.StateDisabled {
		t.Errorf("jira = %q, want %q", st, eventsource.StateDisabled)
	}
	if st := state(t, got, eventsource.KindGitHub); st != eventsource.StateAvailable {
		t.Errorf("github = %q, want %q", st, eventsource.StateAvailable)
	}
	if got.CanProduce(eventsource.KindJira) {
		t.Error("CanProduce(jira) = true, want false — a paused source can fire nothing")
	}
	if !got.CanProduce(eventsource.KindGitHub) {
		t.Error("CanProduce(github) = false, want true")
	}
}

// TestStateFor_ReadsThePause covers the create-time gate's single-source door:
// it asks about one source and must see the same pause the whole-vocabulary
// read does.
func TestStateFor_ReadsThePause(t *testing.T) {
	r := newRig(t)
	r.saveCreds(t, auth.Credentials{JiraURL: "https://jira.example.com", JiraPAT: "jira-pat"})
	r.pause(t, eventsource.KindJira)

	var (
		st    eventsource.State
		known bool
	)
	r.withTx(t, func(tx db.TxStores) error {
		var e error
		st, known, e = eventsource.StateFor(t.Context(), tx, runmode.LocalDefaultOrgID, eventsource.KindJira)
		return e
	})
	if !known || st != eventsource.StateDisabled {
		t.Errorf("StateFor(jira) = (%q, %v), want (%q, true)", st, known, eventsource.StateDisabled)
	}
}
