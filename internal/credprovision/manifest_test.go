package credprovision

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/sky-ai-eng/triage-factory/cmd/exec/agenthost"
	"github.com/sky-ai-eng/triage-factory/internal/credbundle"
	"github.com/sky-ai-eng/triage-factory/internal/credseal"
	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/eventsource"
	"github.com/sky-ai-eng/triage-factory/internal/integrations"
	"github.com/sky-ai-eng/triage-factory/internal/jira"
	"github.com/sky-ai-eng/triage-factory/internal/linear"
	"github.com/sky-ai-eng/triage-factory/internal/upstream"
)

// countingJira resolves a Jira service credential and counts how often it was
// asked, so a test can prove a turned-off Jira is never resolved.
type countingJira struct {
	jira.Resolver
	calls int
}

func (f *countingJira) ResolveSystemCredential(context.Context, string) (jira.SystemCredential, error) {
	f.calls++
	return jira.SystemCredential{URL: "https://acme.atlassian.net", Deployment: jira.DeploymentCloud, Email: "bot@acme.com", APIToken: "tok"}, nil
}

// provisionRig is a whole provision over fake stores, opened the way the
// run's sidecar opens it.
type provisionRig struct {
	kp     *credseal.KeyPair
	claims *provisionClaimCredentials
	m      *Manager
}

func newProvisionRig(t *testing.T, avail eventsource.Availability, linearTokens *installTokens) *provisionRig {
	t.Helper()
	kp, err := credseal.GenerateKeyPair()
	if err != nil {
		t.Fatalf("GenerateKeyPair: %v", err)
	}
	claims := &provisionClaimCredentials{}
	secrets := &linearSecrets{values: map[string]string{integrations.KeyLinearAuthMethod: string(linear.AuthMethodAppInstall)}}
	return &provisionRig{kp: kp, claims: claims, m: &Manager{
		stores: db.Stores{
			Secrets: secrets,
			Orgs:    &fakeOrgs{},
			ConversationQueue: &provisionQueue{claim: db.AwaitingCredentialsConversation{
				ConversationID: "conv-1", OrgID: "org-1", ExecutorID: "exec-1", BootEpoch: 4,
				CredPubKey: base64.StdEncoding.EncodeToString(kp.Public[:]),
			}},
			Instances:        &provisionInstances{inst: &domain.Instance{ID: "exec-1", BootEpoch: 4}},
			Conversations:    provisionConversations{},
			ClaimCredentials: claims,
		},
		ghResolver:     &fakeScopedResolver{hasCred: true, base: "https://github.com"},
		jiraResolver:   &countingJira{},
		linearResolver: linear.NewResolverWithInstall(secrets, nil, linearOn, linearTokens),
		llm:            noLLM{},
		sources: func(context.Context, string) (eventsource.Availability, error) {
			return avail, nil
		},
	}}
}

func (r *provisionRig) provision(t *testing.T) *credbundle.Bundle {
	t.Helper()
	if err := r.m.ProvisionForConversation(context.Background(), "org-1", "conv-1"); err != nil {
		t.Fatalf("ProvisionForConversation: %v", err)
	}
	plaintext, err := r.kp.Open(r.claims.sealed)
	if err != nil {
		t.Fatalf("open the written bundle with the claim's key: %v", err)
	}
	bundle, err := credbundle.Unmarshal(plaintext)
	if err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	return bundle
}

