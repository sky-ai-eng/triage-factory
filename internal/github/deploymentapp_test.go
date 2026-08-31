package github

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/githubapp"
	"github.com/sky-ai-eng/triage-factory/internal/integrations"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// stubbedSource is a deploymentAppSource whose two network reads are counters,
// so the caching and coalescing can be pinned without a server.
func stubbedSource(t *testing.T, app githubapp.DeploymentApp) (*deploymentAppSource, *int32, *int32) {
	t.Helper()
	var preflights, botLookups int32
	src := newDeploymentAppSource(app)
	src.preflight = func(context.Context, *githubapp.Minter) (githubapp.DeploymentAppIdentity, error) {
		atomic.AddInt32(&preflights, 1)
		return githubapp.DeploymentAppIdentity{Slug: deploymentSlug, ClientID: "Iv1.deployment", MembersPermission: "read"}, nil
	}
	src.botUserID = func(context.Context, string, string) (int64, error) {
		atomic.AddInt32(&botLookups, 1)
		return deploymentBotUserID, nil
	}
	return src, &preflights, &botLookups
}

// TestDeploymentAppSource_CachesPerHostUntilTTL pins the shape of the cache:
// one preflight per host per TTL, and a re-ask once it lapses.
//
// The cost this buys is real — resolution happens per org per poll cycle, per
// token mint, per run start — and the thing it must not become is a preflight
// that never re-runs, because losing the members permission on GitHub has to
// stop the tier within minutes rather than at the next deploy.
func TestDeploymentAppSource_CachesPerHostUntilTTL(t *testing.T) {
	src, preflights, botLookups := stubbedSource(t, testDeploymentApp(t))
	now := time.Now()
	src.now = func() time.Time { return now }
	ctx := context.Background()

	for range 3 {
		if _, err := src.resolve(ctx, "https://ghe.acme.test"); err != nil {
			t.Fatalf("resolve: %v", err)
		}
	}
	if got := atomic.LoadInt32(preflights); got != 1 {
		t.Errorf("preflight ran %d times inside one TTL; want 1", got)
	}
	if got := atomic.LoadInt32(botLookups); got != 1 {
		t.Errorf("bot lookup ran %d times inside one TTL; want 1 — it is established with the identity, not per resolution", got)
	}

	// A second host is a second question: the App's identity is established BY a
	// call to a particular GitHub, so a verdict reached against one host cannot
	// answer for another.
	if _, err := src.resolve(ctx, "https://ghe.other.test"); err != nil {
		t.Fatalf("resolve on a second host: %v", err)
	}
	if got := atomic.LoadInt32(preflights); got != 2 {
		t.Errorf("preflight ran %d times across two hosts; want 2", got)
	}

	now = now.Add(deploymentAppTTL + time.Second)
	if _, err := src.resolve(ctx, "https://ghe.acme.test"); err != nil {
		t.Fatalf("resolve after the TTL: %v", err)
	}
	if got := atomic.LoadInt32(preflights); got != 3 {
		t.Errorf("preflight ran %d times after the TTL lapsed; want 3 — a cached verdict must expire", got)
	}
}

// TestDeploymentAppSource_ConcurrentMissesCoalesce pins the singleflight. A
// cold cache under a poll cycle's fan-out is N orgs asking one question at
// once, and without coalescing that is N identical GET /app calls establishing
// one fact.
func TestDeploymentAppSource_ConcurrentMissesCoalesce(t *testing.T) {
	src, preflights, _ := stubbedSource(t, testDeploymentApp(t))
	release := make(chan struct{})
	inner := src.preflight
	src.preflight = func(ctx context.Context, m *githubapp.Minter) (githubapp.DeploymentAppIdentity, error) {
		<-release // hold the flight open so the others have to join it
		return inner(ctx, m)
	}

	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := src.resolve(context.Background(), "https://ghe.acme.test"); err != nil {
				t.Errorf("resolve: %v", err)
			}
		}()
	}
	// Nothing here can prove all eight arrived before the flight completes, so
	// the assertion is the weaker true one: they must not have produced eight
	// preflights.
	time.Sleep(20 * time.Millisecond)
	close(release)
	wg.Wait()

	if got := atomic.LoadInt32(preflights); got != 1 {
		t.Errorf("preflight ran %d times for 8 concurrent resolutions of one host; want 1", got)
	}
}

