package server

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
	ghclient "github.com/sky-ai-eng/triage-factory/internal/github"
	"github.com/sky-ai-eng/triage-factory/internal/githubapp"
)

// An org can hold a registered, active GitHub App that receives no webhook
// deliveries at all, and nothing in TF's own state says so — a blank webhook
// secret, an App whose owner never registered this deployment's URL, and the
// inert placeholder a non-public deployment writes into its manifest all look
// identical from in here. What degrades is the installation mirror, and in
// multi mode the mirror has no other maintainer: polling stops org-wide, the
// repo picker dead-ends, credential resolution misfires.
//
// This file is the diagnosis. githubapp.ProbeWebhookHealth asks GitHub the two
// questions that settle it; the cache below is what keeps that off the critical
// path, and handleGitHubAppWebhookReplay is the one repair it offers.
//
// Three properties hold, and every choice here follows from them:
//
//   - The status read never waits on GitHub. A probe runs in the background and
//     the reader is served whatever is known, including nothing.
//   - A probe failure is not an answer. The last known state survives it, so a
//     GitHub outage never renders as "your webhooks are broken".
//   - Nothing here mints an installation token or touches a delivery.

const (
	// webhookHealthTTL is how long a successful probe answers for. Webhook
	// configuration changes on GitHub, not here, so there is no local event to
	// invalidate on — this is deliberately slow: the states it separates are
	// standing conditions, not transients, and two App-JWT round trips per org
	// per interval is the whole cost.
	webhookHealthTTL = 10 * time.Minute

	// webhookHealthRetryTTL is the backoff after a failed probe. Shorter than
	// the success TTL (a failure leaves the panel on stale information) but far
	// from immediate, so a GitHub outage plus a Settings page left open cannot
	// turn into a retry loop.
	webhookHealthRetryTTL = 2 * time.Minute

	// webhookProbeTimeout bounds one background probe. It is not on any
	// request's path, so this only stops a hung GitHub connection from pinning
	// the entry's in-flight flag indefinitely.
	webhookProbeTimeout = 20 * time.Second
)

// webhookHealthEntry is one org's cached probe result plus its schedule.
//
// health/have are kept separate from the schedule on purpose: a failed probe
// moves nextProbe and nothing else, which is exactly how the last known answer
// survives a GitHub outage.
type webhookHealthEntry struct {
	health    githubapp.WebhookHealth
	have      bool
	checkedAt time.Time
	nextProbe time.Time
	probing   bool
}

// githubAppWebhookHealth is the status DTO's webhook-health block. Null when
// the org has no App, when the deployment has no public identity to compare a
// hook URL against, or before the first probe has answered — "not yet known" is
// an absent field rather than a state, since a state would be a claim.
//
// The block is facts, not copy: the panel names the likely cause from the
// status code (a 401 reads differently from a failure to connect), and the
// secret's VALUE is never carried in any form — secret_configured is GitHub's
// masked presence bit and nothing more.
type githubAppWebhookHealth struct {
	State                  string `json:"state"`
	HookHost               string `json:"hook_host"`
	SecretConfigured       bool   `json:"secret_configured"`
	LastDeliveryAt         string `json:"last_delivery_at"`
	LastDeliveryStatusCode int    `json:"last_delivery_status_code"`
	CheckedAt              string `json:"checked_at"`
}

