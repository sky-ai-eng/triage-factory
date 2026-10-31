package agenthost

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/cred/credbundle"
	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/eventing/eventsource"
	"github.com/sky-ai-eng/triage-factory/internal/proxy/credmiss"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// heldBundleStub is a sidecar's held bundle, as the relay runtime reads it.
type heldBundleStub struct {
	providers map[string]json.RawMessage
	sources   domain.SourceManifest
}

func (h heldBundleStub) ProviderCreds(namespace string) (json.RawMessage, bool) {
	raw, ok := h.providers[namespace]
	return raw, ok
}

func (h heldBundleStub) Sources() domain.SourceManifest { return h.sources }

// noSecrets is an org secret scope holding nothing.
type noSecrets struct{ db.SecretStore }

func (noSecrets) GetSystem(context.Context, string, string) (string, error) { return "", nil }

// manifestRuntime is a direct runtime whose manifest is fixed, the way a
// sidecar's is: what a refusal reads its reason from.
type manifestRuntime struct {
	*directRuntime
	manifest domain.SourceManifest
}

func (r manifestRuntime) SourceManifest(context.Context) (domain.SourceManifest, error) {
	return r.manifest, nil
}

func withManifest(lc *LocalClient, m domain.SourceManifest) *LocalClient {
	lc.rt = manifestRuntime{directRuntime: lc.rt.(*directRuntime), manifest: m}
	return lc
}

// TestRelayRuntime_SourceManifestNeverRelays pins the sidecar placement: the
// manifest comes from the bundle the brain sealed to this run, read
// in-process. recordingRelayConn fails the test on any relay.
func TestRelayRuntime_SourceManifestNeverRelays(t *testing.T) {
	m := domain.SourceManifest{
		"github": {State: domain.SourceSealed},
		"linear": {State: domain.SourceUnresolved, Reason: domain.SourceReasonTokenEndpointUnavailable},
	}
	rt := newRelayRuntime(recordingRelayConn{t: t}, ConversationInfo{OrgID: "org", ConversationID: "conv"}, heldBundleStub{sources: m})
	got, err := rt.SourceManifest(context.Background())
	if err != nil {
		t.Fatalf("SourceManifest: %v", err)
	}
	if got["linear"] != m["linear"] || got["github"] != m["github"] {
		t.Errorf("SourceManifest = %+v, want the held bundle's %+v", got, m)
	}

	for name, held := range map[string]HeldBundle{"no view": nil, "no bundle yet": heldBundleStub{}} {
		rt := newRelayRuntime(recordingRelayConn{t: t}, ConversationInfo{OrgID: "org", ConversationID: "conv"}, held)
		if _, err := rt.SourceManifest(context.Background()); err == nil {
			t.Errorf("%s: SourceManifest succeeded, want an error the caller degrades on", name)
		}
	}
}

// TestDirectRuntime_SourceManifest_LiveResolve pins local mode's manifest: a
// live availability resolve, with an available source recorded as sealed and
// every other state carried through. The available source is a registered one,
// so the test needs no credential in the local secret backend.
func TestDirectRuntime_SourceManifest_LiveResolve(t *testing.T) {
	stores, info := newCaptureStores(t, false)
	// An org with no stored credential at all, without depending on which
	// local secret backend this process resolved.
	stores.Secrets = noSecrets{}
	t.Cleanup(eventsource.Reset)
	eventsource.Register("chat", eventsource.Registration{
		Probe: func(context.Context, db.TxStores, string) (eventsource.State, error) {
			return eventsource.StateAvailable, nil
		},
		SystemProbe: func(context.Context, db.Stores, string) (eventsource.State, error) {
			return eventsource.StateAvailable, nil
		},
	})
	if _, err := stores.OrgEventSources.SetDisabled(context.Background(), runmode.LocalDefaultOrgID, eventsource.KindLinear, true, runmode.LocalDefaultUserID); err != nil {
		t.Fatalf("pause linear: %v", err)
	}

	got, err := newDirectRuntime(stores, info).SourceManifest(context.Background())
	if err != nil {
		t.Fatalf("SourceManifest: %v", err)
	}
	for kind, want := range map[string]domain.SourceState{
		"chat":                 domain.SourceSealed,
		eventsource.KindJira:   domain.SourceUnconfigured,
		eventsource.KindLinear: domain.SourceDisabled,
	} {
		if e, ok := got.Entry(kind); !ok || e.State != want {
			t.Errorf("%s = %+v (present %v), want %s", kind, e, ok, want)
		}
	}
	if _, ok := got.Entry(eventsource.KindSchedule); ok {
		t.Error("the manifest names schedule, a source TF has not built")
	}

	if _, err := newDirectRuntime(db.Stores{}, info).SourceManifest(context.Background()); err == nil {
		t.Error("SourceManifest over no stores succeeded, want an error the caller degrades on")
	}
}