// TestDeploymentAppSource_UnconfiguredRefusesWithoutAsking pins that a
// deployment with no shared App refuses immediately. There is nothing to ask
// GitHub about, and asking would spend a round trip per resolution on a
// question whose answer is in the process's own configuration.
func TestDeploymentAppSource_UnconfiguredRefusesWithoutAsking(t *testing.T) {
	src, preflights, _ := stubbedSource(t, githubapp.DeploymentApp{})

	state, err := src.resolve(context.Background(), "https://github.com")
	if state != nil {
		t.Error("an unconfigured deployment App resolved a state")
	}
	if !errors.Is(err, ErrDeploymentAppUnavailable) || !errors.Is(err, githubapp.ErrNoDeploymentApp) {
		t.Errorf("err = %v; want ErrDeploymentAppUnavailable naming the missing App", err)
	}
	if got := atomic.LoadInt32(preflights); got != 0 {
		t.Errorf("preflight ran %d times with no App configured; want 0", got)
	}
}

// TestWithDeploymentAppFromEnv_LocalModeHasNone pins the multi-only gate at the
// wiring site. A distributed local binary ships no shared key, so a local
// process must hold no deployment App however the environment is set — and a
// managed-class org there resolves nothing rather than something.
func TestWithDeploymentAppFromEnv_LocalModeHasNone(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeLocal)
	t.Setenv("TF_GITHUB_APP_ID", "987")
	t.Setenv("TF_GITHUB_APP_PRIVATE_KEY", testPEM(t))
	t.Setenv("TF_GITHUB_APP_WEBHOOK_SECRET", "whsec")
	t.Setenv("TF_GITHUB_APP_CLIENT_SECRET", "cs")

	r := NewResolver(
		&fakeSecrets{vals: map[string]string{integrations.KeyGitHubPAT: "ghp_present"}},
		&fakeApps{app: nil, insts: []domain.OrgGitHubAppInstallation{installOn("acme")}},
		&fakeOrgs{base: "https://github.com", class: domain.GitHubCredentialClassManagedApp},
		&fakeAgents{},
		nil,
		WithDeploymentAppFromEnv(),
	)

	_, err := r.ClientFor(context.Background(), "org-1", "acme")
	if !errors.Is(err, githubapp.ErrNoDeploymentApp) {
		t.Errorf("err = %v; want the no-deployment-App refusal — local mode reads no shared key, whatever the environment says", err)
	}
}

// TestNewResolver_WithoutOptionsRefusesTheManagedClass pins the default. Most
// construction sites never heard of tier 2 (the local CLI's agenthost among
// them), and a resolver built without the option must read as "no deployment
// App" — a refusal — rather than half-supporting the class.
func TestNewResolver_WithoutOptionsRefusesTheManagedClass(t *testing.T) {
	r := NewResolver(
		&fakeSecrets{vals: map[string]string{integrations.KeyGitHubPAT: "ghp_present"}},
		&fakeApps{app: nil, insts: []domain.OrgGitHubAppInstallation{installOn("acme")}},
		&fakeOrgs{base: "https://github.com", class: domain.GitHubCredentialClassManagedApp},
		&fakeAgents{},
		nil,
	)

	tok, err := r.TokenFor(context.Background(), "org-1", "acme")
	if !errors.Is(err, ErrDeploymentAppUnavailable) {
		t.Errorf("err = %v; want ErrDeploymentAppUnavailable", err)
	}
	if tok.Value != "" {
		t.Errorf("TokenFor returned %q; a resolver with no deployment App must hand back nothing at all", tok.Value)
	}
}
