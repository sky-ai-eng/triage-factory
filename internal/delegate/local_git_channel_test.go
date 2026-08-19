package delegate

import (
	"context"
	"strings"
	"testing"

	"github.com/sky-ai-eng/triage-factory/cmd/exec/agenthost"
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
