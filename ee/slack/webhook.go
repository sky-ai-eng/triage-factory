package slack

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	slackstore "github.com/sky-ai-eng/triage-factory/ee/slack/store"
	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/server/httpx"
)

// maxSlackWebhookBody caps the request body read before signature
// verification. Mirrors internal/server/github_webhooks.go's
// maxWebhookBody discipline, scaled down: a Slack event_callback carries
// one message, never a diff-sized payload — 1 MiB is generous.
const maxSlackWebhookBody = 1 << 20

// slackReplayWindow bounds how stale a signed request's timestamp may be —
// Slack's own documented replay-protection window for X-Slack-Request-Timestamp.
const slackReplayWindow = 5 * time.Minute

// slackEventEnvelope is the outer shape of every Events API delivery:
// url_verification carries token/challenge; event_callback carries
// event_id (the dedup key) plus the inner Event payload. APIAppID is
// present at the top level of every delivery — together with TeamID it
// resolves the exact (workspace, app) row a delivery belongs to.
type slackEventEnvelope struct {
	Type      string          `json:"type"`
	TeamID    string          `json:"team_id"`
	APIAppID  string          `json:"api_app_id"`
	Challenge string          `json:"challenge"`
	EventID   string          `json:"event_id"`
	Event     json.RawMessage `json:"event"`
	// EventTime is when Slack generated the event, in unix seconds.
	EventTime int64 `json:"event_time"`
	// MinuteRateLimited is set on a type=app_rate_limited envelope: the unix
	// timestamp of the minute Slack started dropping this app's event
	// deliveries (its 30k events/workspace/hour Events API budget was
	// exhausted — deliveries in excess are never retried later).
	MinuteRateLimited int64 `json:"minute_rate_limited"`
}

// slackInnerEvent is the subset of the event_callback's nested "event"
// object the ingest pipeline reads, across the event types it handles. Every
// other field (e.g. blocks, attachments) is out of scope for this leaf — no
// write-back, no rich rendering.
type slackInnerEvent struct {
	Type string `json:"type"`
	// Subtype discriminates message.channels / message.groups deliveries: a
	// plain thread reply carries no subtype (""), a reply also broadcast to the
	// channel carries "thread_broadcast", and every edit/delete/system/bot
	// variant carries its own (message_changed, message_deleted, channel_join,
	// bot_message, …). Empty on app_mention. See the engaged-thread branch in
	// ingest.go.
	Subtype  string `json:"subtype"`
	User     string `json:"user"`
	BotID    string `json:"bot_id"`
	Channel  string `json:"channel"`
	Text     string `json:"text"`
	TS       string `json:"ts"`
	ThreadTS string `json:"thread_ts"`
	// OldChannelID / NewChannelID carry a channel_id_changed event: the id
	// Slack retired and the one it now uses.
	OldChannelID string `json:"old_channel_id"`
	NewChannelID string `json:"new_channel_id"`
	// Tokens carries a tokens_revoked event's revoked tokens, each named by
	// the id of the user it belongs to. TF holds no user tokens, so
	// tokens.oauth is not read.
	Tokens struct {
		Bot []string `json:"bot"`
	} `json:"tokens"`
}

// newInboundMention is the one translation both transports make from an
// event_callback envelope and its parsed inner event.
func newInboundMention(envelope slackEventEnvelope, inner slackInnerEvent) inboundMention {
	ev := inboundMention{
		Type:     inner.Type,
		Subtype:  inner.Subtype,
		EventID:  envelope.EventID,
		Channel:  inner.Channel,
		User:     inner.User,
		BotID:    inner.BotID,
		Text:     inner.Text,
		TS:       inner.TS,
		ThreadTS: inner.ThreadTS,

		OldChannelID: inner.OldChannelID,
		NewChannelID: inner.NewChannelID,

		RevokedBots: inner.Tokens.Bot,
	}
	if envelope.EventTime > 0 {
		ev.EventTime = time.Unix(envelope.EventTime, 0)
	}
	return ev
}

