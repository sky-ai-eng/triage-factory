package github

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"golang.org/x/sync/singleflight"

	"github.com/sky-ai-eng/triage-factory/internal/github/ghbase"
	"github.com/sky-ai-eng/triage-factory/internal/githubapp"
)

// Tier 2 — the deployment App. One App key serving many workspaces, its
// identity and key in the operator's environment rather than in any org's
// secret store (see githubapp.DeploymentApp for why the environment).
//
// A managed org has no org_github_apps row and cannot have one, so everything
// the BYO tier reads off that row has to come from somewhere else. Two of the
// three answers arrive from GitHub itself: GET /app, authenticated with a JWT
// the App signs for itself, reports the slug and the client id, which is why
// the operator configures four values and not six. The third — the bot
// account's numeric id, for the commit email that links a bot's commits on
// github.com — is a second, public read alongside it.
//
// That preflight is also a gate: it is where the members permission is
// asserted, and a deployment App that fails it must serve nobody. It runs on
// FIRST USE of the deployment credential rather than at boot, because a GitHub
// blip must never refuse to start the deployment — and its answer is cached, so
// the cost is one pair of reads per host per TTL rather than one per
// resolution.

// ErrDeploymentAppUnavailable is returned when a managed-class org's resolution
// cannot use the deployment App: none is configured on this deployment, or its
// preflight did not come back clean (GitHub rejected the key pair, GitHub did
// not answer, or the App is not granted the organization members permission —
// the cause is wrapped and names which).
//
// Like ErrUnknownCredentialClass it deliberately does NOT wrap
// ErrNoGitHubCredentials. "GitHub is not configured" is a claim about the
// workspace, and every one of these faults is a claim about the DEPLOYMENT: the
// workspace did nothing wrong and has nothing to reconnect, so sending its
// admin to re-authorize would be sending them to fix somebody else's .env.
//
// What it never means is "fall back". A managed org's resolution mints from the
// deployment App or fails — never from a PAT, which would be a credential the
// workspace did not choose.
var ErrDeploymentAppUnavailable = errors.New("github: the deployment app is unavailable")

// ErrDeploymentAppOtherGitHub is the cause wrapped when a managed-class org's
// effective GitHub host is not the deployment's default — the one GitHub the
// deployment App is registered on. Such an org is refused before any network
// call: preflighting the deployment's key against a GitHub that has never seen
// it would fail with a 401 that reads like a bad key, and cache that failure
// against the deployment App itself. The bind ceremony refuses the same
// workspace with the same fact, so this arm is reached only by an org whose
// base URL moved after it bound.
var ErrDeploymentAppOtherGitHub = errors.New("github: the workspace is pointed at a GitHub the deployment app is not on")

// deploymentAppTTL is how long a clean preflight answer serves. The answer
// moves when the operator rotates the App's key or its granted permissions
// change on GitHub — the first restarts the process anyway, and the second is
// rare and wants noticing within minutes rather than instantly.
const deploymentAppTTL = 5 * time.Minute

// deploymentAppFailureTTL is the same for a failed one, and it is deliberately
// much shorter. Caching the failure at all is what keeps a GitHub outage from
// turning every org's every resolution into another GET /app; keeping the
// window small is what keeps a fixed key or a granted permission from waiting
// out a full TTL before anything works again.
const deploymentAppFailureTTL = 30 * time.Second

// deploymentAppEstablishTimeout bounds one flight's two reads. It is a
// backstop rather than the working limit — both underlying HTTP clients cap a
// request at 30s — and it exists because the flight runs detached from every
// caller's context (see resolve), so nothing else would ever cut it short.
//
// Unlike a caller giving up, this expiring IS an answer: GitHub did not respond
// in a minute, which is what ErrDeploymentAppUnreachable means, and it caches
// for the failure window like any other.
const deploymentAppEstablishTimeout = 60 * time.Second

// deploymentAppState is what a completed preflight establishes about the
// deployment App: the identity GitHub reports, plus the bot account's numeric
// id when it could be read.
//
// BotUserID is 0 for "unknown", which is a supported state and not a gap to
// retry into: it degrades the commit email to the plain "<slug>[bot]@…" form,
// exactly as an org_github_apps row with a NULL bot_user_id does on the BYO
// path. A run must never fail for want of it.
type deploymentAppState struct {
	identity  githubapp.DeploymentAppIdentity
	botUserID int64
}