// TestGuidanceFor_PrefersTheMostSpecificRow pins the lookup order: a
// source-specific row over a general one, a reason-specific row over its
// state's, and the after-start reason over the state it was refused in.
func TestGuidanceFor_PrefersTheMostSpecificRow(t *testing.T) {
	type row struct {
		kind   string
		state  domain.SourceState
		reason string
	}
	for _, tc := range []struct {
		name string
		in   row
		want row
	}{
		{"a source-specific row", row{"linear", domain.SourceSealed, reasonCredentialRefused},
			row{"linear", domain.SourceSealed, reasonCredentialRefused}},
		{"the general row", row{"jira", domain.SourceSealed, reasonCredentialRefused},
			row{"", domain.SourceSealed, reasonCredentialRefused}},
		{"the reason's row", row{"linear", domain.SourceUnresolved, domain.SourceReasonTokenEndpointUnavailable},
			row{"", domain.SourceUnresolved, domain.SourceReasonTokenEndpointUnavailable}},
		{"an unknown reason falls to the state's row", row{"linear", domain.SourceUnresolved, "some_future_reason"},
			row{"", domain.SourceUnresolved, ""}},
		{"after-start over sealed", row{"jira", domain.SourceSealed, reasonAfterStart},
			row{"", "", reasonAfterStart}},
		{"after-start over unresolved", row{"jira", domain.SourceUnresolved, reasonAfterStart},
			row{"", "", reasonAfterStart}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g, ok := guidanceFor(tc.in.kind, tc.in.state, tc.in.reason)
			if got := (row{g.kind, g.state, g.reason}); !ok || got != tc.want {
				t.Errorf("guidanceFor(%+v) = %+v (ok %v), want %+v", tc.in, got, ok, tc.want)
			}
		})
	}
	if g, ok := guidanceFor("jira", domain.SourceSealed, ""); ok {
		t.Errorf("a sealed source with no refusal reason matched a row: %+v", g)
	}
}

// TestSourceRefusal_EveryStateIsExplained walks every state a manifest can
// record: each has a refusal and a note for the run's <run_context> (sealed
// excepted), and a disabled refusal is recognizable as one without matching
// prose.
func TestSourceRefusal_EveryStateIsExplained(t *testing.T) {
	for _, state := range []domain.SourceState{
		domain.SourceUnresolved, domain.SourceUnconfigured, domain.SourceDisabled,
		domain.SourceUnlicensed, domain.SourceWithheld,
	} {
		if g, ok := guidanceFor(eventsource.KindJira, state, ""); !ok || g.refusal == "" {
			t.Errorf("%s has no refusal row", state)
		}
		err := &SourceRefusal{Kind: eventsource.KindJira, State: state}
		note := SourceNote(eventsource.KindJira, domain.SourceEntry{State: state})
		if !strings.HasPrefix(note, eventsource.Label(eventsource.KindJira)+": ") {
			t.Errorf("%s note = %q, want one led by the source", state, note)
		}
		if got := errors.Is(err, eventsource.ErrDisabled); got != (state == domain.SourceDisabled) {
			t.Errorf("%s refusal wraps ErrDisabled = %v", state, got)
		}
	}
	if note := SourceNote(eventsource.KindGitHub, domain.SourceEntry{State: domain.SourceSealed}); note != "" {
		t.Errorf("sealed note = %q, want none", note)
	}
}

// TestSourceNotes pins the <run_context> lines: one per source that is not
// sealed, in the manifest's kind order.
func TestSourceNotes(t *testing.T) {
	m := domain.SourceManifest{
		"slack":  {State: domain.SourceUnconfigured},
		"github": {State: domain.SourceSealed},
		"linear": {State: domain.SourceUnresolved, Reason: domain.SourceReasonTokenEndpointUnavailable},
		"jira":   {State: domain.SourceDisabled},
	}
	var want []string
	for _, kind := range []string{"jira", "linear", "slack"} {
		want = append(want, SourceNote(kind, m[kind]))
	}
	if got := SourceNotes(m); !slices.Equal(got, want) {
		t.Errorf("SourceNotes = %q, want %q", got, want)
	}
}

