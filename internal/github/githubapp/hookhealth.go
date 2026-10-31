package githubapp

import (
	"context"
	"errors"
	"net/url"
	"strings"
	"time"
)

// Is this deployment actually receiving the App's webhooks?
//
// The question has to be asked, not waited for. A registered, active App can
// sit there receiving nothing — its webhook never configured, or pointed at
// some other host, or delivering into a receiver that refuses every one — and
// from inside TF all three look exactly like an App whose events simply
// haven't fired yet. Nothing about the stored registration distinguishes them:
// a stored webhook secret does not mean GitHub is sending anywhere near us,
// and its absence does not mean nobody is sending.
//
// Two App-JWT reads settle it. Where GitHub is sending (hook config) and what
// happened when it did (hook deliveries) are enough to separate every reachable
// case, and neither needs a delivery to have arrived.

// PlaceholderHookURL is the inert hook URL TF's own manifest writes for a
// deployment GitHub cannot reach. GitHub's manifest flow rejects a blank hook
// url outright, so the manifest always emits the block; a local / NAT'd
// deployment gets this IANA-reserved address with active:false instead of an
// address that would only ever fail. Recognizing our own sentinel here is what
// turns "registered an App on a laptop" from an undiagnosable silence into a
// named, correct state.
const PlaceholderHookURL = "https://example.com/triage-factory-webhook-unconfigured"

// hookHealthPageSize is how many recent deliveries the probe reads. Only the
// newest one decides the state, but a page costs the same round trip and
// leaves room for a caller that wants more context.
const hookHealthPageSize = 30

// WebhookState is the answer to "is this deployment receiving this App's
// webhooks", in the form the Settings panel renders.
type WebhookState string

const (
	// WebhookStateNotConfigured — the App has no webhook, or has the
	// placeholder TF writes for an unreachable deployment. A state, not a
	// fault: it is the NORMAL and correct answer in local mode, where
	// installations are discovered by periodic sync instead.
	WebhookStateNotConfigured WebhookState = "not_configured"

	// WebhookStateElsewhere — the App has a webhook, and it is not this
	// deployment's receiver. Deliveries are going somewhere; they are not
	// coming here.
	WebhookStateElsewhere WebhookState = "pointing_elsewhere"

	// WebhookStateRejected — GitHub is delivering to this deployment's
	// receiver and the receiver is not accepting: a missing or mismatched
	// webhook secret (401), a URL this deployment does not serve (404), or a
	// failure to connect at all (status code 0).
	WebhookStateRejected WebhookState = "delivering_rejected"

	// WebhookStateNoDeliveries — the webhook points here and GitHub has
	// recorded no delivery in its 30-day retention window. Ordinary for a
	// freshly installed or quiet App, and deliberately NOT folded into
	// healthy: "healthy" with no evidence behind it is the exact illusion
	// this probe exists to remove.
	WebhookStateNoDeliveries WebhookState = "no_deliveries"

	// WebhookStateHealthy — the webhook points here and the most recent
	// delivery was accepted.
	WebhookStateHealthy WebhookState = "healthy"

	// WebhookStateUnavailable — this GitHub does not serve the App-webhook
	// endpoints (GHES below 3.2 has no deliveries endpoint at all), so the
	// question cannot be answered here. Not a claim about the webhook.
	WebhookStateUnavailable WebhookState = "unavailable"
)

// WebhookHealth is one probe's answer.
//
// HookHost is the origin (scheme://host) of the configured URL rather than the
// whole URL: naming the host is what the "points somewhere else" case needs,
// and the status payload it rides on is readable by every org member. It is
// empty when no webhook is configured.
//
// LastDeliveryStatusCode is what the receiving endpoint answered on the most
// recent delivery — 0 when GitHub could not connect. It is carried rather than
// interpreted so the panel can name the likely cause (a 401 reads differently
// from a connection failure) without this package owning user-facing copy.
type WebhookHealth struct {
	State                  WebhookState
	HookHost               string
	SecretConfigured       bool
	LastDeliveryAt         time.Time
	LastDeliveryStatusCode int
}

