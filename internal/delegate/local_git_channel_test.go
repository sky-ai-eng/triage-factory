package delegate

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"testing"

	"github.com/sky-ai-eng/triage-factory/cmd/exec/agenthost"
	"github.com/sky-ai-eng/triage-factory/cmd/gitssh"
	"github.com/sky-ai-eng/triage-factory/internal/db"
	sqlitestore "github.com/sky-ai-eng/triage-factory/internal/db/sqlite"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	ghclient "github.com/sky-ai-eng/triage-factory/internal/github"
	"github.com/sky-ai-eng/triage-factory/internal/githubapp"
	"github.com/sky-ai-eng/triage-factory/internal/paths"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
	"github.com/sky-ai-eng/triage-factory/internal/upstream"
	"github.com/sky-ai-eng/triage-factory/internal/worktree"
)

type localGitResolver struct {
	*fakeResolver
}

func (r *localGitResolver) TokenForReposScoped(context.Context, string, string, []string, map[string]string) (githubapp.Token, error) {
	return r.token, r.err
}

var _ ghclient.ScopedResolver = (*localGitResolver)(nil)

func TestStartLocalGitChannel_RefusesAmbientFallbackForGitHubTask(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeLocal)
	s := NewSpawner(nil, db.Stores{}, nil, nil, "")
	s.SetStores(db.Stores{})
	s.SetRunCredentialResolvers(&localGitResolver{fakeResolver: &fakeResolver{noCredential: true}}, nil, nil)

	_, err := s.startLocalGitChannel(context.Background(), runmode.LocalDefaultOrgID,
		domain.Task{EntitySource: "github", EntitySourceID: "acme/widgets#7"},
		agenthost.ConversationInfo{OrgID: runmode.LocalDefaultOrgID, ConversationID: "conv-1"})
	if err == nil || !strings.Contains(err.Error(), "refuses to fall back") {
		t.Fatalf("startLocalGitChannel error = %v, want explicit ambient-credential refusal", err)
	}
}

func TestStartLocalGitChannel_RoutesHTTPSAndSSHForms(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeLocal)
	repos := &seedRepositoryStore{profile: &domain.Repository{
		Owner: "acme", Repo: "widgets", CloneURL: "git@github.com:acme/widgets.git",
	}}
	stores := db.Stores{Repos: repos}
	s := NewSpawner(nil, stores, nil, nil, "")
	s.SetStores(stores)
	s.SetRunCredentialResolvers(&localGitResolver{fakeResolver: &fakeResolver{
		token:   githubapp.Token{Value: "configured-bot-token"},
		baseURL: "https://github.com",
	}}, nil, nil)

	channel, err := s.startLocalGitChannel(context.Background(), runmode.LocalDefaultOrgID,
		domain.Task{EntitySource: "github", EntitySourceID: "acme/widgets#7"},
		agenthost.ConversationInfo{OrgID: runmode.LocalDefaultOrgID, ConversationID: "conv-2"})
	if err != nil {
		t.Fatalf("startLocalGitChannel: %v", err)
	}
	defer func() { _ = channel.Close() }()

	pairs := channel.configPairs(nil)
	var rewrites []string
	for _, pair := range pairs {
		if strings.HasSuffix(pair[0], ".insteadOf") {
			rewrites = append(rewrites, pair[1])
		}
		if strings.Contains(pair[1], "configured-bot-token") {
			t.Fatalf("real GitHub credential leaked into agent Git config: %v", pair)
		}
	}
	for _, want := range []string{"https://github.com/", "git@github.com:", "ssh://git@github.com/"} {
		found := false
		for _, got := range rewrites {
			found = found || got == want
		}
		if !found {
			t.Errorf("Git rewrites = %v, missing %q", rewrites, want)
		}
	}

	seed := s.gitSeedFor(context.Background(), runmode.LocalDefaultOrgID, "acme", "widgets", nil, channel)
	if seed.cloneURL != "https://github.com/acme/widgets.git" {
		t.Errorf("local rehydrate clone URL = %q, want managed HTTPS form", seed.cloneURL)
	}
	proxyURL, placeholder := channel.proxy.Coordinates()
	assertEntries(t, seed.auth.GitConfigEntries(), wantProxyEntries(proxyURL, "https://github.com", placeholder))
}

