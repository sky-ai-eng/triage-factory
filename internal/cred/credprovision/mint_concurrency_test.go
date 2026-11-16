package credprovision

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/cred/credbundle"
	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/eventing/eventsource"
	ghclient "github.com/sky-ai-eng/triage-factory/internal/github"
)

// pairedMintGitHub is a GitHub that answers no installation-token request
// until two are in flight at once, so a seal that mints one token at a time
// waits on its first mint until the seal's deadline.
type pairedMintGitHub struct {
	mu       sync.Mutex
	inFlight int
	paired   chan struct{}
}

func newPairedMintGitHub(t *testing.T) *httptest.Server {
	t.Helper()
	g := &pairedMintGitHub{paired: make(chan struct{})}
	// done releases a request still waiting when the test ends. The handler
	// cannot rely on its request's context for that: the server cancels it
	// on a client's disconnect only after the body is read, and Close waits
	// on every handler still running.
	done := make(chan struct{})
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/v3/app/installations/{id}/access_tokens", func(w http.ResponseWriter, r *http.Request) {
		g.mu.Lock()
		g.inFlight++
		if g.inFlight == 2 {
			close(g.paired)
		}
		g.mu.Unlock()
		select {
		case <-g.paired:
		case <-done:
			return
		}
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"token":      "ghs_minted",
			"expires_at": time.Now().Add(time.Hour).UTC(),
		})
	})
	srv := httptest.NewServer(mux)
	// Cleanups run last-registered first, so done closes before Close waits.
	t.Cleanup(srv.Close)
	t.Cleanup(func() { close(done) })
	return srv
}

// oneApp is an org's active App registration and its one installation, on
// acme.
type oneApp struct {
	db.GitHubAppsStore
}

func (oneApp) GetForOrgSystem(context.Context, string) (*domain.OrgGitHubApp, error) {
	return &domain.OrgGitHubApp{OrgID: "org-1", AppID: "123", PEMRef: "pem", Slug: "acme-bot", Active: true}, nil
}

func (oneApp) ListInstallationsOnHostSystem(context.Context, string, string) ([]domain.OrgGitHubAppInstallation, error) {
	return []domain.OrgGitHubAppInstallation{{InstallationID: "456", OrgID: "org-1", AccountType: "Organization", AccountLogin: "acme"}}, nil
}

func appPEM(t *testing.T) string {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)}))
}

// TestProvision_MintsTheSealsGitHubTokensTogether: a one-repository
// conversation's seal mints two installation tokens, its repository's and the
// gh channel's, and requests both at once. Against a GitHub that answers
// neither until both are in flight, the seal completes; one that minted them
// one after another would still be waiting on the first at its deadline.
func TestProvision_MintsTheSealsGitHubTokensTogether(t *testing.T) {
	srv := newPairedMintGitHub(t)
	orgs := &fakeOrgs{githubBaseURL: srv.URL, class: domain.GitHubCredentialClassBYOApp}
	host := db.EffectiveGitHubHost(srv.URL)

	rig := newProvisionRig(t, eventsource.Availability{
		eventsource.NewSource(eventsource.KindGitHub, eventsource.StateAvailable),
	}, &installTokens{})
	queue := rig.m.stores.ConversationQueue.(*provisionQueue)
	queue.claim.TeamID, queue.claim.TaskID = "team-1", "task-1"
	rig.m.stores.Orgs = orgs
	rig.m.stores.TeamGitHubRepos = &fakeTeamRepos{host: host, tracked: map[string]bool{"acme/widgets": true}}
	rig.m.stores.Tasks = &fakeTasks{task: &domain.Task{EntitySource: "github", EntitySourceID: "acme/widgets#42"}}
	rig.m.stores.Entities = &fakeEntities{scope: host}
	rig.m.stores.Repos = fakeRepos{}
	rig.m.stores.ConversationWorktrees = &fakeConversationWorktrees{}
	rig.m.ghResolver = ghclient.NewResolver(signingSecrets{keys: map[string]string{"pem": appPEM(t)}}, oneApp{}, orgs, nil, nil)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := rig.m.ProvisionForConversation(ctx, "org-1", "conv-1", ReasonCredRequest); err != nil {
		t.Fatalf("ProvisionForConversation: %v", err)
	}
	plaintext, err := rig.kp.Open(rig.claims.sealed)
	if err != nil {
		t.Fatalf("open the written bundle: %v", err)
	}
	bundle, err := credbundle.Unmarshal(plaintext)
	if err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if bundle.GitHub == nil || bundle.GitHub.RepoTokens["acme/widgets"].Token != "ghs_minted" || bundle.GitHub.CLIToken == nil {
		t.Errorf("GitHub half = %+v, want the repository's token and the gh channel's", bundle.GitHub)
	}
}