// ProbeWebhookHealth resolves the App's webhook state against expectedURL —
// the receiver URL this deployment serves for the org that owns the App.
//
// It costs one App-JWT read in the cases decided by configuration alone (no
// webhook, or one pointing elsewhere) and two when the URL matches and only
// delivery outcomes can separate healthy from rejected. Nothing here writes,
// mints an installation token, or touches the receiver.
//
// An empty expectedURL is refused rather than guessed at: without knowing
// which URL is ours, "points elsewhere" and "points here" are the same
// observation, and answering either way would be an invention. Callers that
// have no deployment identity configured should not probe at all.
//
// A transport failure or an unexpected status propagates. Callers are expected
// to treat that as "no new answer" and keep whatever they last knew — a GitHub
// outage is not evidence that anyone's webhooks are broken.
func (m *Minter) ProbeWebhookHealth(ctx context.Context, expectedURL string) (WebhookHealth, error) {
	if strings.TrimSpace(expectedURL) == "" {
		return WebhookHealth{}, errors.New("githubapp: expectedURL is required to probe webhook health")
	}

	cfg, err := m.HookConfig(ctx)
	if errors.Is(err, ErrHookAPIUnavailable) {
		// 404 is not a documented response for this endpoint, so it means one
		// of two things: this App has no webhook configuration at all, or this
		// GitHub does not serve the endpoint. One more call splits them —
		// whether the OTHER App-webhook endpoint answers is a property of the
		// host, not of the App. Deliberately asked rather than assumed: the
		// two readings want opposite states, and picking one blind is how a
		// diagnostic starts lying.
		if _, derr := m.ListHookDeliveries(ctx, HookDeliveryQuery{PerPage: 1}); derr != nil {
			if errors.Is(derr, ErrHookAPIUnavailable) {
				return WebhookHealth{State: WebhookStateUnavailable}, nil
			}
			return WebhookHealth{}, derr
		}
		return WebhookHealth{State: WebhookStateNotConfigured}, nil
	}
	if err != nil {
		return WebhookHealth{}, err
	}

	health := WebhookHealth{SecretConfigured: cfg.SecretSet}
	configured := strings.TrimSpace(cfg.URL)
	if configured == "" || sameWebhookURL(configured, PlaceholderHookURL) {
		health.State = WebhookStateNotConfigured
		return health, nil
	}
	health.HookHost = urlOrigin(configured)
	if !sameWebhookURL(configured, expectedURL) {
		health.State = WebhookStateElsewhere
		return health, nil
	}

	// The webhook points at this deployment's receiver, so whether anything is
	// getting through is now a question about deliveries, not configuration.
	deliveries, err := m.ListHookDeliveries(ctx, HookDeliveryQuery{PerPage: hookHealthPageSize})
	if errors.Is(err, ErrHookAPIUnavailable) {
		// The URL is right and this host keeps no delivery history (GHES <
		// 3.2). Reporting healthy would be asserting something never observed.
		health.State = WebhookStateUnavailable
		return health, nil
	}
	if err != nil {
		return WebhookHealth{}, err
	}

	latest, ok := latestDelivery(deliveries)
	if !ok {
		health.State = WebhookStateNoDeliveries
		return health, nil
	}
	health.LastDeliveryAt = latest.DeliveredAt
	health.LastDeliveryStatusCode = latest.StatusCode
	if latest.StatusCode >= 200 && latest.StatusCode < 300 {
		health.State = WebhookStateHealthy
	} else {
		// The most recent attempt is what matters, not the best one in the
		// window: an org whose secret was rotated an hour ago has plenty of
		// 2xx history and is receiving nothing now.
		health.State = WebhookStateRejected
	}
	return health, nil
}

// latestDelivery returns the most recently delivered entry. GitHub already
// orders the listing newest-first, but the state hangs on this being the
// newest, so it is established from delivered_at rather than from position.
// Entries with no timestamp are skipped — an undated delivery cannot be shown
// as "last delivered" and cannot be compared against one that can.
func latestDelivery(ds []HookDelivery) (HookDelivery, bool) {
	var best HookDelivery
	var found bool
	for _, d := range ds {
		if d.DeliveredAt.IsZero() {
			continue
		}
		if !found || d.DeliveredAt.After(best.DeliveredAt) {
			best, found = d, true
		}
	}
	return best, found
}

// sameWebhookURL reports whether two webhook URLs address the same endpoint.
// Scheme and host compare case-insensitively (both are case-insensitive per
// RFC 3986) and one trailing slash on the path is ignored; everything else is
// exact, because the path carries the org id and a mismatch there is exactly
// the "someone else's receiver" case this exists to catch. Unparseable input
// falls back to a trimmed string comparison rather than claiming a match.
func sameWebhookURL(a, b string) bool {
	ua, erra := url.Parse(strings.TrimSpace(a))
	ub, errb := url.Parse(strings.TrimSpace(b))
	if erra != nil || errb != nil {
		return strings.TrimSpace(a) == strings.TrimSpace(b)
	}
	if !strings.EqualFold(ua.Scheme, ub.Scheme) || !strings.EqualFold(ua.Host, ub.Host) {
		return false
	}
	return strings.TrimSuffix(ua.Path, "/") == strings.TrimSuffix(ub.Path, "/") &&
		ua.RawQuery == ub.RawQuery
}

// urlOrigin returns "scheme://host" for a webhook URL, or "" when it cannot be
// parsed. The path is dropped deliberately: the panel names where deliveries
// are going, and the host is the whole of that answer.
func urlOrigin(raw string) string {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Host == "" {
		return ""
	}
	return u.Scheme + "://" + u.Host
}