// webhookHealthDTO returns the org's last known webhook health for the status
// payload, kicking a background refresh when the cached answer is due. It never
// blocks on GitHub and never fails: nil means "nothing known yet", which the
// panel renders as nothing at all.
//
// Returns nil without probing when there is no App (nothing to ask about) or no
// deployment identity (no receiver URL to compare against — see
// ProbeWebhookHealth's refusal to guess).
func (s *Server) webhookHealthDTO(ctx context.Context, orgID string, app *domain.OrgGitHubApp) *githubAppWebhookHealth {
	if app == nil || s.deployCfg == nil {
		return nil
	}
	expectedURL := s.webhookReceiverURL(orgID)

	s.webhookHealthMu.Lock()
	if s.webhookHealth == nil {
		s.webhookHealth = make(map[string]*webhookHealthEntry)
	}
	e, ok := s.webhookHealth[orgID]
	if !ok {
		e = &webhookHealthEntry{}
		s.webhookHealth[orgID] = e
	}
	due := !e.probing && !time.Now().Before(e.nextProbe)
	if due {
		// Claimed under the lock so a burst of Settings polls produces one
		// probe rather than one per reader.
		e.probing = true
	}
	health, have, checkedAt := e.health, e.have, e.checkedAt
	s.webhookHealthMu.Unlock()

	if due {
		// Detached from the request: a reader navigating away must not cancel
		// the probe that would have answered for the next one. WithoutCancel
		// keeps the request's logging/trace values while dropping its deadline.
		probeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), webhookProbeTimeout)
		go func() {
			defer cancel()
			s.refreshWebhookHealth(probeCtx, orgID, app, expectedURL)
		}()
	}
	if !have {
		return nil
	}
	dto := &githubAppWebhookHealth{
		State:                  string(health.State),
		HookHost:               health.HookHost,
		SecretConfigured:       health.SecretConfigured,
		LastDeliveryStatusCode: health.LastDeliveryStatusCode,
		CheckedAt:              checkedAt.UTC().Format(time.RFC3339),
	}
	// "" rather than the zero instant formatted, so a state with no delivery
	// behind it doesn't read as one delivered in year one.
	if !health.LastDeliveryAt.IsZero() {
		dto.LastDeliveryAt = health.LastDeliveryAt.UTC().Format(time.RFC3339)
	}
	return dto
}

// refreshWebhookHealth runs one probe and folds the outcome into the cache.
// Synchronous and callable on its own (the background kick is just a goroutine
// around it), which is what makes the failure behaviour testable.
//
// A failure updates the schedule and NOTHING else. That is the invariant: the
// probe is a diagnosis of the webhook, and a failure to reach GitHub is not a
// diagnosis at all.
func (s *Server) refreshWebhookHealth(ctx context.Context, orgID string, app *domain.OrgGitHubApp, expectedURL string) {
	health, err := s.probeWebhookHealth(ctx, orgID, app, expectedURL)
	now := time.Now()

	s.webhookHealthMu.Lock()
	defer s.webhookHealthMu.Unlock()
	if s.webhookHealth == nil {
		s.webhookHealth = make(map[string]*webhookHealthEntry)
	}
	e, ok := s.webhookHealth[orgID]
	if !ok {
		e = &webhookHealthEntry{}
		s.webhookHealth[orgID] = e
	}
	e.probing = false
	if err != nil {
		// Warn, not error: the common cause is GitHub being briefly
		// unreachable, and this is a diagnostic that failed to run rather than
		// a fault in anything it diagnoses.
		githubAppLog.Warn("github app webhook probe failed; keeping the last known state",
			"org", orgID, "error", err)
		e.nextProbe = now.Add(webhookHealthRetryTTL)
		return
	}
	e.health = health
	e.have = true
	e.checkedAt = now
	e.nextProbe = now.Add(webhookHealthTTL)
}

// probeWebhookHealth resolves the org's App key and asks GitHub. Split from the
// cache so tests can substitute it, and so the one place that reads a private
// key for this purpose is visible on its own.
func (s *Server) probeWebhookHealth(ctx context.Context, orgID string, app *domain.OrgGitHubApp, expectedURL string) (githubapp.WebhookHealth, error) {
	if s.hookProbe != nil {
		return s.hookProbe(ctx, orgID, app, expectedURL)
	}
	minter, err := s.appMinter(ctx, orgID, app)
	if err != nil {
		return githubapp.WebhookHealth{}, err
	}
	return minter.ProbeWebhookHealth(ctx, expectedURL)
}

// invalidateWebhookHealth makes the org's next status read re-probe. Called
// after the replay action, whose whole point is to change what the next probe
// would see.
func (s *Server) invalidateWebhookHealth(orgID string) {
	s.webhookHealthMu.Lock()
	defer s.webhookHealthMu.Unlock()
	if e, ok := s.webhookHealth[orgID]; ok {
		e.nextProbe = time.Time{}
	}
}

// webhookReceiverURL is the absolute URL GitHub must be configured to deliver
// this org's App webhooks to — the same URL buildManifestAndState bakes into a
// reachable deployment's manifest, and the route handleGitHubWebhook serves.
func (s *Server) webhookReceiverURL(orgID string) string {
	return s.deployCfg.publicURL + "/api/webhooks/github/" + orgID
}