// TestProvision_EachStateFromItsCause walks one provision through every state
// the brain writes and the cause of each: a resolved credential is sealed and
// in the bundle, an org without one is unconfigured, a turned-off source is
// disabled and never resolved, an unentitled source is unlicensed and its
// provider credential never resolved, a credential that failed to resolve is
// unresolved for its reason, and an unbuilt source is not named at all.
func TestProvision_EachStateFromItsCause(t *testing.T) {
	agenthost.ResetProviders()
	t.Cleanup(agenthost.ResetProviders)
	providerCalls := map[string]int{}
	for _, ns := range []string{"chat", "pager", "mail"} {
		agenthost.RegisterProviderCredential(ns, func(_ context.Context, _ db.Stores, _ agenthost.ProvisionScope) (json.RawMessage, error) {
			providerCalls[ns]++
			if ns == "pager" {
				return nil, nil
			}
			return json.RawMessage(`{"token":"` + ns + `"}`), nil
		})
	}

	avail := eventsource.Availability{
		eventsource.NewSource(eventsource.KindGitHub, eventsource.StateAvailable),
		eventsource.NewSource(eventsource.KindJira, eventsource.StateDisabled),
		eventsource.NewSource(eventsource.KindLinear, eventsource.StateAvailable),
		eventsource.NewSource("chat", eventsource.StateUnlicensed),
		eventsource.NewSource("pager", eventsource.StateAvailable),
		eventsource.NewSource("mail", eventsource.StateAvailable),
		eventsource.NewSource(eventsource.KindSchedule, eventsource.StateWIP),
	}
	down := &upstream.TransportError{Err: errors.New("dial tcp: connection refused")}
	rig := newProvisionRig(t, avail, &installTokens{err: down})
	bundle := rig.provision(t)

	want := domain.SourceManifest{
		eventsource.KindGitHub: {State: domain.SourceSealed},
		eventsource.KindJira:   {State: domain.SourceDisabled},
		eventsource.KindLinear: {State: domain.SourceUnresolved, Reason: domain.SourceReasonTokenEndpointUnavailable},
		"chat":                 {State: domain.SourceUnlicensed},
		"pager":                {State: domain.SourceUnconfigured},
		"mail":                 {State: domain.SourceSealed},
	}
	if len(bundle.Sources) != len(want) {
		t.Errorf("manifest = %+v, want exactly %+v", bundle.Sources, want)
	}
	for kind, e := range want {
		if got := bundle.Sources[kind]; got.State != e.State || got.Reason != e.Reason {
			t.Errorf("%s = %+v, want %+v", kind, got, e)
		}
	}

	if bundle.GitHub == nil {
		t.Error("a sealed GitHub left no GitHub half in the bundle")
	}
	if bundle.Jira != nil || rig.m.jiraResolver.(*countingJira).calls != 0 {
		t.Errorf("a turned-off Jira was resolved (calls %d) or sealed (%+v)", rig.m.jiraResolver.(*countingJira).calls, bundle.Jira)
	}
	if bundle.Linear != nil {
		t.Errorf("an unresolved Linear sealed %+v", bundle.Linear)
	}
	if providerCalls["chat"] != 0 {
		t.Error("an unlicensed source's provider credential was resolved")
	}
	if _, ok := bundle.ProviderCreds("chat"); ok {
		t.Error("an unlicensed source's provider credential was sealed")
	}
	if _, ok := bundle.ProviderCreds("mail"); !ok {
		t.Error("an available source's provider credential was not sealed")
	}
	if rig.claims.sources["linear"] != bundle.Sources["linear"] || len(rig.claims.sources) != len(bundle.Sources) {
		t.Errorf("the claims-row copy %+v differs from the sealed copy %+v", rig.claims.sources, bundle.Sources)
	}
}

