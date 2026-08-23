package delegate

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/agentproc"
	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/githooks"
	ghclient "github.com/sky-ai-eng/triage-factory/internal/github"
	"github.com/sky-ai-eng/triage-factory/internal/github/ghbase"
	"github.com/sky-ai-eng/triage-factory/internal/githubapp"
)

// fakeUsers is a minimal UsersStore for the co-author lookup: it returns a
// fixed login from GetGitHubLoginSystem and embeds the interface so the
// unexercised methods compile (and panic only if unexpectedly called).
type fakeUsers struct {
	db.UsersStore
	login string
}

func (f fakeUsers) GetGitHubLoginSystem(_ context.Context, _, _ string) (string, error) {
	return f.login, nil
}

// fakeResolver records ClientFor calls and returns a preconfigured client.
// Mirrors the poller package's fake so the per-org run-credential seam is
// exercised through the real ghclient.Resolver interface.
type fakeResolver struct {
	calls  []resolverCall
	client *ghclient.Client
	err    error
	// token is what TokenFor hands back — the credential the
	// host-side clone injects. Defaults to the zero Token when unset.
	token githubapp.Token
	// baseURL is what BaseURLFor hands back — the org's git host base.
	// Empty defaults to github.com so existing fixtures need no change.
	baseURL string
	// orgLogin is the name OrgIdentityFor hands back; empty → ok=false (the
	// default, so credential-only fixtures keep getting a no-identity result).
	orgLogin string
	// orgEmail is the email OrgIdentityFor hands back. Empty defaults to the
	// plain "<orgLogin>@users.noreply.github.com" form, so existing fixtures need
	// no change; a fixture sets it explicitly to exercise the App-bot numeric-id
	// noreply form passing through the delegate seam verbatim (TFAC-474).
	orgEmail string
	// noCredential drives HasAnyCredential: when true the org has no GitHub
	// credential, so credprovision's resolveGitHub wires no git proxy. Independent of err
	// (a per-call resolve failure) so a fixture can exercise "config built but
	// TokenSource surfaces the typed no-cred error" vs "no proxy at all".
	noCredential bool
}

// TokenForRepoScoped + HasAnyCredential satisfy ghclient.ScopedResolver, the
// optional extension credprovision's resolveGitHub type-asserts for. The scoped token is
// the same f.token the unscoped TokenFor hands back (the fake doesn't model
// per-repo narrowing — the proxy/gate tests cover that).
func (f *fakeResolver) TokenForRepoScoped(ctx context.Context, orgID, owner, repo string, permissions map[string]string) (githubapp.Token, error) {
	if f.err != nil {
		return githubapp.Token{}, f.err
	}
	return f.token, nil
}

func (f *fakeResolver) HasAnyCredential(ctx context.Context, orgID string) (bool, error) {
	return !f.noCredential, nil
}

type resolverCall struct {
	orgID  string
	target string
}

func (f *fakeResolver) ClientFor(ctx context.Context, orgID, target string) (*ghclient.Client, error) {
	f.calls = append(f.calls, resolverCall{orgID: orgID, target: target})
	return f.client, f.err
}

func (f *fakeResolver) ClientForRepo(ctx context.Context, orgID, owner, repo string) (*ghclient.Client, error) {
	f.calls = append(f.calls, resolverCall{orgID: orgID, target: owner})
	return f.client, f.err
}

func (f *fakeResolver) TokenFor(ctx context.Context, orgID, target string) (githubapp.Token, error) {
	if f.err != nil {
		return githubapp.Token{}, f.err
	}
	return f.token, nil
}

func (f *fakeResolver) BaseURLFor(ctx context.Context, orgID string) (string, error) {
	if f.err != nil {
		return "", f.err
	}
	if f.baseURL != "" {
		return f.baseURL, nil
	}
	return ghbase.DefaultBaseURL, nil
}