// webhookHandler serves the Events API (webhook) transport for the shared
// ingest pipeline on two routes: POST /api/webhooks/slack/{org_id} for an
// org's own app, and POST /api/webhooks/slack for the deployment's app.
// Pre-auth: Slack has no session, so every gate here (signature, workspace
// resolution, entitlement) runs by hand inside the handler.
type webhookHandler struct {
	stores   db.Stores
	pipeline *ingestPipeline
	// stats records the transport-level volume signals (retry deliveries,
	// app_rate_limited notices); the per-delivery outcome counters live in
	// the pipeline. Nil-safe, like the pipeline's optional collaborators.
	stats *ingestStats
	// deployment is the deployment's own Slack app, the zero app when there
	// is none.
	deployment deploymentApp
	// mismatches and otherAppMismatches limit how often the deployment
	// endpoint logs a delivery signed with another secret: one naming the
	// deployment's app or none, and one naming another app.
	mismatches         mismatchReport
	otherAppMismatches mismatchReport
}

// handleWebhook serves POST /api/webhooks/slack/{org_id}, for an org's own
// app. It mirrors the GitHub receiver's discipline
// (internal/server/github_webhooks.go): body cap, generic responses that
// never disclose whether a team_id/api_app_id/org pairing exists, and a
// real signature check before anything else runs.
//
// Order matters and is deliberate:
//  1. Resolve the workspace from the payload's (team_id, api_app_id)
//     composite key, cross-checked against the org_id path param — unknown,
//     missing api_app_id, or mismatched is a generic 200-empty (never
//     discloses which org owns a workspace/app).
//  2. Signature — verified against the workspace's stored signing secret;
//     a connection with no signing secret (a socket-transport app, or any
//     connection through the deployment's app) is rejected the same as a
//     bad signature: this endpoint isn't for it.
//     Deliberately BEFORE the entitlement check: only a caller already
//     holding the workspace's signing secret (i.e. Slack itself) can ever
//     reach past this point, so a 401-vs-200 split here can't be used as
//     an entitlement oracle — an unsigned/forged request always gets 401
//     regardless of whether the org's Slack feature happens to be
//     licensed.
//  3. Only past a verified signature does serveVerified dispatch the
//     payload's type.
func (h *webhookHandler) handleWebhook(w http.ResponseWriter, r *http.Request) {
	pathOrgID := r.PathValue("org_id")

	body, ok := readSlackWebhookBody(w, r)
	if !ok {
		return
	}

	var envelope slackEventEnvelope
	if err := json.Unmarshal(body, &envelope); err != nil {
		httpx.BadRequest(w, "malformed payload")
		return
	}

	ws, err := slackstore.FromStores(h.stores).Connections.GetByWorkspaceAppSystem(r.Context(), envelope.TeamID, envelope.APIAppID)
	if err != nil {
		httpx.InternalError(w, "slack-webhook", err)
		return
	}
	if ws == nil || ws.OrgID != pathOrgID {
		// Covers an unknown (team_id, api_app_id) pair AND a payload
		// missing api_app_id entirely (envelope.APIAppID == "" never
		// matches a row — api_app_id is NOT NULL) with the identical
		// response as any other unrecognized delivery.
		ackEmpty(w)
		return
	}

	if ws.SigningSecretRef == "" {
		// A socket-transport connection and a connection through the
		// deployment's app have no signing secret: this endpoint is not
		// for them.
		httpx.WriteUnauth(w)
		return
	}
	// Every delivery from here on is counted by its verdict, because one org's
	// refusals are its own app's configuration, while most orgs' at once are
	// this deployment's: its secret store, a proxy that changes request bodies
	// or headers, or this host's clock.
	secret, err := h.stores.Secrets.GetSystem(r.Context(), ws.OrgID, ws.SigningSecretRef)
	if err != nil {
		h.stats.recordOrgDelivery(r.Context(), ws.APIAppID, orgDeliveryNoSecret)
		httpx.InternalError(w, "slack-webhook", err)
		return
	}
	if secret == "" {
		h.stats.recordOrgDelivery(r.Context(), ws.APIAppID, orgDeliveryNoSecret)
		httpx.WriteUnauth(w)
		return
	}
	check := checkSlackSignature(secret, body, r.Header.Get("X-Slack-Request-Timestamp"), r.Header.Get("X-Slack-Signature"))
	h.stats.recordOrgDelivery(r.Context(), ws.APIAppID, check.label())
	if check != signatureValid {
		httpx.WriteUnauth(w)
		return
	}

	h.serveVerified(w, r, envelope, func(ctx context.Context, inner slackInnerEvent) error {
		return h.pipeline.deliverForConnection(ctx, *ws, envelope, inner)
	})
}