// TestLiveSourceManifest pins the mapping local mode reads its manifest
// through: available is sealed, every other state carries over, and an
// unbuilt source is left out.
func TestLiveSourceManifest(t *testing.T) {
	got := LiveSourceManifest(eventsource.Availability{
		eventsource.NewSource("github", eventsource.StateAvailable),
		eventsource.NewSource("jira", eventsource.StateUnconfigured),
		eventsource.NewSource("linear", eventsource.StateDisabled),
		eventsource.NewSource("slack", eventsource.StateUnlicensed),
		eventsource.NewSource("schedule", eventsource.StateWIP),
	})
	want := domain.SourceManifest{
		"github": {State: domain.SourceSealed},
		"jira":   {State: domain.SourceUnconfigured},
		"linear": {State: domain.SourceDisabled},
		"slack":  {State: domain.SourceUnlicensed},
	}
	if len(got) != len(want) {
		t.Fatalf("LiveSourceManifest = %+v, want %+v", got, want)
	}
	for k, e := range want {
		if got[k] != e {
			t.Errorf("%s = %+v, want %+v", k, got[k], e)
		}
	}
}

// TestUnresolvedReason pins the two reasons an unresolved entry records.
func TestUnresolvedReason(t *testing.T) {
	if got := UnresolvedReason(errors.New("read secret: connection refused")); got != domain.SourceReasonResolveFailed {
		t.Errorf("UnresolvedReason(other) = %q, want %q", got, domain.SourceReasonResolveFailed)
	}
}

// TestSourceVerbs_ProxyMissReadsTheManifestReason pins a refusal on the
// sidecar's proxy path: the proxy names the state, the manifest supplies the
// reason when its entry is still in that state, and a run started with no
// proxy for a source is refused from its manifest without calling anything.
func TestSourceVerbs_ProxyMissReadsTheManifestReason(t *testing.T) {
	shortLinearBackoff(t)
	info := ConversationInfo{OrgID: runmode.LocalDefaultOrgID, ConversationID: "conv-manifest"}
	ctx := context.Background()

	miss := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		credmiss.NewResponder("apiproxy-linear", "conv", slog.New(slog.DiscardHandler)).Respond(w,
			&credbundle.SourceMissError{Kind: eventsource.KindLinear, State: domain.SourceUnresolved})
	}))
	defer miss.Close()

	t.Run("reason from the manifest", func(t *testing.T) {
		lc := withManifest(NewLocal(db.Stores{}, info), domain.SourceManifest{
			"linear": {State: domain.SourceUnresolved, Reason: domain.SourceReasonTokenEndpointUnavailable},
		})
		lc.proxyCreds = &ProxyCredentials{LinearAPIURL: miss.URL, LinearAPIToken: fakeLinearPlaceholer}
		_, err := lc.LinearGetIssue(ctx, "ENG-7")
		want := (&SourceRefusal{Kind: "linear", State: domain.SourceUnresolved, Reason: domain.SourceReasonTokenEndpointUnavailable}).Error()
		if err == nil || err.Error() != want {
			t.Fatalf("err = %v, want %q", err, want)
		}
	})

	t.Run("manifest moved on", func(t *testing.T) {
		lc := withManifest(NewLocal(db.Stores{}, info), domain.SourceManifest{"linear": {State: domain.SourceSealed}})
		lc.proxyCreds = &ProxyCredentials{LinearAPIURL: miss.URL, LinearAPIToken: fakeLinearPlaceholer}
		_, err := lc.LinearGetIssue(ctx, "ENG-7")
		want := (&SourceRefusal{Kind: "linear", State: domain.SourceUnresolved}).Error()
		if err == nil || err.Error() != want {
			t.Fatalf("err = %v, want the proxy's state alone, %q", err, want)
		}
	})

	t.Run("no proxy", func(t *testing.T) {
		jira := unreachable(t, "jira")
		for _, tc := range []struct {
			entry domain.SourceEntry
			want  *SourceRefusal
		}{
			{domain.SourceEntry{State: domain.SourceUnconfigured}, &SourceRefusal{Kind: "jira", State: domain.SourceUnconfigured}},
			{domain.SourceEntry{State: domain.SourceUnlicensed}, &SourceRefusal{Kind: "jira", State: domain.SourceUnlicensed}},
			{domain.SourceEntry{State: domain.SourceSealed}, &SourceRefusal{Kind: "jira", State: domain.SourceSealed, Reason: reasonAfterStart}},
		} {
			lc := withManifest(NewLocal(db.Stores{}, info), domain.SourceManifest{"jira": tc.entry})
			lc.proxyCreds = &ProxyCredentials{GitHubAPIURL: jira.URL}
			_, err := lc.JiraGetIssue(ctx, "PROJ-1")
			if err == nil || err.Error() != tc.want.Error() {
				t.Errorf("%s: err = %v, want %q", tc.entry.State, err, tc.want)
			}
		}
	})
}