// deploymentAppEntry is the cached preflight outcome. err and state are
// alternatives: a cached failure serves the same error to every caller inside
// its window, which is the whole point of caching one.
type deploymentAppEntry struct {
	state     *deploymentAppState
	err       error
	expiresAt time.Time
}

// deploymentAppSource resolves the deployment App, running the preflight at
// most once per TTL.
//
// One answer, not one per host. The App's identity is established BY a call to
// a particular GitHub, and the deployment App is registered on exactly one —
// the deployment's default (ghbase.DefaultBaseURL), which is also the GitHub
// every managed org is on, since the bind refuses a workspace pointed anywhere
// else. So there is one GitHub to ask and one verdict to hold; an org whose
// host is not that GitHub is refused by resolve without a flight rather than
// given a per-host entry that could only ever cache a failure.
type deploymentAppSource struct {
	app githubapp.DeploymentApp

	// preflight and botUserID are the two network reads, injectable so the
	// caching, the failure arms and the bot-id degradation are all testable
	// without a live GitHub.
	preflight func(ctx context.Context, m *githubapp.Minter) (githubapp.DeploymentAppIdentity, error)
	botUserID func(ctx context.Context, base, login string) (int64, error)

	// now is injectable for tests; production leaves it nil → time.Now.
	now func() time.Time

	ttl              time.Duration
	failureTTL       time.Duration
	establishTimeout time.Duration

	// group coalesces concurrent misses into a single preflight. Without it a
	// cold cache under a poll cycle's fan-out would spend one GET /app per
	// org, all establishing the same fact.
	group singleflight.Group

	mu    sync.Mutex
	entry *deploymentAppEntry
}

// deploymentAppFlightKey is the singleflight key: there is one question, so
// there is one key.
const deploymentAppFlightKey = "deployment-app"

func newDeploymentAppSource(app githubapp.DeploymentApp) *deploymentAppSource {
	return &deploymentAppSource{
		app:              app,
		preflight:        githubapp.PreflightDeploymentApp,
		botUserID:        fetchBotUserID,
		ttl:              deploymentAppTTL,
		failureTTL:       deploymentAppFailureTTL,
		establishTimeout: deploymentAppEstablishTimeout,
	}
}

func (s *deploymentAppSource) timeNow() time.Time {
	if s.now != nil {
		return s.now()
	}
	return time.Now()
}

// configured reports whether this deployment has a deployment App at all. It
// answers without a network call, so a deployment with none refuses a managed
// org's resolution immediately rather than after a round trip.
func (s *deploymentAppSource) configured() bool { return s.app.Configured() }

