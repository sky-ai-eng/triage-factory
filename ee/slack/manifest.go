package slack

import "fmt"

// slackBotScopes is the full OAuth bot scope set the manifest requests,
// regardless of transport. Over-asking now (the complete set the planned
// exec verbs will eventually need) avoids a reinstall-to-rescope prompt
// later — Slack scope grants are all-or-nothing per install, so adding a
// scope after the fact means every connected workspace re-authorizes.
//
//	app_mentions:read   - see @mentions of the bot (the only inbound signal this leaf's ingest follow-on needs)
//	chat:write          - post messages / replies
//	reactions:write     - add emoji reactions (progress/ack signaling)
//	users:read          - resolve a Slack user id to a profile (identity capture, later leaf)
//	users:read.email    - resolve a Slack user's verified email (TF-user auto-match, later leaf)
//	channels:history    - read public-channel message history (thread context for a mention)
//	groups:history      - read private-channel ("group") message history, same reason
//	files:read          - read files shared in a thread (attachments as agent context)
//	files:write         - upload files (agent-produced artifacts back into the thread)
var slackBotScopes = []string{
	"app_mentions:read",
	"chat:write",
	"reactions:write",
	"users:read",
	"users:read.email",
	"channels:history",
	"groups:history",
	"files:read",
	"files:write",
}

// slackManifest is the subset of Slack's app-manifest schema this leaf
// populates. Built via structs + encoding/json (not text/template string
// interpolation) so every field is properly escaped — a template would
// otherwise need to hand-roll JSON-string escaping for the org display name.
type slackManifest struct {
	DisplayInformation slackManifestDisplayInfo `json:"display_information"`
	Features           slackManifestFeatures    `json:"features"`
	OAuthConfig        slackManifestOAuth       `json:"oauth_config"`
	Settings           slackManifestSettings    `json:"settings"`
}

type slackManifestDisplayInfo struct {
	Name string `json:"name"`
}

type slackManifestFeatures struct {
	BotUser slackManifestBotUser `json:"bot_user"`
}

type slackManifestBotUser struct {
	DisplayName  string `json:"display_name"`
	AlwaysOnline bool   `json:"always_online"`
}

type slackManifestOAuth struct {
	Scopes slackManifestScopes `json:"scopes"`
}

type slackManifestScopes struct {
	Bot []string `json:"bot"`
}

type slackManifestSettings struct {
	EventSubscriptions slackManifestEventSubs `json:"event_subscriptions"`
	// SocketModeEnabled is a pointer so transport=events_api omits the key
	// entirely (Slack treats absence as false) rather than emitting an
	// explicit "socket_mode_enabled": false alongside a request_url — the
	// two are mutually exclusive by construction, not just by convention.
	SocketModeEnabled    *bool `json:"socket_mode_enabled,omitempty"`
	OrgDeployEnabled     bool  `json:"org_deploy_enabled"`
	TokenRotationEnabled bool  `json:"token_rotation_enabled"`
}

type slackManifestEventSubs struct {
	// RequestURL is omitted (empty) for transport=socket — Socket Mode
	// carries events over the websocket, not an inbound webhook.
	RequestURL string   `json:"request_url,omitempty"`
	BotEvents  []string `json:"bot_events"`
}

// slackManifestName builds the manifest's display name from the org's
// display name — "Triage Factory" alone when the org has none (shouldn't
// happen in practice; orgs always have a name, but the manifest builder
// stays defensive rather than emitting a trailing separator).
func slackManifestName(orgName string) string {
	if orgName == "" {
		return "Triage Factory"
	}
	return fmt.Sprintf("Triage Factory (%s)", orgName)
}

// slackWebhookPath is the URL shape ee/slack's manifest advertises for the
// events_api transport. The receiver at this path doesn't exist yet — it's
// the next leaf's job — but the shape must be fixed now so today's manifest
// is the one the receiver actually serves.
func slackWebhookPath(orgID string) string {
	return "/api/webhooks/slack/" + orgID
}

// buildSlackManifest renders the manifest for one (org, transport) pair.
// publicURL is the deployment's external base (ExtensionAPI.PublicURL());
// callers must reject an empty publicURL for transport=events_api before
// calling this (buildSlackManifest itself has no HTTP-status concept) — see
// handleManifest's 409.
func buildSlackManifest(orgName, transport, publicURL, orgID string) slackManifest {
	m := slackManifest{
		DisplayInformation: slackManifestDisplayInfo{Name: slackManifestName(orgName)},
		Features: slackManifestFeatures{
			BotUser: slackManifestBotUser{DisplayName: "Triage Factory", AlwaysOnline: true},
		},
		OAuthConfig: slackManifestOAuth{Scopes: slackManifestScopes{Bot: slackBotScopes}},
		Settings: slackManifestSettings{
			EventSubscriptions: slackManifestEventSubs{BotEvents: []string{"app_mention"}},
		},
	}
	switch transport {
	case transportSocket:
		enabled := true
		m.Settings.SocketModeEnabled = &enabled
	case transportEventsAPI:
		m.Settings.EventSubscriptions.RequestURL = publicURL + slackWebhookPath(orgID)
	}
	return m
}