// TestRelayRuntime_ProviderCredentialRefusesFromTheManifest pins the sidecar's
// answer when the held bundle carries no set for a provider: the refusal
// table's explanation of the provider's manifest entry, read in-process
// (recordingRelayConn fails the test on any relay), and unconfigured for a
// provider the manifest does not name.
func TestRelayRuntime_ProviderCredentialRefusesFromTheManifest(t *testing.T) {
	info := ConversationInfo{OrgID: "org", ConversationID: "conv"}
	for _, tc := range []struct {
		name string
		held HeldBundle
		want SourceRefusal
	}{
		{
			name: "unconfigured",
			held: heldBundleStub{sources: domain.SourceManifest{"slack": {State: domain.SourceUnconfigured}}},
			want: SourceRefusal{Kind: "slack", State: domain.SourceUnconfigured},
		},
		{
			name: "unresolved for a reason",
			held: heldBundleStub{sources: domain.SourceManifest{"slack": {State: domain.SourceUnresolved, Reason: domain.SourceReasonResolveFailed}}},
			want: SourceRefusal{Kind: "slack", State: domain.SourceUnresolved, Reason: domain.SourceReasonResolveFailed},
		},
		{
			name: "turned off",
			held: heldBundleStub{sources: domain.SourceManifest{"slack": {State: domain.SourceDisabled}}},
			want: SourceRefusal{Kind: "slack", State: domain.SourceDisabled},
		},
		{
			name: "not named",
			held: heldBundleStub{sources: domain.SourceManifest{"github": {State: domain.SourceSealed}}},
			want: SourceRefusal{Kind: "slack", State: domain.SourceUnconfigured},
		},
		{
			name: "no bundle view",
			held: nil,
			want: SourceRefusal{Kind: "slack", State: domain.SourceUnconfigured},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rt := newRelayRuntime(recordingRelayConn{t: t}, info, tc.held)
			_, err := rt.ProviderCredential(context.Background(), "slack")
			var got *SourceRefusal
			if !errors.As(err, &got) {
				t.Fatalf("ProviderCredential = %v, want a SourceRefusal", err)
			}
			if got.Kind != tc.want.Kind || got.State != tc.want.State || got.Reason != tc.want.Reason {
				t.Errorf("refusal = %+v, want %+v", *got, tc.want)
			}
			if err.Error() != tc.want.Error() {
				t.Errorf("refusal text = %q, want the table's %q", err, tc.want.Error())
			}
		})
	}
}

// TestDirectRuntime_ProviderCredentialRefusesFromTheTable pins local mode's
// answer, built at verb time the way Jira and Linear build theirs: a provider
// resolving no set is unconfigured, and one whose resolve fails is unresolved
// with the failure still reachable beneath the refusal.
func TestDirectRuntime_ProviderCredentialRefusesFromTheTable(t *testing.T) {
	ResetProviders()
	t.Cleanup(ResetProviders)
	locked := errors.New("keychain locked")
	RegisterProviderCredential("chat", func(context.Context, db.Stores, ProvisionScope) (json.RawMessage, error) {
		return nil, nil
	})
	RegisterProviderCredential("pager", func(context.Context, db.Stores, ProvisionScope) (json.RawMessage, error) {
		return nil, locked
	})
	rt := newDirectRuntime(db.Stores{}, ConversationInfo{OrgID: runmode.LocalDefaultOrgID, ConversationID: "conv-provider"})

	_, err := rt.ProviderCredential(context.Background(), "chat")
	var refusal *SourceRefusal
	if !errors.As(err, &refusal) || refusal.State != domain.SourceUnconfigured {
		t.Errorf("empty set = %v, want the unconfigured refusal", err)
	}

	_, err = rt.ProviderCredential(context.Background(), "pager")
	if !errors.As(err, &refusal) || refusal.State != domain.SourceUnresolved || refusal.Reason != domain.SourceReasonResolveFailed {
		t.Errorf("failed resolve = %v, want the unresolved refusal", err)
	}
	if !errors.Is(err, locked) {
		t.Errorf("the unresolved refusal %v does not carry the resolve failure beneath it", err)
	}
}