// The rewrites cover the canonical SSH spellings of the org host and the
// bridge covers everything else, so both have to be in the run env at once:
// git only execs the dispatcher for a remote no rewrite caught.
func TestStartLocalGitChannel_BridgesSSHOntoTheSameProxy(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeLocal)
	stores := db.Stores{Repos: &seedRepositoryStore{}}
	s := NewSpawner(nil, stores, nil, nil, "")
	s.SetStores(stores)
	s.SetRunCredentialResolvers(&localGitResolver{fakeResolver: &fakeResolver{
		token:   githubapp.Token{Value: "configured-bot-token"},
		baseURL: "https://ghe.example.com",
	}}, nil, nil)

	channel, err := s.startLocalGitChannel(context.Background(), runmode.LocalDefaultOrgID,
		domain.Task{EntitySource: "github", EntitySourceID: "acme/widgets#7"},
		agenthost.ConversationInfo{OrgID: runmode.LocalDefaultOrgID, ConversationID: "conv-3"})
	if err != nil {
		t.Fatalf("startLocalGitChannel: %v", err)
	}
	defer func() { _ = channel.Close() }()

	env := map[string]string{}
	for _, entry := range channel.sshBridgeEnv() {
		key, value, _ := strings.Cut(entry, "=")
		env[key] = value
	}
	proxyURL, placeholder := channel.proxy.Coordinates()
	want := map[string]string{
		"GIT_SSH_COMMAND":         gitSSHCommand,
		"GIT_SSH_VARIANT":         "ssh",
		gitssh.UpstreamHostEnvVar: "ghe.example.com",
		gitssh.ProxyURLEnvVar:     proxyURL,
		gitssh.ProxyTokenEnvVar:   placeholder,
	}
	for key, wantValue := range want {
		if env[key] != wantValue {
			t.Errorf("%s = %q, want %q", key, env[key], wantValue)
		}
	}
	if strings.Contains(strings.Join(channel.sshBridgeEnv(), " "), "configured-bot-token") {
		t.Error("real GitHub credential leaked into the SSH bridge env")
	}

	// A bridged fetch maps one protocol-v2 command onto one stateless request;
	// v0 has no such mapping, so the run pins the version rather than
	// discovering it is unbridgeable mid-session.
	var pinned bool
	for _, pair := range channel.configPairs(nil) {
		if pair[0] == "protocol.version" {
			pinned = pair[1] == "2"
		}
	}
	if !pinned {
		t.Errorf("Git config = %v, want protocol.version pinned to 2", channel.configPairs(nil))
	}
}

// An org that chose the SSH clone protocol chose the operator's key as its Git
// identity. The managed channel is HTTPS end to end, so it must not start —
// otherwise every downstream decision it drives substitutes the one transport
// the operator explicitly chose against.
func TestStartLocalGitChannel_SSHProtocolKeepsTheOperatorsTransport(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeLocal)
	stores := db.Stores{
		Repos: &seedRepositoryStore{},
		Orgs:  stubOrgsSettings{settings: domain.OrgSettings{GitHubCloneProtocol: "ssh"}},
	}
	s := NewSpawner(nil, stores, nil, nil, "")
	s.SetStores(stores)
	s.SetRunCredentialResolvers(&localGitResolver{fakeResolver: &fakeResolver{
		token:   githubapp.Token{Value: "configured-bot-token"},
		baseURL: "https://ghe.example.com",
	}}, nil, nil)

	channel, err := s.startLocalGitChannel(context.Background(), runmode.LocalDefaultOrgID,
		domain.Task{EntitySource: "github", EntitySourceID: "acme/widgets#7"},
		agenthost.ConversationInfo{OrgID: runmode.LocalDefaultOrgID, ConversationID: "conv-ssh"})
	if err != nil {
		t.Fatalf("startLocalGitChannel: %v", err)
	}
	if channel != nil {
		_ = channel.Close()
		t.Fatal("an SSH-protocol org got a managed HTTPS Git channel, want none")
	}
	// Every downstream decision hangs off the channel's absence, so a nil one
	// is the whole answer: no rewrites, no bridge, and TF_GIT_PUSH_CAPTURE
	// unset, which hands ref policy and recording to the pre-push hook.
	if channel.configPairs(nil) != nil || channel.sshBridgeEnv() != nil {
		t.Error("a nil channel still produced Git config or SSH bridge env")
	}
}