// TestProvision_ReSealMovesUnresolvedToSealed pins recovery: the provision the
// sweep re-runs for an unresolved engagement seals the credential once it
// resolves, with its expiry, and replaces the claim's manifest with it.
func TestProvision_ReSealMovesUnresolvedToSealed(t *testing.T) {
	tokens := &installTokens{err: &upstream.TransportError{Err: errors.New("timeout")}}
	rig := newProvisionRig(t, eventsource.Availability{
		eventsource.NewSource(eventsource.KindGitHub, eventsource.StateAvailable),
		eventsource.NewSource(eventsource.KindLinear, eventsource.StateAvailable),
	}, tokens)

	if got := rig.provision(t).Sources[eventsource.KindLinear]; got.State != domain.SourceUnresolved {
		t.Fatalf("first seal: linear = %+v, want unresolved", got)
	}

	expires := time.Now().Add(24 * time.Hour).Truncate(time.Second)
	tokens.err, tokens.token, tokens.expiresAt = nil, "lin_oauth_recovered", expires
	bundle := rig.provision(t)
	got := bundle.Sources[eventsource.KindLinear]
	if got.State != domain.SourceSealed || got.ExpiresAt == nil || !got.ExpiresAt.Equal(expires) {
		t.Fatalf("re-seal: linear = %+v, want sealed expiring at %s", got, expires)
	}
	if bundle.Linear == nil || bundle.Linear.AccessToken != "lin_oauth_recovered" {
		t.Errorf("re-seal: Linear = %+v, want the recovered token", bundle.Linear)
	}
	if rig.claims.puts != 2 || rig.claims.sources[eventsource.KindLinear].State != domain.SourceSealed {
		t.Errorf("claims row after re-seal = %+v (%d puts), want linear sealed", rig.claims.sources, rig.claims.puts)
	}
}

// turnedOffJira is a Jira resolver whose gate refuses, as it does for an org
// whose admin turned Jira off.
type turnedOffJira struct{ jira.Resolver }

func (turnedOffJira) ResolveSystemCredential(context.Context, string) (jira.SystemCredential, error) {
	return jira.SystemCredential{}, fmt.Errorf("Jira for org org-1: %w", eventsource.ErrDisabled)
}

// TestProvision_SourceTurnedOffAfterTheAvailabilityRead covers the seal that
// races the switch: availability still read the sources as on, and their
// resolvers' own gates refused. The seal carries on, holding neither
// credential, and records both sources as what they now are rather than as
// unresolved or as a failure.
func TestProvision_SourceTurnedOffAfterTheAvailabilityRead(t *testing.T) {
	tokens := &installTokens{token: "lin_oauth"}
	rig := newProvisionRig(t, eventsource.Availability{
		eventsource.NewSource(eventsource.KindGitHub, eventsource.StateAvailable),
		eventsource.NewSource(eventsource.KindJira, eventsource.StateAvailable),
		eventsource.NewSource(eventsource.KindLinear, eventsource.StateAvailable),
	}, tokens)
	rig.m.jiraResolver = turnedOffJira{}
	linearOff := func(context.Context, string) error {
		return fmt.Errorf("Linear for org org-1: %w", eventsource.ErrDisabled)
	}
	rig.m.linearResolver = linear.NewResolverWithInstall(rig.m.stores.Secrets, nil, linearOff, tokens)

	bundle := rig.provision(t)
	for _, kind := range []string{eventsource.KindJira, eventsource.KindLinear} {
		if got := bundle.Sources[kind]; got.State != domain.SourceDisabled {
			t.Errorf("%s = %+v, want disabled", kind, got)
		}
	}
	if bundle.Jira != nil || bundle.Linear != nil {
		t.Errorf("bundle holds Jira %+v and Linear %+v, want neither", bundle.Jira, bundle.Linear)
	}
	if got := bundle.Sources[eventsource.KindGitHub]; got.State != domain.SourceSealed {
		t.Errorf("github = %+v, want sealed", got)
	}
}

// TestProvision_AvailabilityFailureWritesNothing pins that a manifest is never
// guessed: a failed availability resolve fails the provision, and no bundle
// is written whose manifest nothing could vouch for.
func TestProvision_AvailabilityFailureWritesNothing(t *testing.T) {
	rig := newProvisionRig(t, nil, &installTokens{})
	boom := errors.New("org_event_sources unreachable")
	rig.m.sources = func(context.Context, string) (eventsource.Availability, error) { return nil, boom }
	if err := rig.m.ProvisionForConversation(context.Background(), "org-1", "conv-1"); !errors.Is(err, boom) {
		t.Fatalf("ProvisionForConversation = %v, want the availability failure", err)
	}
	if rig.claims.puts != 0 {
		t.Error("a bundle was written without an availability answer")
	}
}