// appMinter builds a Minter from the org's stored App PEM + App ID, pinned at
// the org's resolved GitHub API base. The App authenticates as itself, so this
// needs no installation and mints no installation token.
func (s *Server) appMinter(ctx context.Context, orgID string, app *domain.OrgGitHubApp) (*githubapp.Minter, error) {
	base, err := s.ghResolver.BaseURLFor(ctx, orgID)
	if err != nil {
		return nil, fmt.Errorf("resolve github base for org %s: %w", orgID, err)
	}
	return s.appMinterAt(ctx, orgID, base, app)
}

// appMinterAt is appMinter for a caller that has already resolved the org's
// GitHub base URL and would otherwise read it twice.
func (s *Server) appMinterAt(ctx context.Context, orgID, base string, app *domain.OrgGitHubApp) (*githubapp.Minter, error) {
	pem, err := s.secrets.GetSystem(ctx, orgID, app.PEMRef)
	if err != nil {
		return nil, fmt.Errorf("read app pem: %w", err)
	}
	if pem == "" {
		return nil, fmt.Errorf("app pem secret %q not found for org %s", app.PEMRef, orgID)
	}
	key, err := githubapp.ParsePrivateKey([]byte(pem))
	if err != nil {
		return nil, fmt.Errorf("parse app pem: %w", err)
	}
	appID, err := strconv.ParseInt(app.AppID, 10, 64)
	if err != nil {
		return nil, fmt.Errorf("parse app id %q: %w", app.AppID, err)
	}
	minter, err := githubapp.NewMinter(githubapp.Config{
		PrivateKey: key,
		AppID:      appID,
		APIBase:    ghclient.APIBase(base),
	})
	if err != nil {
		return nil, fmt.Errorf("init app token minter: %w", err)
	}
	return minter, nil
}

// Replaying what was missed.

const (
	// replayDeliveryPageSize / replayDeliveryPages bound how far back the
	// replay looks. GitHub retains deliveries for 30 days; two pages of 100
	// failures is far more than an org that was hookless for a while will have
	// accumulated in installation events, and it keeps one click from paging
	// through a busy App's entire failed history.
	replayDeliveryPageSize = 100
	replayDeliveryPages    = 2

	// maxReplayAttempts caps the redelivery POSTs one call issues. The mirror
	// converges on the newest state per installation, so replaying the most
	// recent failures is what heals it; a higher cap would only add round trips.
	maxReplayAttempts = 50
)

// replayCandidates picks which deliveries are worth sending again, and reports
// how many there were in total.
//
// Installation events only: the rest of what a hookless window dropped is
// content the pollers re-derive on their own schedule, while the installation
// mirror has no other maintainer between cycles in multi mode — it is the thing
// a rejecting receiver actually corrupts.
//
// The failed-status test is applied here as well as in the query, because the
// query's `status` filter is a github.com parameter that the GHES descriptions
// do not carry — an older host would ignore it and hand back successes too.
// Re-applying one would be harmless (the receiver drops a redelivery of an
// applied delivery on its GUID), but counting it as "missed" would not be.
//
// The list arrives newest-first and the cap keeps the newest, which is the
// right end to keep: the mirror converges on the latest state per installation,
// so an older delivery for the same installation would only be overwritten by
// the newer one anyway.
func replayCandidates(ds []githubapp.HookDelivery, limit int) (attempt []githubapp.HookDelivery, candidates int) {
	for _, d := range ds {
		if d.Event != "installation" {
			continue
		}
		if d.StatusCode >= 200 && d.StatusCode < 300 {
			continue
		}
		candidates++
		if len(attempt) < limit {
			attempt = append(attempt, d)
		}
	}
	return attempt, candidates
}

// githubWebhookReplayResponse reports what the repair actually did. Candidates
// is how many failed installation deliveries were found in the window, which is
// the number that makes "replayed: 0" legible — nothing to replay reads very
// differently from nothing accepted.
type githubWebhookReplayResponse struct {
	Candidates int `json:"candidates"`
	Replayed   int `json:"replayed"`
	Failed     int `json:"failed"`
}

