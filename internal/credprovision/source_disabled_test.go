package credprovision

import (
	"context"
	"errors"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/eventsource"
	"github.com/sky-ai-eng/triage-factory/internal/githubapp"
	"github.com/sky-ai-eng/triage-factory/internal/jira"
)

// The agenthost gate cuts an agent's exec verbs, but it never sees the lane the
// jailed agent's own `gh` and `git` use — those reach their credential injector
// directly. This is where that lane closes: a turned-off source contributes no
// credential to the sealed bundle, so the sidecar starts no proxy for it and
// the sandbox is built with no channel to that source at all.

// fakeSourcePolicy answers the policy read with a fixed set, or a fixed error.
type fakeSourcePolicy struct {
	db.OrgEventSourceStore
	off []string
	err error
}

func (f fakeSourcePolicy) ListDisabledSystem(context.Context, string) ([]string, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.off, nil
}

// TestManager_resolveGitHub_TurnedOffSourceSealsNothing: nil rather than an
// error is the right shape, because nil is already what a Jira-only org's
// bundle carries — so a run whose GitHub is turned off takes an existing,
// tested path rather than a new failure mode.
func TestManager_resolveGitHub_TurnedOffSourceSealsNothing(t *testing.T) {
	res := &fakeScopedResolver{
		base: "https://ghe.example", hasCred: true,
		token: githubapp.Token{Value: "ghs_scoped"},
	}
	m := &Manager{
		stores: db.Stores{
			OrgEventSources: fakeSourcePolicy{off: []string{eventsource.KindGitHub}},
			TeamGitHubRepos: &fakeTeamRepos{tracked: map[string]bool{"acme/widgets": true}},
			Tasks:           &fakeTasks{task: &domain.Task{EntitySource: "github", EntitySourceID: "acme/widgets#42"}},
		},
		ghResolver: res,
	}

	gh, err := m.resolveGitHub(context.Background(), "org-1", "team-1", "task-1", "conv-1")
	if err != nil {
		t.Fatalf("resolveGitHub: %v", err)
	}
	if gh != nil {
		t.Errorf("resolveGitHub = %+v, want nil for a source an org admin turned off", gh)
	}
	// Nothing was minted. A token minted and then discarded is a token that
	// existed, and GitHub's audit log would show this org still minting
	// installation tokens after an admin turned it off.
	if len(res.calls) != 0 {
		t.Errorf("minted %d tokens for a turned-off source, want 0", len(res.calls))
	}
}

// TestManager_resolveGitHub_PolicyReadFailurePropagates pins fail-closed at the
// sealer. Sealing on the assumption that nobody turned the source off is the
// one outcome that cannot be walked back — the bundle is already inside a
// sidecar this process cannot reach into.
func TestManager_resolveGitHub_PolicyReadFailurePropagates(t *testing.T) {
	m := &Manager{
		stores:     db.Stores{OrgEventSources: fakeSourcePolicy{err: errors.New("boom")}},
		ghResolver: &fakeScopedResolver{hasCred: true},
	}
	if _, err := m.resolveGitHub(context.Background(), "org-1", "team-1", "task-1", "conv-1"); err == nil {
		t.Fatal("resolveGitHub succeeded on an unreadable policy, want an error that fails the provision")
	}
}

// TestManager_resolveJira_TurnedOffSourceSealsNothing is the Jira twin, and it
// covers both callers: the delegated provision and the curator turn share this
// resolver.
func TestManager_resolveJira_TurnedOffSourceSealsNothing(t *testing.T) {
	m := &Manager{stores: db.Stores{OrgEventSources: fakeSourcePolicy{off: []string{eventsource.KindJira}}}}

	jc, err := m.resolveJira(context.Background(), "org-1")
	if err != nil {
		t.Fatalf("resolveJira: %v", err)
	}
	if jc != nil {
		t.Errorf("resolveJira = %+v, want nil for a source an org admin turned off", jc)
	}
}

// fakeJiraSystemResolver answers ResolveSystemCredential with a live-looking
// Data Center credential, and records whether it was asked at all.
type fakeJiraSystemResolver struct {
	jira.Resolver
	calls int
}

func (f *fakeJiraSystemResolver) ResolveSystemCredential(context.Context, string) (jira.SystemCredential, error) {
	f.calls++
	return jira.SystemCredential{URL: "https://jira.example", Deployment: jira.DeploymentDataCenter, PAT: "pat"}, nil
}

// TestManager_resolveJira_DifferentSourceTurnedOff_StillResolves is the
// negative control: the switch names one source, so a resolver that would
// answer still answers.
func TestManager_resolveJira_DifferentSourceTurnedOff_StillResolves(t *testing.T) {
	res := &fakeJiraSystemResolver{}
	m := &Manager{
		stores:       db.Stores{OrgEventSources: fakeSourcePolicy{off: []string{eventsource.KindGitHub}}},
		jiraResolver: res,
	}

	jc, err := m.resolveJira(context.Background(), "org-1")
	if err != nil {
		t.Fatalf("turning GitHub off cost a Jira resolution: %v", err)
	}
	if jc == nil || jc.PAT != "pat" {
		t.Fatalf("resolveJira = %+v, want the resolved Data Center credential", jc)
	}
	if res.calls != 1 {
		t.Errorf("resolver asked %d times, want 1", res.calls)
	}
}

// TestManager_resolveJira_TurnedOffSourceNeverAsksTheResolver pairs with it:
// the refusal happens before the secret store is touched, so a turned-off
// source reads no credential rather than reading one and dropping it.
func TestManager_resolveJira_TurnedOffSourceNeverAsksTheResolver(t *testing.T) {
	res := &fakeJiraSystemResolver{}
	m := &Manager{
		stores:       db.Stores{OrgEventSources: fakeSourcePolicy{off: []string{eventsource.KindJira}}},
		jiraResolver: res,
	}

	jc, err := m.resolveJira(context.Background(), "org-1")
	if err != nil || jc != nil {
		t.Fatalf("resolveJira = %+v, %v; want nil, nil", jc, err)
	}
	if res.calls != 0 {
		t.Errorf("resolver asked %d times for a turned-off source, want 0", res.calls)
	}
}
