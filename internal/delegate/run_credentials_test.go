package delegate

import (
	"context"
	"errors"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/agentproc"
	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	ghclient "github.com/sky-ai-eng/triage-factory/internal/github"
)

// fakeResolver records ClientFor calls and returns a preconfigured client.
// Mirrors the poller package's fake so the SKY-389 seam is exercised
// through the real ghclient.Resolver interface.
type fakeResolver struct {
	calls  []resolverCall
	client *ghclient.Client
	err    error
}

type resolverCall struct {
	orgID  string
	target string
}

func (f *fakeResolver) ClientFor(ctx context.Context, orgID, target string) (*ghclient.Client, error) {
	f.calls = append(f.calls, resolverCall{orgID: orgID, target: target})
	return f.client, f.err
}

var _ ghclient.Resolver = (*fakeResolver)(nil)

// recordingSecrets is a SecretsReader test double whose identity we can
// assert flows through getRunSecrets unchanged.
type recordingSecrets struct{}

func (recordingSecrets) Get(ctx context.Context, orgID, key string) (string, error) {
	return "", nil
}

var _ agentproc.SecretsReader = recordingSecrets{}

// TestResolveRunCredentials_MultiSeam pins the SKY-389 contract: when the
// per-org seam is wired (the production path in both modes), a run's GitHub
// client resolves per (org, owner) through the resolver — NOT the
// process-global ghClient — and the org's default model comes from modelFor.
// A regression that reverted to the global client/model would fail this.
func TestResolveRunCredentials_MultiSeam(t *testing.T) {
	const orgID = "11111111-2222-3333-4444-555555555555"
	const owner = "acme"
	const teamID = "team-abc"

	wantClient := ghclient.NewClient("https://github.example.com", "tok-from-resolver")
	resolver := &fakeResolver{client: wantClient}
	secrets := recordingSecrets{}

	// Constructor client/model are deliberately distinct sentinels so a
	// pass can only come from the resolver/modelFor, never the fallback.
	s := NewSpawner(nil, db.Stores{}, ghclient.NewClient("https://fallback", "fallback-tok"), nil, "fallback-model", "")
	s.SetRunCredentialResolvers(resolver, secrets, func(_ context.Context, gotOrg, gotTeam string) string {
		if gotOrg != orgID {
			t.Errorf("modelFor got org %q; want %q", gotOrg, orgID)
		}
		// SKY-389 review #2: the task's team must reach modelFor so a
		// multi-team org resolves the run's own team model, not the org default.
		if gotTeam != teamID {
			t.Errorf("modelFor got team %q; want %q", gotTeam, teamID)
		}
		return "claude-resolved-model"
	})

	gotClient, gotModel := s.resolveRunCredentials(context.Background(), orgID, owner, teamID)

	if gotClient != wantClient {
		t.Errorf("resolveRunCredentials returned the wrong GitHub client (got the fallback, not the resolver's per-org client)")
	}
	if gotModel != "claude-resolved-model" {
		t.Errorf("model = %q; want the modelFor-resolved value", gotModel)
	}
	if len(resolver.calls) != 1 {
		t.Fatalf("resolver.ClientFor called %d times; want 1", len(resolver.calls))
	}
	if resolver.calls[0].orgID != orgID || resolver.calls[0].target != owner {
		t.Errorf("ClientFor called with (org=%q, target=%q); want (org=%q, target=%q)",
			resolver.calls[0].orgID, resolver.calls[0].target, orgID, owner)
	}

	// The LLM-credential reader threaded into RunOptions.Secrets must be
	// exactly the one wired (system door in multi). nil here would resurface
	// the "SecretsReader is nil in multi mode" failure the ticket exists to fix.
	if s.getRunSecrets() != agentproc.SecretsReader(secrets) {
		t.Errorf("getRunSecrets did not return the wired reader")
	}
}

// TestResolveRunCredentials_FallbackWithoutSeam pins the test/no-seam path:
// with no resolver wired, the spawner falls back to the constructor-supplied
// client + model (the 20-odd existing test fixtures rely on this), and the
// secrets reader is nil → the local ambient-subscription fallback.
func TestResolveRunCredentials_FallbackWithoutSeam(t *testing.T) {
	fallbackClient := ghclient.NewClient("https://fallback", "fallback-tok")
	s := NewSpawner(nil, db.Stores{}, fallbackClient, nil, "fallback-model", "")

	gotClient, gotModel := s.resolveRunCredentials(context.Background(), "org", "owner", "team")
	if gotClient != fallbackClient {
		t.Errorf("without a resolver, expected the constructor client")
	}
	if gotModel != "fallback-model" {
		t.Errorf("model = %q; want the constructor model", gotModel)
	}
	if s.getRunSecrets() != nil {
		t.Errorf("getRunSecrets should be nil when no seam is wired (local ambient-subscription fallback)")
	}
}

// TestResolveGHClient_ResolveErrorReturnsNil pins that a resolver error
// yields a nil client (setupGitHub then surfaces "GitHub credentials not
// configured") rather than the stale fallback — a resolve failure must not
// silently route the run through a different org's process-global client.
func TestResolveGHClient_ResolveErrorReturnsNil(t *testing.T) {
	resolver := &fakeResolver{err: errors.New("vault down")}
	s := NewSpawner(nil, db.Stores{}, ghclient.NewClient("https://fallback", "fallback-tok"), nil, "m", "")
	s.SetRunCredentialResolvers(resolver, nil, nil)

	if got := s.resolveGHClient(context.Background(), "org", "owner"); got != nil {
		t.Errorf("resolveGHClient on resolver error = %v; want nil", got)
	}
}

// TestOwnerForTask pins the per-(org, owner) key derivation: GitHub PR
// tasks resolve the repo owner from "owner/repo#N"; Jira (and any non-github
// source) resolves to "" so the resolver picks the org's sole installation.
func TestOwnerForTask(t *testing.T) {
	cases := []struct {
		name string
		task domain.Task
		want string
	}{
		{"github PR", domain.Task{EntitySource: "github", EntitySourceID: "acme/widgets#42"}, "acme"},
		{"github no PR number", domain.Task{EntitySource: "github", EntitySourceID: "acme/widgets"}, "acme"},
		{"jira", domain.Task{EntitySource: "jira", EntitySourceID: "SKY-123"}, ""},
		{"malformed", domain.Task{EntitySource: "github", EntitySourceID: "nostructure"}, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ownerForTask(tc.task); got != tc.want {
				t.Errorf("ownerForTask(%+v) = %q; want %q", tc.task, got, tc.want)
			}
		})
	}
}