// handleGitHubAppWebhookReplay asks GitHub to redeliver the App's failed
// installation deliveries, healing an installation mirror that went stale while
// the receiver was rejecting them.
//
// This is the repair half of the diagnosis above. An org that imported an App
// without its webhook secret has been 401ing every delivery GitHub sent; the
// installation events among them are the ones that matter, because in multi
// mode the mirror has no other maintainer between poll cycles. Replaying inside
// GitHub's 30-day window applies them for real rather than waiting on the next
// reconcile.
//
// Only failed deliveries are candidates, so a replay cannot re-apply something
// that already worked — and even if it could, the receiver dedups on the
// delivery GUID, which a redelivery shares with its original. That is why this
// is safe to offer as a button.
//
// POST /api/orgs/{org_id}/github/app/webhook/replay
func (s *Server) handleGitHubAppWebhookReplay(w http.ResponseWriter, r *http.Request) {
	orgID, _, ok := s.az.RequireOrgAdmin(w, r)
	if !ok {
		return
	}
	ctx := r.Context()

	// Same gate shape as the installations refresh: a replay only means
	// anything for an org that brought its own App, and a missing registration
	// is a 404 by decision rather than by the accident of a nil row.
	class, err := s.githubCredentialClass(ctx, orgID)
	if err != nil {
		if errors.Is(err, ErrUnknownGitHubCredentialClass) {
			githubAppLog.Error("unknown github credential class on webhook replay", "org", orgID)
		}
		internalError(w, "github-app", err)
		return
	}
	if class != domain.GitHubCredentialClassBYOApp {
		notFound(w, "github app")
		return
	}
	app, err := s.githubApps.GetForOrgSystem(ctx, orgID)
	if err != nil {
		internalError(w, "github-app", err)
		return
	}
	if app == nil {
		notFound(w, "github app")
		return
	}

	minter, err := s.appMinter(ctx, orgID, app)
	if err != nil {
		githubAppLog.Error("webhook replay: build app minter failed", "org", orgID, "error", err)
		writeJSON(w, http.StatusBadGateway, map[string]string{
			"error": "Could not authenticate as the GitHub App." + localDetail(err),
		})
		return
	}

	deliveries, err := minter.ListHookDeliveries(ctx, githubapp.HookDeliveryQuery{
		Status: "failure", PerPage: replayDeliveryPageSize, MaxPages: replayDeliveryPages,
	})
	if errors.Is(err, githubapp.ErrHookAPIUnavailable) {
		// A capability gap, not an outage: GitHub Enterprise Server below 3.2
		// has no delivery history to replay from. Named plainly so an operator
		// stops looking for the button that does not exist here.
		writeJSON(w, http.StatusBadGateway, map[string]string{
			"error": "This GitHub host doesn't expose App webhook deliveries, so they can't be replayed.",
		})
		return
	}
	if err != nil {
		githubAppLog.Error("webhook replay: list deliveries failed", "org", orgID, "error", err)
		writeJSON(w, http.StatusBadGateway, map[string]string{
			"error": "Could not read the App's webhook deliveries from GitHub." + localDetail(err),
		})
		return
	}

	attempt, candidates := replayCandidates(deliveries, maxReplayAttempts)
	out := githubWebhookReplayResponse{Candidates: candidates}
	for _, d := range attempt {
		if rerr := minter.RedeliverHookDelivery(ctx, d.ID); rerr != nil {
			// One refused replay (a delivery aged out of the window between the
			// list and the POST, say) is not the whole repair failing — count it
			// and keep going, so a stale entry can't block the rest.
			githubAppLog.Warn("webhook replay: redelivery refused",
				"org", orgID, "delivery", d.ID, "error", rerr)
			out.Failed++
			continue
		}
		out.Replayed++
	}

	githubAppLog.Info("replayed failed installation webhook deliveries",
		"org", orgID, "candidates", out.Candidates, "replayed", out.Replayed, "failed", out.Failed)

	// GitHub delivers the replays asynchronously, so the next probe is what
	// will show whether they landed — let it run rather than serving the
	// pre-repair answer for the rest of the TTL.
	s.invalidateWebhookHealth(orgID)

	writeJSON(w, http.StatusOK, out)
}