// OrgIdentityFor satisfies the ghclient.Resolver interface. Returns the
// configured orgLogin (name) + orgEmail (ok iff the name is non-empty);
// credential-only fixtures leave both unset so the delegate's git-identity
// resolution stays a no-op. An unset orgEmail defaults to the plain
// "<orgLogin>@users.noreply.github.com" form.
func (f *fakeResolver) OrgIdentityFor(ctx context.Context, orgID string) (string, string, bool) {
	if f.orgLogin == "" {
		return "", "", false
	}
	email := f.orgEmail
	if email == "" {
		email = f.orgLogin + "@users.noreply.github.com"
	}
	return f.orgLogin, email, true
}

var _ ghclient.Resolver = (*fakeResolver)(nil)

// recordingSecrets is a SecretsReader test double whose identity we can
// assert flows through getRunSecrets unchanged.
type recordingSecrets struct{}

func (recordingSecrets) Get(ctx context.Context, orgID, key string) (string, error) {
	return "", nil
}

var _ agentproc.SecretsReader = recordingSecrets{}

// TestResolveRunCredentials_MultiSeam pins the contract: when the
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
	s := NewSpawner(nil, db.Stores{}, ghclient.NewClient("https://fallback", "fallback-tok"), nil, "fallback-model")
	s.SetRunCredentialResolvers(resolver, secrets, func(_ context.Context, gotOrg, gotTeam string) string {
		if gotOrg != orgID {
			t.Errorf("modelFor got org %q; want %q", gotOrg, orgID)
		}
		// The task's team must reach modelFor so a
		// multi-team org resolves the run's own team model, not the org default.
		if gotTeam != teamID {
			t.Errorf("modelFor got team %q; want %q", gotTeam, teamID)
		}
		return "claude-resolved-model"
	})

	gotClient, gotModel := s.resolveRunCredentials(context.Background(), orgID, owner, "widgets", teamID)

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
	s := NewSpawner(nil, db.Stores{}, fallbackClient, nil, "fallback-model")

	gotClient, gotModel := s.resolveRunCredentials(context.Background(), "org", "owner", "repo", "team")
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
	s := NewSpawner(nil, db.Stores{}, ghclient.NewClient("https://fallback", "fallback-tok"), nil, "m")
	s.SetRunCredentialResolvers(resolver, nil, nil)

	if got := s.resolveGHClient(context.Background(), "org", "owner", "repo"); got != nil {
		t.Errorf("resolveGHClient on resolver error = %v; want nil", got)
	}
}

// TestResumeWithMessage_RequiresModel pins the contract that a resume must
// reuse the model captured at run start, so ResumeWithMessage rejects an
// empty opts.Model with an error rather than falling back to a live
// per-(org, team) resolve that could switch models mid-run. The guard sits
// before any subprocess work (after the sessionID/cwd checks), so a bare
// spawner with no stores is sufficient to exercise it.
func TestResumeWithMessage_RequiresModel(t *testing.T) {
	s := NewSpawner(nil, db.Stores{}, nil, nil, "fallback-model")
	// Even with a constructor fallback model set, an empty opts.Model must
	// NOT silently borrow it — the resume has to carry the run's own model.
	_, err := s.ResumeWithMessage(context.Background(), "org", "run-1", "sess-1", "/tmp/wt", "hi", ResumeOptions{}, "manual", "user-1")
	if err == nil {
		t.Fatal("ResumeWithMessage with empty opts.Model returned nil error; want a missing-model error")
	}
	if !strings.Contains(err.Error(), "missing model") {
		t.Errorf("error = %q; want it to mention the missing model", err)
	}
}

// TestOwnerRepoForTask pins the per-(org, owner, repo) key derivation:
// GitHub PR tasks resolve owner+repo from "owner/repo#N"; Jira (and any
// non-github source) resolves to ("", "") so the resolver picks the org's
// sole installation.
func TestOwnerRepoForTask(t *testing.T) {
	cases := []struct {
		name      string
		task      domain.Task
		wantOwner string
		wantRepo  string
	}{
		{"github PR", domain.Task{EntitySource: "github", EntitySourceID: "acme/widgets#42"}, "acme", "widgets"},
		{"github no PR number", domain.Task{EntitySource: "github", EntitySourceID: "acme/widgets"}, "acme", "widgets"},
		{"jira", domain.Task{EntitySource: "jira", EntitySourceID: "SKY-123"}, "", ""},
		{"malformed", domain.Task{EntitySource: "github", EntitySourceID: "nostructure"}, "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			owner, repo := ownerRepoForTask(tc.task)
			if owner != tc.wantOwner || repo != tc.wantRepo {
				t.Errorf("ownerRepoForTask(%+v) = (%q, %q); want (%q, %q)", tc.task, owner, repo, tc.wantOwner, tc.wantRepo)
			}
		})
	}
}