// handleDeploymentWebhook serves POST /api/webhooks/slack, for the
// deployment's app when its transport is the Events API. The signature comes
// first, before the body is parsed, because the signing secret is the
// environment's and needs no row to find: a forged request gets a 401 and
// learns nothing about which workspaces are connected.
func (h *webhookHandler) handleDeploymentWebhook(w http.ResponseWriter, r *http.Request) {
	if !h.deployment.Configured() || h.deployment.Transport != transportEventsAPI {
		httpx.NotFound(w, "deployment Slack app Events API endpoint")
		return
	}

	body, ok := readSlackWebhookBody(w, r)
	if !ok {
		return
	}
	switch checkSlackSignature(h.deployment.SigningSecret, body, r.Header.Get("X-Slack-Request-Timestamp"), r.Header.Get("X-Slack-Signature")) {
	case signatureValid:
	case signatureMismatch:
		h.reportSignatureMismatch(r.Context(), body)
		httpx.WriteUnauth(w)
		return
	default:
		httpx.WriteUnauth(w)
		return
	}

	var envelope slackEventEnvelope
	if err := json.Unmarshal(body, &envelope); err != nil {
		httpx.BadRequest(w, "malformed payload")
		return
	}

	h.serveVerified(w, r, envelope, func(ctx context.Context, inner slackInnerEvent) error {
		return h.pipeline.deliverDeploymentEvent(ctx, h.deployment.AppID, envelope, inner)
	})
}

// readSlackWebhookBody reads a delivery's body under maxSlackWebhookBody,
// answering the request itself when it can't: 400 for a read failure, 413
// over the cap.
func readSlackWebhookBody(w http.ResponseWriter, r *http.Request) ([]byte, bool) {
	body, err := io.ReadAll(io.LimitReader(r.Body, maxSlackWebhookBody+1))
	if err != nil {
		httpx.BadRequest(w, "could not read request body")
		return nil, false
	}
	if len(body) > maxSlackWebhookBody {
		httpx.WriteErrors(w, http.StatusRequestEntityTooLarge, httpx.ErrorItem{
			Reason:  httpx.ReasonPayloadTooLarge,
			Message: fmt.Sprintf("request body exceeds the %d-byte limit", maxSlackWebhookBody),
		})
		return nil, false
	}
	return body, true
}

