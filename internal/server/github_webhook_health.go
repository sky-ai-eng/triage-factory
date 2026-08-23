package server

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/github/ghbase"
	"github.com/sky-ai-eng/triage-factory/internal/githubapp"
	"github.com/sky-ai-eng/triage-factory/internal/server/httpx"
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
//
// fingerprint is which REGISTRATION the answer describes, and it is the reason
// this cache is safe across an App swap. The map is keyed by org, but the thing
// probed is the org's App — tear one down and register another (switch to a PAT
// and back, discard a staged App and import a different one) and the org id is
// unchanged while every answer about the old App is now a statement about
// something the workspace no longer has. Both the read and the write compare it,
// which covers the two ways a stale answer could be served: a reader finding the
// previous App's entry, and a probe that was already in flight when the
// registration changed landing afterwards and being marked fresh for a full TTL.
type webhookHealthEntry struct {
	fingerprint string
	health      githubapp.WebhookHealth
	have        bool
	checkedAt   time.Time
	nextProbe   time.Time
	probing     bool
}

// appFingerprint identifies one registration for the cache above: which App, and
// which registration of it.
//
// RegisteredAt is what separates two registrations of the SAME App id, which is
// not a hypothetical — discarding an App and importing it again with the webhook
// secret that was missing the first time produces exactly that, and it is the
// case where a stale "delivering, rejected" would be most misleading. The App id
// alone cannot see it; the stored webhook secret changes with the row, not with
// the App.
//
// Deliberately not a hash: this never leaves the process and only ever has to
// compare equal to itself.
func appFingerprint(app *domain.OrgGitHubApp) string {
	if app == nil {
		return ""
	}
	return app.AppID + "\x00" + app.RegisteredAt.UTC().Format(time.RFC3339Nano)
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
	fingerprint := appFingerprint(app)

	s.webhookHealthMu.Lock()
	if s.webhookHealth == nil {
		s.webhookHealth = make(map[string]*webhookHealthEntry)
	}
	e, ok := s.webhookHealth[orgID]
	if !ok || e.fingerprint != fingerprint {
		// A different registration than the one the entry describes: start
		// clean rather than answer for the App this org used to have. The
		// reader gets nil — "not known yet" — and a probe for the current App
		// starts now. Replacing the entry also orphans any probe still running
		// for the old App; its write is dropped on the same comparison.
		e = &webhookHealthEntry{fingerprint: fingerprint}
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
//
// A probe runs outside the lock, so the org's registration can change while one
// is in flight. That answer describes the App the org had when the probe
// started, so it is dropped rather than written — otherwise the swap would be
// followed by the OLD App's state, stamped fresh, for a full TTL. This is why
// the App lifecycle paths need no invalidate call of their own: the entry is
// scoped to a registration, so a new one misses by construction instead of by
// remembering.
func (s *Server) refreshWebhookHealth(ctx context.Context, orgID string, app *domain.OrgGitHubApp, expectedURL string) {
	fingerprint := appFingerprint(app)
	health, err := s.probeWebhookHealth(ctx, orgID, app, expectedURL)
	now := time.Now()

	s.webhookHealthMu.Lock()
	defer s.webhookHealthMu.Unlock()
	if s.webhookHealth == nil {
		s.webhookHealth = make(map[string]*webhookHealthEntry)
	}
	e, ok := s.webhookHealth[orgID]
	if !ok {
		e = &webhookHealthEntry{fingerprint: fingerprint}
		s.webhookHealth[orgID] = e
	} else if e.fingerprint != fingerprint {
		// The registration changed while this probe was running. Its answer is
		// about an App this org no longer has — and the entry now holding the
		// org's slot belongs to the current one, including its own probing
		// flag, so nothing here is ours to clear.
		return
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

// invalidateWebhookHealth makes the org's next status read re-probe, keeping the
// current answer until one lands. Its caller is the replay action, whose whole
// point is to change what the next probe would see.
//
// The App lifecycle paths (register, import, discard, switch-to-PAT) do NOT call
// this, unlike their invalidateWebhookSecret neighbour, and the difference is
// deliberate. That cache is keyed by org alone, so a rotation has to be pushed
// at it; this one keys its entry to the registration (appFingerprint), so a
// swapped App misses the cache without anyone remembering to say so — including
// on the path an explicit invalidate would still get wrong, where a probe
// already in flight lands after the swap.
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
		APIBase:    ghbase.APIBase(base),
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
		httpx.WriteErrors(w, http.StatusBadGateway, httpx.ErrorItem{Reason: httpx.ReasonUpstreamUnavailable, Message: "Could not authenticate as the GitHub App." + localDetail(err)})
		return
	}

	deliveries, err := minter.ListHookDeliveries(ctx, githubapp.HookDeliveryQuery{
		Status: "failure", PerPage: replayDeliveryPageSize, MaxPages: replayDeliveryPages,
	})
	if errors.Is(err, githubapp.ErrHookAPIUnavailable) {
		// A capability gap, not an outage: GitHub Enterprise Server below 3.2
		// has no delivery history to replay from. Named plainly so an operator
		// stops looking for the button that does not exist here.
		httpx.WriteErrors(w, http.StatusBadGateway, httpx.ErrorItem{Reason: httpx.ReasonUpstreamUnavailable, Message: "This GitHub host doesn't expose App webhook deliveries, so they can't be replayed."})
		return
	}
	if err != nil {
		githubAppLog.Error("webhook replay: list deliveries failed", "org", orgID, "error", err)
		httpx.WriteErrors(w, http.StatusBadGateway, httpx.ErrorItem{Reason: httpx.ReasonUpstreamUnavailable, Message: "Could not read the App's webhook deliveries from GitHub." + localDetail(err)})
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