// Choosing SSH says which transport carries Git, not that GitHub can be worked
// without a credential — the REST surface needs one either way. So the ambient
// refusal outranks the protocol rather than being excused by it.
func TestStartLocalGitChannel_SSHProtocolDoesNotExcuseAnUnconfiguredOrg(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeLocal)
	stores := db.Stores{Orgs: stubOrgsSettings{settings: domain.OrgSettings{GitHubCloneProtocol: "ssh"}}}
	s := NewSpawner(nil, stores, nil, nil, "")
	s.SetStores(stores)
	s.SetRunCredentialResolvers(&localGitResolver{fakeResolver: &fakeResolver{noCredential: true}}, nil, nil)

	_, err := s.startLocalGitChannel(context.Background(), runmode.LocalDefaultOrgID,
		domain.Task{EntitySource: "github", EntitySourceID: "acme/widgets#7"},
		agenthost.ConversationInfo{OrgID: runmode.LocalDefaultOrgID, ConversationID: "conv-none"})
	if err == nil || !strings.Contains(err.Error(), "refuses to fall back") {
		t.Fatalf("startLocalGitChannel error = %v, want the ambient-credential refusal", err)
	}
}

// A credential the local resolver can never produce for a repository (here,
// an App with no installation on the repository's owner) is answered the way
// the sidecar answers a missing one: a 403, which git reports as a refusal.
// Driven through the clone a GitHub setup runs, through the real gate and
// token source, that makes it an ordinary setup failure rather than an
// unreachable git host that would be retried for four hours. A resolver that
// could not reach GitHub, or that GitHub rate-limited, keeps the 502 an
// outage is answered with.
func TestLocalGitChannel_AnUnresolvableCredentialIsARefusalNotAnOutage(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeLocal)
	for name, tc := range map[string]struct {
		resolveErr error
		wantStatus string
		upstream   bool
	}{
		"no installation for the owner": {
			resolveErr: fmt.Errorf("%w: org=%s: app has no installation for owner", ghclient.ErrNoGitHubCredentials, runmode.LocalDefaultOrgID),
			wantStatus: "The requested URL returned error: 403",
		},
		"GitHub unreachable while minting": {
			resolveErr: fmt.Errorf("githubapp: mint installation token: %w", &upstream.TransportError{Err: &url.Error{
				Op: "Post", URL: "https://api.github.com/app/installations/1/access_tokens", Err: errors.New("dial tcp: lookup api.github.com: no such host"),
			}}),
			wantStatus: "The requested URL returned error: 502",
			upstream:   true,
		},
		"GitHub rate-limited the mint": {
			resolveErr: &githubapp.APIStatusError{Op: "mint installation token", StatusCode: 429, Class: upstream.RateLimited},
			wantStatus: "The requested URL returned error: 502",
			upstream:   true,
		},
	} {
		t.Run(name, func(t *testing.T) {
			paths.SetForTest(t, t.TempDir())
			database := newDelegateTestDB(t)
			stores := sqlitestore.New(database)
			ctx := context.Background()
			seedConversation(t, database, "run-cred", "sess", "")
			if err := stores.TeamGitHubRepos.ReplaceForTeam(ctx, runmode.LocalDefaultOrgID, runmode.LocalDefaultTeamID,
				[]domain.TeamGitHubRepo{{Owner: "owner", Repo: "repo"}}); err != nil {
				t.Fatalf("track repo: %v", err)
			}
			if _, err := stores.Repos.Upsert(ctx, runmode.LocalDefaultOrgID, domain.Repository{
				Owner: "owner", Repo: "repo", DefaultBranch: "main", CloneURL: "https://github.com/owner/repo.git", ProfileText: "t",
			}); err != nil {
				t.Fatalf("seed repository: %v", err)
			}

			s := NewSpawner(nil, stores, nil, nil, "")
			s.SetStores(stores)
			s.SetRunCredentialResolvers(&localGitResolver{fakeResolver: &fakeResolver{
				err: tc.resolveErr, baseURL: "https://github.com",
			}}, nil, nil)
			channel, err := s.startLocalGitChannel(ctx, runmode.LocalDefaultOrgID,
				domain.Task{EntitySource: "github", EntitySourceID: "owner/repo#run-cred"},
				agenthost.ConversationInfo{OrgID: runmode.LocalDefaultOrgID, TeamID: runmode.LocalDefaultTeamID, ConversationID: "run-cred"})
			if err != nil {
				t.Fatalf("startLocalGitChannel: %v", err)
			}
			defer func() { _ = channel.Close() }()

			upstreamURL := "https://github.com/owner/repo.git"
			_, cloneErr := worktree.CreateForPR(ctx, "owner", "repo", upstreamURL, "", "feature", 7, "task-cred",
				worktree.WithCloneAuth(channel.cloneAuth(upstreamURL)))
			var gitErr *worktree.GitError
			if !errors.As(cloneErr, &gitErr) {
				t.Fatalf("clone error = %v, want a git command that failed", cloneErr)
			}
			if !strings.Contains(gitErr.Output, tc.wantStatus) {
				t.Errorf("git output = %q, want it to report %q", gitErr.Output, tc.wantStatus)
			}
			cause := fmt.Errorf("failed to create worktree: %w", cloneErr)
			if got := upstreamSetupFailure(cause); got != tc.upstream {
				t.Errorf("upstreamSetupFailure = %v, want %v", got, tc.upstream)
			}
		})
	}
}