// serveVerified answers a delivery whose signature both routes have checked,
// the same way on each. deliver is the part that differs: it finds the
// connection an event_callback is for and runs the event through the
// pipeline, returning an error only for a failure Slack should redeliver.
//
// The entitlement gate is deliver's, and applies only to event_callback, not
// to url_verification: the handshake proves the endpoint is live and
// correctly configured, not processing work, so a lapsed org can still
// complete it. A lapsed org's real (validly-signed) event deliveries get
// 200-empty so Slack doesn't deactivate the subscription for persistent
// failures.
func (h *webhookHandler) serveVerified(w http.ResponseWriter, r *http.Request, envelope slackEventEnvelope, deliver func(context.Context, slackInnerEvent) error) {
	// Past the signature: the delivery is authentically Slack's, so its
	// retry marker is a trustworthy operational signal. X-Slack-Retry-Num > 0
	// means an earlier attempt wasn't acked in time (or 5xxed) — sustained
	// retries are the early warning of the disable cascade (Slack turns off
	// an app's event subscription after enough consecutive failures), so
	// this Warn is the line operators alert on.
	if n, err := strconv.Atoi(r.Header.Get("X-Slack-Retry-Num")); err == nil && n > 0 {
		slackLog.Warn("slack events api: delivery is a Slack retry — sustained retries precede Slack disabling the event subscription",
			"workspace", envelope.TeamID, "app", envelope.APIAppID,
			"retry_num", n, "retry_reason", r.Header.Get("X-Slack-Retry-Reason"))
		h.stats.recordRetry(r.Context(), envelope.APIAppID, transportEventsAPI)
	}

	switch envelope.Type {
	case "url_verification":
		httpx.WriteJSON(w, http.StatusOK, map[string]string{"challenge": envelope.Challenge})
	case "event_callback":
		var inner slackInnerEvent
		if err := json.Unmarshal(envelope.Event, &inner); err != nil {
			httpx.BadRequest(w, "malformed event payload")
			return
		}
		if err := deliver(r.Context(), inner); err != nil {
			httpx.InternalError(w, "slack-webhook", err)
			return
		}
		ackEmpty(w)
	case "app_rate_limited":
		// Slack exhausted this app's Events API budget (30k events per
		// workspace per hour) and is DROPPING deliveries at the source —
		// they will not be retried once the limit lifts. Nothing to
		// process; the ack keeps the subscription healthy. Logged at Warn
		// (not gated on entitlement — it's operational telemetry about
		// volume, not work) so operators see the loss the moment it starts;
		// with the message.* firehose this is the "volume outgrew the
		// transport" signal that says move to Socket Mode or split apps.
		slackLog.Warn("slack events api: app_rate_limited — Slack is dropping this app's event deliveries",
			"workspace", envelope.TeamID, "app", envelope.APIAppID,
			"minute_rate_limited", envelope.MinuteRateLimited)
		h.stats.recordRateLimited(r.Context(), envelope.APIAppID)
		ackEmpty(w)
	default:
		// Slack documents other envelope types this leaf has no use for —
		// ack so Slack doesn't retry.
		ackEmpty(w)
	}
}

// ackEmpty writes the generic 200 this receiver uses for every case that
// must look identical to a caller with no context to distinguish them:
// unknown workspace, cross-org mismatch, lapsed entitlement, and an
// envelope type with nothing to do. Same body shape every time so none of
// those cases is distinguishable from another by response inspection.
func ackEmpty(w http.ResponseWriter) {
	httpx.WriteJSON(w, http.StatusOK, map[string]string{})
}

// signatureCheck is the outcome of checking a request's Slack signature
// against Slack's documented v0 HMAC-SHA256 scheme
// (HMAC(signingSecret, "v0:"+timestamp+":"+body)) and the 5-minute replay
// window on X-Slack-Request-Timestamp.
type signatureCheck int

const (
	// signatureMalformed is a request Slack would not send: a header missing
	// or unparseable, or a digest that is not SHA-256's length. It is the
	// zero value, so a check left unset refuses.
	signatureMalformed signatureCheck = iota
	// signatureStale is a timestamp outside the replay window. Every
	// delivery from Slack looks like this when this host's clock is off by
	// more than the window, and so does a replay.
	signatureStale
	// signatureMismatch is a fresh, well-formed signature that secret does
	// not produce. Every delivery Slack sends looks like this when the
	// configured secret is not the app's, and so does a forgery.
	signatureMismatch
	signatureValid
)

// label is the check's verdict on slack.webhook.org.deliveries.
func (c signatureCheck) label() string {
	switch c {
	case signatureValid:
		return "verified"
	case signatureMismatch:
		return "mismatch"
	case signatureStale:
		return "stale"
	default:
		return "malformed"
	}
}