// TestResolveCommitIdentity_DelegateSeam pins the delegate's commit-identity
// resolution (TFAC-452) end to end through the real ghclient.Resolver interface:
// a resolver miss stamps nothing (so RunOptions.GitUserName/Email stay empty and
// the agent inherits ambient git config), an event run yields the org identity
// with no trailer, and a manual run with a distinct delegating login adds the
// Co-authored-by trailer.
func TestResolveCommitIdentity_DelegateSeam(t *testing.T) {
	const orgID = "11111111-2222-3333-4444-555555555555"

	newSpawner := func(resolver ghclient.Resolver, users db.UsersStore) *Spawner {
		s := NewSpawner(nil, db.Stores{}, ghclient.NewClient("https://fallback", "tok"), nil, "m")
		s.SetRunCredentialResolvers(resolver, nil, nil)
		if users != nil {
			s.SetStores(db.Stores{Users: users})
		}
		return s
	}

	t.Run("resolver miss -> no identity (ambient inherited)", func(t *testing.T) {
		// orgLogin unset -> OrgIdentityFor returns ok=false.
		s := newSpawner(&fakeResolver{}, nil)
		id := s.resolveCommitIdentity(context.Background(), orgID, "manual", "user-1")
		if id != (githooks.CommitIdentity{}) {
			t.Fatalf("resolver miss stamped %+v; want zero CommitIdentity (no injection)", id)
		}
	})

	t.Run("event run -> org identity, no trailer", func(t *testing.T) {
		s := newSpawner(&fakeResolver{orgLogin: "acme-bot"}, nil)
		id := s.resolveCommitIdentity(context.Background(), orgID, "event", "")
		if id.Name != "acme-bot" || id.Email != "acme-bot@users.noreply.github.com" {
			t.Errorf("identity = %+v; want the acme-bot org identity", id)
		}
		if id.CoAuthorTrailer != "" {
			t.Errorf("event run carried a trailer: %q", id.CoAuthorTrailer)
		}
	})

	t.Run("manual distinct user -> org identity + trailer", func(t *testing.T) {
		s := newSpawner(&fakeResolver{orgLogin: "acme-bot"}, fakeUsers{login: "alice"})
		id := s.resolveCommitIdentity(context.Background(), orgID, "manual", "user-1")
		if id.Name != "acme-bot" {
			t.Errorf("author = %q; want acme-bot", id.Name)
		}
		want := "Co-authored-by: alice <alice@users.noreply.github.com>"
		if id.CoAuthorTrailer != want {
			t.Errorf("trailer = %q; want %q", id.CoAuthorTrailer, want)
		}
	})

	t.Run("App numeric-id email threads through verbatim, plain human trailer", func(t *testing.T) {
		// TFAC-474: the resolver's numeric-id noreply email must reach
		// RunOptions.GitUserEmail untouched, while the co-author trailer keeps the
		// plain human form.
		s := newSpawner(&fakeResolver{
			orgLogin: "acme-bot[bot]",
			orgEmail: "41898282+acme-bot[bot]@users.noreply.github.com",
		}, fakeUsers{login: "alice"})
		id := s.resolveCommitIdentity(context.Background(), orgID, "manual", "user-1")
		if id.Name != "acme-bot[bot]" {
			t.Errorf("name = %q; want acme-bot[bot]", id.Name)
		}
		if id.Email != "41898282+acme-bot[bot]@users.noreply.github.com" {
			t.Errorf("email = %q; want the numeric-id noreply form passed through verbatim", id.Email)
		}
		want := "Co-authored-by: alice <alice@users.noreply.github.com>"
		if id.CoAuthorTrailer != want {
			t.Errorf("trailer = %q; want %q (plain human form, never numeric)", id.CoAuthorTrailer, want)
		}
	})
}
