package delegate

import (
	"context"
	"strings"
	"testing"

	"github.com/sky-ai-eng/triage-factory/cmd/exec/agenthost"
	"github.com/sky-ai-eng/triage-factory/cmd/gitssh"
	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	ghclient "github.com/sky-ai-eng/triage-factory/internal/github"
	"github.com/sky-ai-eng/triage-factory/internal/githubapp"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
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