// resolve returns the deployment App's established state for an org on the
// deployment's GitHub, or an error — never (nil, nil). base is the org's
// resolved web base, and it is checked rather than used: an org whose host is
// not the deployment default is refused here with ErrDeploymentAppOtherGitHub,
// before any flight, because the deployment App is on one GitHub and that org
// is on another. Every failure arm that says something about the App is
// ErrDeploymentAppUnavailable with the cause wrapped, so the one thing a caller
// can do with such a failure is refuse, which is the one thing it may do. The
// single arm that is NOT is this caller's own context ending, which is a fact
// about the call rather than about the App.
//
// Either way there is no third answer: a resolution that does not establish the
// App fails, and no path here reaches a PAT.
func (s *deploymentAppSource) resolve(ctx context.Context, base string) (*deploymentAppState, error) {
	if !s.configured() {
		return nil, fmt.Errorf("%w: %w", ErrDeploymentAppUnavailable, githubapp.ErrNoDeploymentApp)
	}
	deploymentHost := ghbase.DefaultBaseURL()
	if host := ghbase.ResolveBaseURL(base); host != deploymentHost {
		return nil, fmt.Errorf("%w: %w: workspace on %s, deployment app on %s",
			ErrDeploymentAppUnavailable, ErrDeploymentAppOtherGitHub, host, deploymentHost)
	}
	apiBase := ghbase.APIBase(deploymentHost)
	if state, err, ok := s.cached(); ok {
		return state, err
	}

	// One flight, and its outcome is shared by everyone waiting on it — correct
	// here, because they are all asking the same question of the same GitHub
	// and there is one answer.
	type result struct {
		state *deploymentAppState
		err   error
	}
	ch := s.group.DoChan(deploymentAppFlightKey, func() (any, error) {
		// A second look under the flight: the caller this one coalesced behind
		// may already have stored an answer.
		if state, err, ok := s.cached(); ok {
			return result{state: state, err: err}, nil
		}
		// The flight runs on a context of its OWN — detached from whichever
		// caller happened to lead it, and bounded by its own deadline. Two
		// things follow and both are the point.
		//
		// Whoever arrived first is an accident of scheduling, so their context
		// must not decide anyone else's outcome. Under the leader's context, one
		// abandoned request or one caller with a tight deadline would fail every
		// resolution coalesced behind it, with an error about a context those
		// callers never held.
		//
		// And the work has to finish even when its leader leaves. A flight that
		// dies with its leader leaves the cache cold, so the next wave stampedes
		// into a new flight that its own leader can abandon in turn — a cache
		// that never warms precisely when load is highest. Detached, one
		// preflight establishes the answer for everyone, including the callers
		// still on their way.
		//
		// WithoutCancel rather than Background: the values ride along, so the
		// call stays inside the leader's trace instead of opening a rootless
		// span nobody goes looking for.
		workCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), s.establishTimeout)
		defer cancel()
		state, err := s.establish(workCtx, deploymentHost, apiBase)
		s.store(state, err)
		return result{state: state, err: err}, nil
	})

	select {
	case out := <-ch:
		res := out.Val.(result)
		return res.state, res.err
	case <-ctx.Done():
		// THIS caller gave up. The flight carries on for everyone else, and the
		// error says what actually happened rather than passing a verdict on the
		// App: nothing here establishes that the deployment App is unusable, so
		// it is deliberately not ErrDeploymentAppUnavailable, and the answer this
		// flight is still on its way to caching is what the next resolution
		// finds.
		return nil, fmt.Errorf("resolve deployment app for %s: %w", apiBase, ctx.Err())
	}
}

// establish runs the two reads that turn the configured App into a usable one.
//
// The preflight decides; the bot-id read only decorates. A failed bot lookup is
// logged and left at 0 rather than failing the resolution, because the value it
// feeds is a commit email that has a working fallback form — refusing a whole
// run over the cosmetics of a commit's author link would be the wrong trade in
// the wrong direction.
func (s *deploymentAppSource) establish(ctx context.Context, base, apiBase string) (*deploymentAppState, error) {
	minter, err := s.app.Minter(apiBase)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrDeploymentAppUnavailable, err)
	}
	identity, err := s.preflight(ctx, minter)
	if err != nil {
		ghResolverLog.Warn("deployment app preflight failed; managed-class orgs resolve nothing on this host",
			"api_base", apiBase, "error", err)
		return nil, fmt.Errorf("%w: %w", ErrDeploymentAppUnavailable, err)
	}

	state := &deploymentAppState{identity: identity}
	if identity.Slug != "" {
		botLogin := identity.Slug + "[bot]"
		id, berr := s.botUserID(ctx, base, botLogin)
		if berr != nil {
			ghResolverLog.Warn("resolve deployment app bot user id failed; commit email falls back to the plain noreply form",
				"bot_login", botLogin, "error", berr)
		} else {
			state.botUserID = id
		}
	}
	return state, nil
}

// cached returns the stored outcome when it has not expired. The third value
// distinguishes a miss from a cached failure, which is an answer.
func (s *deploymentAppSource) cached() (*deploymentAppState, error, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e := s.entry
	if e == nil || s.timeNow().After(e.expiresAt) {
		return nil, nil, false
	}
	return e.state, e.err, true
}

// store writes the outcome with the TTL its kind earns.
func (s *deploymentAppSource) store(state *deploymentAppState, err error) {
	ttl := s.ttl
	if err != nil {
		ttl = s.failureTTL
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.entry = &deploymentAppEntry{state: state, err: err, expiresAt: s.timeNow().Add(ttl)}
}

// fetchBotUserID reads the numeric account id of an App's bot ("<slug>[bot]")
// from the public GET /users/{login} on the org's GitHub base.
//
// Unauthenticated, exactly as the BYO path's registration-time lookup is: the
// endpoint is public, and the alternative — an installation token — would mean
// choosing an installation to answer a question about a global bot account that
// belongs to none of them.
func fetchBotUserID(ctx context.Context, base, login string) (int64, error) {
	return NewClient(base, "").UserID(ctx, login)
}