// A gate that cannot read its own data refuses the request, and git reports
// the refusal as one: the clone a GitHub setup runs then fails as an ordinary
// setup failure. The data is TF's own, so this is never the git host being
// unreachable, which is what the 502 git reports would claim.
func TestLocalGitChannel_AGateThatCannotDecideIsARefusalNotAnOutage(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeLocal)
	paths.SetForTest(t, t.TempDir())
	database := newDelegateTestDB(t)
	stores := sqlitestore.New(database)
	ctx := context.Background()
	seedConversation(t, database, "run-gate", "sess", "")
	stores.TeamGitHubRepos = failingTracksStore{TeamGitHubReposStore: stores.TeamGitHubRepos, err: errors.New("database is locked")}

	s := NewSpawner(nil, stores, nil, nil, "")
	s.SetStores(stores)
	s.SetRunCredentialResolvers(&localGitResolver{fakeResolver: &fakeResolver{baseURL: "https://github.com"}}, nil, nil)
	channel, err := s.startLocalGitChannel(ctx, runmode.LocalDefaultOrgID,
		domain.Task{EntitySource: "github", EntitySourceID: "owner/repo#run-gate"},
		agenthost.ConversationInfo{OrgID: runmode.LocalDefaultOrgID, TeamID: runmode.LocalDefaultTeamID, ConversationID: "run-gate"})
	if err != nil {
		t.Fatalf("startLocalGitChannel: %v", err)
	}
	defer func() { _ = channel.Close() }()

	upstreamURL := "https://github.com/owner/repo.git"
	_, cloneErr := worktree.CreateForPR(ctx, "owner", "repo", upstreamURL, "", "feature", 7, "task-gate",
		worktree.WithCloneAuth(channel.cloneAuth(upstreamURL)))
	var gitErr *worktree.GitError
	if !errors.As(cloneErr, &gitErr) {
		t.Fatalf("clone error = %v, want a git command that failed", cloneErr)
	}
	if !strings.Contains(gitErr.Output, "The requested URL returned error: 403") {
		t.Errorf("git output = %q, want it to report a 403", gitErr.Output)
	}
	if upstreamSetupFailure(fmt.Errorf("failed to create worktree: %w", cloneErr)) {
		t.Error("upstreamSetupFailure = true, want false: the gate's own store failed, not the git host")
	}
}