// TestGithubEntry pins the GitHub entry's expiry: the earliest token sealed
// for it, and none for a PAT.
func TestGithubEntry(t *testing.T) {
	soon := time.Date(2026, 10, 11, 12, 30, 0, 0, time.UTC)
	later := soon.Add(30 * time.Minute)
	got := githubEntry(&credbundle.GitHubCreds{
		RepoTokens: map[string]credbundle.RepoToken{"a/b": {Token: "x", ExpiresAt: later}},
		CLIToken:   &credbundle.RepoToken{Token: "y", ExpiresAt: soon},
	})
	if got.State != domain.SourceSealed || got.ExpiresAt == nil || !got.ExpiresAt.Equal(soon) {
		t.Errorf("app entry = %+v, want sealed expiring at %s", got, soon)
	}
	pat := githubEntry(&credbundle.GitHubCreds{Mode: credbundle.GitHubModePAT, RepoTokens: map[string]credbundle.RepoToken{"a/b": {Token: "ghp"}}})
	if pat.State != domain.SourceSealed || pat.ExpiresAt != nil {
		t.Errorf("PAT entry = %+v, want sealed with no expiry", pat)
	}
	if none := githubEntry(nil); none.State != domain.SourceUnconfigured {
		t.Errorf("no GitHub half = %+v, want unconfigured", none)
	}
}

// sweepQueue records the refresh sweep's query and answers it with fixed
// conversations, each of which then reads as unclaimed so the provision it
// triggers ends there.
type sweepQueue struct {
	db.ConversationQueueStore
	olderThan, expiringBefore time.Time
	due                       []db.AwaitingCredentialsConversation
	provisioned               []string
}

func (f *sweepQueue) ListActiveNeedingCredentialRefresh(_ context.Context, olderThan, expiringBefore time.Time) ([]db.AwaitingCredentialsConversation, error) {
	f.olderThan, f.expiringBefore = olderThan, expiringBefore
	return f.due, nil
}

func (f *sweepQueue) GetClaim(_ context.Context, _, conversationID string) (db.AwaitingCredentialsConversation, bool, error) {
	f.provisioned = append(f.provisioned, conversationID)
	return db.AwaitingCredentialsConversation{}, false, nil
}

// TestSweepRefresh_ReSealsWhatTheQueueNames pins the sweep's half of the
// early re-seal: it asks for bundles past the age cutoff and for credentials
// expiring before the next tick could reach them, and re-provisions every
// engagement the query names — the unresolved and the near-expiry alike.
func TestSweepRefresh_ReSealsWhatTheQueueNames(t *testing.T) {
	q := &sweepQueue{due: []db.AwaitingCredentialsConversation{
		{ConversationID: "conv-unresolved", OrgID: "org-1"},
		{ConversationID: "conv-expiring", OrgID: "org-1"},
	}}
	m := &Manager{stores: db.Stores{ConversationQueue: q}}

	before := time.Now()
	sweepRefresh(context.Background(), m, 5*time.Minute, 30*time.Minute)

	if d := before.Add(-30 * time.Minute).Sub(q.olderThan); d > time.Second || d < -time.Second {
		t.Errorf("olderThan = %s, want about refreshAfter ago", q.olderThan)
	}
	horizon := before.Add(5*time.Minute + expiryMargin)
	if d := horizon.Sub(q.expiringBefore); d > time.Second || d < -time.Second {
		t.Errorf("expiringBefore = %s, want about one interval plus the margin ahead (%s)", q.expiringBefore, horizon)
	}
	if len(q.provisioned) != 2 || q.provisioned[0] != "conv-unresolved" || q.provisioned[1] != "conv-expiring" {
		t.Errorf("provisioned %v, want both named engagements", q.provisioned)
	}
}
