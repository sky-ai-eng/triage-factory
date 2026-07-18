package slack

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	slackstore "github.com/sky-ai-eng/triage-factory/ee/slack/store"
	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/entitlements"
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
	// MinuteRateLimited is set on a type=app_rate_limited envelope: the unix
	// timestamp of the minute Slack started dropping this app's event
	// deliveries (its 30k events/workspace/hour Events API budget was
	// exhausted — deliveries in excess are never retried later).
	MinuteRateLimited int64 `json:"minute_rate_limited"`
}

// slackInnerEvent is the subset of the event_callback's nested "event"
// object app_mention ingest needs. Every other app_mention field (e.g.
// blocks, attachments) is out of scope for this leaf — no write-back, no
// rich rendering.
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
}

// webhookHandler serves POST /api/webhooks/slack/{org_id} — the Events API
// (webhook) transport for the shared ingest pipeline. Pre-auth: Slack has
// no session, so every gate here (workspace resolution, entitlement,
// signature) runs by hand inside the handler.
type webhookHandler struct {
	stores   db.Stores
	pipeline *ingestPipeline
	// stats records the transport-level volume signals (retry deliveries,
	// app_rate_limited notices); the per-delivery outcome counters live in
	// the pipeline. Nil-safe, like the pipeline's optional collaborators.
	stats *ingestStats
}

// handleWebhook mirrors the GitHub receiver's discipline
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
//     a workspace with no signing secret (a socket-transport connection)
//     is rejected the same as a bad signature: this endpoint isn't for it.
//     Deliberately BEFORE the entitlement check: only a caller already
//     holding the workspace's signing secret (i.e. Slack itself) can ever
//     reach past this point, so a 401-vs-200 split here can't be used as
//     an entitlement oracle — an unsigned/forged request always gets 401
//     regardless of whether the org's Slack feature happens to be
//     licensed.
//  3. Only past a verified signature does the payload's type get
//     dispatched. The entitlement gate (TFAC-524's ack-and-drop dormancy
//     contract) applies only to event_callback, not to url_verification —
//     the handshake is proving the endpoint is live and correctly
//     configured, not processing work, so a lapsed org can still complete
//     it; a lapsed org's real (validly-signed) event deliveries get
//     200-empty so Slack doesn't deactivate the subscription for
//     persistent failures.
func (h *webhookHandler) handleWebhook(w http.ResponseWriter, r *http.Request) {
	pathOrgID := r.PathValue("org_id")

	body, err := io.ReadAll(io.LimitReader(r.Body, maxSlackWebhookBody+1))
	if err != nil {
		httpx.BadRequest(w, "could not read request body")
		return
	}
	if len(body) > maxSlackWebhookBody {
		http.Error(w, "payload too large", http.StatusRequestEntityTooLarge)
		return
	}

	var envelope slackEventEnvelope
	if err := json.Unmarshal(body, &envelope); err != nil {
		httpx.BadRequest(w, "malformed payload")
		return
	}

	ws, err := slackstore.FromStores(h.stores).Workspaces.GetByWorkspaceAppSystem(r.Context(), envelope.TeamID, envelope.APIAppID)
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
		// A socket-transport workspace has no signing secret — this
		// endpoint is not for it.
		httpx.WriteUnauth(w)
		return
	}
	secret, err := h.stores.Secrets.GetSystem(r.Context(), ws.OrgID, ws.SigningSecretRef)
	if err != nil {
		httpx.InternalError(w, "slack-webhook", err)
		return
	}
	if secret == "" || !validSlackSignature(secret, body, r.Header.Get("X-Slack-Request-Timestamp"), r.Header.Get("X-Slack-Signature")) {
		httpx.WriteUnauth(w)
		return
	}

	// Past the signature: the delivery is authentically Slack's, so its
	// retry marker is a trustworthy operational signal. X-Slack-Retry-Num > 0
	// means an earlier attempt wasn't acked in time (or 5xxed) — sustained
	// retries are the early warning of the disable cascade (Slack turns off
	// an app's event subscription after enough consecutive failures), so
	// this Warn is the line operators alert on.
	if n, err := strconv.Atoi(r.Header.Get("X-Slack-Retry-Num")); err == nil && n > 0 {
		slackLog.Warn("slack events api: delivery is a Slack retry — sustained retries precede Slack disabling the event subscription",
			"workspace", ws.WorkspaceID, "app", ws.APIAppID,
			"retry_num", n, "retry_reason", r.Header.Get("X-Slack-Retry-Reason"))
		h.stats.recordRetry(r.Context(), ws.APIAppID, transportEventsAPI)
	}

	switch envelope.Type {
	case "url_verification":
		httpx.WriteJSON(w, http.StatusOK, map[string]string{"challenge": envelope.Challenge})
	case "event_callback":
		if !entitlements.For(ws.OrgID).Has(entitlements.FeatureSlack) {
			ackEmpty(w)
			return
		}
		h.handleEventCallback(w, r, *ws, envelope)
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
			"workspace", ws.WorkspaceID, "app", ws.APIAppID,
			"minute_rate_limited", envelope.MinuteRateLimited)
		h.stats.recordRateLimited(r.Context(), ws.APIAppID)
		ackEmpty(w)
	default:
		// Slack documents other envelope types this leaf has no use for —
		// ack so Slack doesn't retry.
		ackEmpty(w)
	}
}

func (h *webhookHandler) handleEventCallback(w http.ResponseWriter, r *http.Request, ws slackstore.Workspace, envelope slackEventEnvelope) {
	var inner slackInnerEvent
	if err := json.Unmarshal(envelope.Event, &inner); err != nil {
		httpx.BadRequest(w, "malformed event payload")
		return
	}

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
	}
	if err := h.pipeline.handleEventCallback(r.Context(), ws, ev); err != nil {
		httpx.InternalError(w, "slack-webhook", err)
		return
	}
	ackEmpty(w)
}

// ackEmpty writes the generic 200 this receiver uses for every case that
// must look identical to a caller with no context to distinguish them:
// unknown workspace, cross-org mismatch, lapsed entitlement, and an
// envelope type with nothing to do. Same body shape every time so none of
// those cases is distinguishable from another by response inspection.
func ackEmpty(w http.ResponseWriter) {
	httpx.WriteJSON(w, http.StatusOK, map[string]string{})
}

// validSlackSignature verifies X-Slack-Signature against Slack's documented
// v0 HMAC-SHA256 scheme (HMAC(signingSecret, "v0:"+timestamp+":"+body)) and
// enforces the 5-minute replay window on X-Slack-Request-Timestamp.
// Constant-time comparison, mirrors validWebhookSignature's GitHub
// analogue (internal/server/github_webhooks.go).
func validSlackSignature(secret string, body []byte, timestampHeader, sigHeader string) bool {
	if timestampHeader == "" || sigHeader == "" {
		return false
	}
	ts, err := strconv.ParseInt(timestampHeader, 10, 64)
	if err != nil {
		return false
	}
	age := time.Since(time.Unix(ts, 0))
	if age < 0 {
		age = -age
	}
	if age > slackReplayWindow {
		return false
	}

	const prefix = "v0="
	if !strings.HasPrefix(sigHeader, prefix) {
		return false
	}
	want, err := hex.DecodeString(strings.TrimPrefix(sigHeader, prefix))
	if err != nil {
		return false
	}

	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte("v0:"))
	mac.Write([]byte(timestampHeader))
	mac.Write([]byte(":"))
	mac.Write(body)
	return hmac.Equal(want, mac.Sum(nil))
}