func checkSlackSignature(secret string, body []byte, timestampHeader, sigHeader string) signatureCheck {
	if timestampHeader == "" || sigHeader == "" {
		return signatureMalformed
	}
	ts, err := strconv.ParseInt(timestampHeader, 10, 64)
	if err != nil {
		return signatureMalformed
	}
	age := time.Since(time.Unix(ts, 0))
	if age < 0 {
		age = -age
	}
	if age > slackReplayWindow {
		return signatureStale
	}

	const prefix = "v0="
	if !strings.HasPrefix(sigHeader, prefix) {
		return signatureMalformed
	}
	want, err := hex.DecodeString(strings.TrimPrefix(sigHeader, prefix))
	if err != nil || len(want) != sha256.Size {
		return signatureMalformed
	}

	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte("v0:"))
	mac.Write([]byte(timestampHeader))
	mac.Write([]byte(":"))
	mac.Write(body)
	if !hmac.Equal(want, mac.Sum(nil)) {
		return signatureMismatch
	}
	return signatureValid
}

// The values of slack.webhook.signature.mismatches' names_app attribute: the
// app a mismatched delivery's unverified body names.
const (
	mismatchNamesDeploymentApp = "deployment"
	mismatchNamesOtherApp      = "other"
	mismatchNamesNoApp         = "none"
)

// slackAppIDPattern is the shape of a Slack App ID, bounded so an unverified
// body cannot put a long string in the log.
var slackAppIDPattern = regexp.MustCompile(`^A[A-Z0-9]{2,31}$`)

// reportSignatureMismatch counts, and logs at a limited rate, a delivery to
// the deployment endpoint that TF_SLACK_SIGNING_SECRET did not sign. A wrong
// secret fails every delivery here and nowhere else, before Slack turns the
// app's event delivery off. So does another app whose request URL points
// here, which is why the app the body names is reported: it is unverified,
// so it tells the operator where to look and nothing acts on it. The lines
// are warnings, not errors, because anyone can send a request that lands
// here.
func (h *webhookHandler) reportSignatureMismatch(ctx context.Context, body []byte) {
	var named struct {
		APIAppID string `json:"api_app_id"`
	}
	_ = json.Unmarshal(body, &named)
	names := mismatchNamesNoApp
	switch {
	case named.APIAppID == "":
	case named.APIAppID == h.deployment.AppID:
		names = mismatchNamesDeploymentApp
	case slackAppIDPattern.MatchString(named.APIAppID):
		names = mismatchNamesOtherApp
	}
	h.stats.recordSignatureMismatch(ctx, h.deployment.AppID, names)

	if names == mismatchNamesOtherApp {
		if log, count := h.otherAppMismatches.record(time.Now()); log {
			slackLog.Warn("slack: a delivery naming another Slack app reached the deployment Slack app's Events API endpoint; "+
				"if these keep coming, that app's request URL points here: change it or delete the app",
				"app_id", h.deployment.AppID, "delivery_app_id", named.APIAppID, "mismatches", count)
		}
		return
	}
	if log, count := h.mismatches.record(time.Now()); log {
		slackLog.Warn("slack: a delivery to the deployment Slack app's Events API endpoint is not signed with "+envSlackSigningSecret+
			"; if Slack's deliveries all fail this way, set it to the Signing Secret on the app's Basic Information page",
			"app_id", h.deployment.AppID, "mismatches", count)
	}
}

// signatureMismatchLogInterval is the least time between two logged
// signature mismatches.
const signatureMismatchLogInterval = time.Minute

// mismatchReport decides which signature mismatches the deployment endpoint
// logs: the first, then at most one an interval, each line counting the
// mismatches since the one before. The endpoint answers anyone, and a forger
// can produce a mismatch as readily as a wrong secret can, so a line per
// request would let any caller write the log. The zero value is ready to use.
type mismatchReport struct {
	mu       sync.Mutex
	logged   time.Time
	unlogged int
}

// record counts a mismatch at now. It reports whether to log it and, if so,
// how many mismatches the line covers, this one included.
func (m *mismatchReport) record(now time.Time) (log bool, count int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.unlogged++
	if !m.logged.IsZero() && now.Sub(m.logged) < signatureMismatchLogInterval {
		return false, 0
	}
	count = m.unlogged
	m.logged, m.unlogged = now, 0
	return true, count
}
