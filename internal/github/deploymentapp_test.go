package github

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/github/ghbase"
	"github.com/sky-ai-eng/triage-factory/internal/githubapp"
	"github.com/sky-ai-eng/triage-factory/internal/integrations"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// stubbedSource is a deploymentAppSource whose two network reads are counters,
// so the caching and coalescing can be pinned without a server.
func stubbedSource(t *testing.T, app githubapp.DeploymentApp) (*deploymentAppSource, *int32, *int32) {
	t.Helper()
	var preflights, botLookups int32
	// The deployment's GitHub, for the whole test: every resolve below asks
	// about an org on it.
	ghbase.SetDefaultBaseURLForTest(t, deploymentTestHost)
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

// deploymentTestHost is the deployment's GitHub in these tests — a GHES rather
// than github.com, so nothing below can pass on the literal.
const deploymentTestHost = "https://ghe.acme.test"

// TestDeploymentAppSource_CachesUntilTTL pins the shape of the cache: one
// preflight per TTL, whatever spelling of the deployment's host the orgs
// arrive with, and a re-ask once it lapses.
//
// The cost this buys is real — resolution happens per org per poll cycle, per
// token mint, per run start — and the thing it must not become is a preflight
// that never re-runs, because losing the members permission on GitHub has to
// stop the tier within minutes rather than at the next deploy.
func TestDeploymentAppSource_CachesUntilTTL(t *testing.T) {
	src, preflights, botLookups := stubbedSource(t, testDeploymentApp(t))
	now := time.Now()
	src.now = func() time.Time { return now }
	ctx := context.Background()

	// Three orgs on the deployment's GitHub: one with the host spelled out,
	// one with a trailing slash, one that configured nothing and resolves to
	// the default. One question, one preflight.
	for _, base := range []string{deploymentTestHost, deploymentTestHost + "/", ""} {
		if _, err := src.resolve(ctx, base); err != nil {
			t.Fatalf("resolve(%q): %v", base, err)
		}
	}
	if got := atomic.LoadInt32(preflights); got != 1 {
		t.Errorf("preflight ran %d times inside one TTL; want 1", got)
	}
	if got := atomic.LoadInt32(botLookups); got != 1 {
		t.Errorf("bot lookup ran %d times inside one TTL; want 1 — it is established with the identity, not per resolution", got)
	}

	now = now.Add(deploymentAppTTL + time.Second)
	if _, err := src.resolve(ctx, deploymentTestHost); err != nil {
		t.Fatalf("resolve after the TTL: %v", err)
	}
	if got := atomic.LoadInt32(preflights); got != 2 {
		t.Errorf("preflight ran %d times after the TTL lapsed; want 2 — a cached verdict must expire", got)
	}
}

// TestDeploymentAppSource_AnotherGitHubRefusesWithoutAsking: the deployment App
// is on one GitHub, so an org whose host is another is refused before any
// flight — naming both hosts — rather than preflighted against a server that
// has never seen the key. The refusal is per call and leaves the cache alone:
// it is a fact about that org, not about the App, so the next org on the
// deployment's GitHub still gets its one preflight.
func TestDeploymentAppSource_AnotherGitHubRefusesWithoutAsking(t *testing.T) {
	src, preflights, _ := stubbedSource(t, testDeploymentApp(t))
	ctx := context.Background()

	state, err := src.resolve(ctx, "https://ghe.other.test")
	if state != nil {
		t.Error("an org on another GitHub resolved a state")
	}
	if !errors.Is(err, ErrDeploymentAppUnavailable) || !errors.Is(err, ErrDeploymentAppOtherGitHub) {
		t.Errorf("err = %v; want ErrDeploymentAppUnavailable naming the other GitHub", err)
	}
	for _, want := range []string{"https://ghe.other.test", deploymentTestHost} {
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("err = %v; want it to name %q", err, want)
		}
	}
	if got := atomic.LoadInt32(preflights); got != 0 {
		t.Errorf("preflight ran %d times for an org on another GitHub; want 0", got)
	}

	if _, err := src.resolve(ctx, deploymentTestHost); err != nil {
		t.Fatalf("resolve on the deployment's GitHub after a refusal: %v", err)
	}
	if got := atomic.LoadInt32(preflights); got != 1 {
		t.Errorf("preflight ran %d times for the deployment's GitHub; want 1 — the refusal must not have cached anything", got)
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
			if _, err := src.resolve(context.Background(), deploymentTestHost); err != nil {
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

// TestDeploymentAppSource_LeaderCancellationDoesNotPoisonTheFlight pins the
// property that makes the singleflight safe to share: the caller that happens
// to lead a flight is an accident of scheduling, and its context must not
// decide anyone else's outcome.
//
// Two halves, and the second is the one that bites under load. A caller that
// gives up gets its OWN error — not a verdict on the deployment App, which
// nothing has established. And the flight it abandoned still finishes and still
// caches, so the next caller reads an answer instead of starting another flight
// for the next leader to abandon. Without that, every resolution during a burst
// of short-deadline callers stampedes, and the cache never warms.
func TestDeploymentAppSource_LeaderCancellationDoesNotPoisonTheFlight(t *testing.T) {
	src, preflights, _ := stubbedSource(t, testDeploymentApp(t))
	entered := make(chan struct{}, 1)
	release := make(chan struct{})
	inner := src.preflight
	src.preflight = func(ctx context.Context, m *githubapp.Minter) (githubapp.DeploymentAppIdentity, error) {
		entered <- struct{}{}
		<-release
		return inner(ctx, m)
	}

	leaderCtx, cancel := context.WithCancel(context.Background())
	leaderErr := make(chan error, 1)
	go func() {
		_, err := src.resolve(leaderCtx, deploymentTestHost)
		leaderErr <- err
	}()

	<-entered // the flight is in GitHub, holding
	cancel()  // and its leader walks away

	// The leader returns promptly rather than waiting out the flight it no
	// longer cares about, and says what happened to IT.
	select {
	case err := <-leaderErr:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("leader err = %v; want its own context error", err)
		}
		if errors.Is(err, ErrDeploymentAppUnavailable) {
			t.Error("a cancelled caller was told the deployment App is unavailable; nothing established that, and the next resolution is about to succeed")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a cancelled caller stayed blocked on the flight it abandoned")
	}

	close(release) // GitHub answers the flight nobody is waiting on

	// The answer is there for whoever comes next, off the same single preflight.
	state, err := src.resolve(context.Background(), deploymentTestHost)
	if err != nil {
		t.Fatalf("resolve after the leader left: %v", err)
	}
	if state == nil || state.identity.Slug != deploymentSlug {
		t.Fatalf("state = %+v; want the established identity", state)
	}
	if got := atomic.LoadInt32(preflights); got != 1 {
		t.Errorf("preflight ran %d times; want 1 — the abandoned flight established the answer rather than being thrown away", got)
	}
}

// TestDeploymentAppSource_WaiterKeepsItsOwnDeadline is the same rule from the
// other side: a caller with a tight deadline coalesced behind a slow flight
// fails on its own terms, and takes nobody with it.
func TestDeploymentAppSource_WaiterKeepsItsOwnDeadline(t *testing.T) {
	src, preflights, _ := stubbedSource(t, testDeploymentApp(t))
	entered := make(chan struct{}, 1)
	release := make(chan struct{})
	inner := src.preflight
	src.preflight = func(ctx context.Context, m *githubapp.Minter) (githubapp.DeploymentAppIdentity, error) {
		entered <- struct{}{}
		<-release
		return inner(ctx, m)
	}

	leaderErr := make(chan error, 1)
	go func() {
		_, err := src.resolve(context.Background(), deploymentTestHost)
		leaderErr <- err
	}()
	<-entered

	// A second caller joins the flight with almost no time left.
	waiterCtx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, err := src.resolve(waiterCtx, deploymentTestHost); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("waiter err = %v; want its own deadline", err)
	}

	close(release)
	if err := <-leaderErr; err != nil {
		t.Errorf("the leader failed after an impatient waiter gave up: %v", err)
	}
	if got := atomic.LoadInt32(preflights); got != 1 {
		t.Errorf("preflight ran %d times; want 1", got)
	}
}

// TestDeploymentAppSource_UnconfiguredRefusesWithoutAsking pins that a
// deployment with no shared App refuses immediately. There is nothing to ask
// GitHub about, and asking would spend a round trip per resolution on a
// question whose answer is in the process's own configuration.
func TestDeploymentAppSource_UnconfiguredRefusesWithoutAsking(t *testing.T) {
	src, preflights, _ := stubbedSource(t, githubapp.DeploymentApp{})

	state, err := src.resolve(context.Background(), deploymentTestHost)
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
