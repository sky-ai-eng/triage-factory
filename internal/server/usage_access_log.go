package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/server/httpx"
)

// Access & credential change-log viewer (TFAC-484) — the org-admin EE audit
// surface on the Usage page. It reads the already-captured access_change_log
// (TFAC-471): who changed org/team memberships, roles, ownership, and
// credentials, and when. This is the SECOND audit surface (the bot-activity /
// artifacts feed is the other) and answers a distinct question — who changed the
// org's *access*, not what the agent did externally.
//
// The data + store are core (access_change_log lives in the baseline schema); the
// VIEWER is Enterprise — a cross-team, org-wide lens gated by FeatureGovernance.
// Only the gate is EE, so the endpoint lives next to the spend rollup in the core
// usageHandler rather than in the ee/ subtree.

// accessChangeJSON is one rendered audit row. ActionLabel is the server-rendered
// human predicate the FE shows after the actor + timestamp ("changed Alice from
// member to admin"); ActorName/TargetName/TeamName are the pre-resolved display
// names ("" when a since-removed user/team no longer resolves). DetailJSON is the
// raw captured payload, passed through for power users / future use (omitted when
// empty).
type accessChangeJSON struct {
	ID          string          `json:"id"`
	Action      string          `json:"action"`
	ActionLabel string          `json:"action_label"`
	ActorName   string          `json:"actor_name"`
	TargetName  string          `json:"target_name,omitempty"`
	TeamName    string          `json:"team_name,omitempty"`
	DetailJSON  json.RawMessage `json:"detail_json,omitempty"`
	CreatedAt   time.Time       `json:"created_at"`
}

// accessLogListRequest is the body of POST /api/orgs/{org_id}/usage/access-log/list.
// Category narrows to one bucket of actions; empty is every action.
type accessLogListRequest struct {
	Category string `json:"category"`

	httpx.PageRequest
}

type accessLogFilterKey struct {
	Category string `json:"category"`
}

// handleUsageAccessLog serves the EE access & credential change-log viewer.
//
// Gate: org admin AND FeatureGovernance, in that order — the whole family's
// rule (see resolveGovernedOrgAdmin). A non-admin always gets a 403 and so
// never learns the deployment's licence tier; an org admin on an unlicensed
// build gets a 404 that reads as "no such route", so the feature stays
// invisible to everyone entitled to see it. Local mode short-circuits the admin
// gate to allowed (N=1), so a licensed local build still serves it.
//
// The read runs on the app pool under the admin's claims: access_change_log's
// org-scoped RLS (org_id = current_org_id() AND user_has_org_access) admits the
// admin's own org and nothing else — the org-admin gate is the authorization for
// the org-wide lens, RLS the defense-in-depth.
//
// It answers the shared list envelope. Its old ad-hoc
// `{items, limit, offset, has_more}` shape is gone, and with it the read-one-
// past-the-page trick that stood in for a total: the viewer now knows how many
// rows the filter matches, not merely that another page exists.
//
// POST /api/orgs/{org_id}/usage/access-log/list
func (h *usageHandler) handleUsageAccessLog(w http.ResponseWriter, r *http.Request) {
	orgID, userID, ok := h.az.RequireOrgAdmin(w, r)
	if !ok {
		return
	}
	if !requireGovernance(w, r, orgID) {
		return
	}

	var req accessLogListRequest
	if !httpx.DecodeJSONStrict(w, r, &req) {
		return
	}
	var v httpx.Validation
	category := strings.TrimSpace(req.Category)
	// A closed vocabulary: an unknown category used to pass straight through to
	// a filter that matched nothing, so a typo returned an
	// authoritative-looking empty log.
	if category != "" && !slices.Contains(accessLogCategories, category) {
		v.Invalid("category", "must be one of: "+strings.Join(accessLogCategories, ", "))
	}
	page := httpx.ResolvePage(&v, req.PageRequest, httpx.FilterFingerprint(accessLogFilterKey{Category: category}), 0)
	if v.Flush(w, http.StatusBadRequest) {
		return
	}

	var (
		rows  []domain.AccessChange
		total int
		names map[string]string // user id -> display name (actors + targets)
		teams map[string]string // team id -> team name
	)
	if err := h.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		var e error
		rows, total, e = tx.AccessChangeLog.ListByOrg(r.Context(), orgID, domain.AccessChangeListOpts{
			Limit: page.Limit, Offset: page.Offset, Category: category,
		})
		if e != nil {
			return e
		}
		if names, e = resolveAccessUserNames(r.Context(), tx, rows); e != nil {
			return e
		}
		teams, e = resolveAccessTeamNames(r.Context(), tx, orgID, rows)
		return e
	}); err != nil {
		internalError(w, "usage-access-log", err)
		return
	}

	items := make([]accessChangeJSON, 0, len(rows))
	for _, e := range rows {
		target := names[e.TargetUserID]
		team := teams[e.TeamID]
		items = append(items, accessChangeJSON{
			ID:          e.ID,
			Action:      e.Action,
			ActionLabel: accessChangeLabel(e, target, team),
			ActorName:   names[e.ActorUserID],
			TargetName:  target,
			TeamName:    team,
			DetailJSON:  rawJSONOrNil(e.DetailJSON),
			CreatedAt:   e.CreatedAt,
		})
	}
	httpx.WriteList(w, page, items, total)
}

// accessLogCategories is the closed category vocabulary — the buckets
// domain.AccessChangeListOpts knows how to narrow to.
var accessLogCategories = []string{
	domain.AccessCategoryMembership,
	domain.AccessCategoryCredential,
	domain.AccessCategoryPolicy,
}

// --- name resolution (N+1 over the small distinct-id sets; the page is bounded) ---

// resolveAccessUserNames maps each distinct actor + target user id in rows to a
// display name via the app-pool GetDisplayName (users_select RLS is org-scoped,
// so the org admin resolves any co-member's name). A name that doesn't resolve —
// a since-revoked member who no longer shares the org under RLS — maps to "", and
// the label falls back to a generic noun while the FE shows "Unknown".
func resolveAccessUserNames(ctx context.Context, tx db.TxStores, rows []domain.AccessChange) (map[string]string, error) {
	names := map[string]string{}
	for _, e := range rows {
		for _, uid := range [2]string{e.ActorUserID, e.TargetUserID} {
			if uid == "" {
				continue
			}
			if _, done := names[uid]; done {
				continue
			}
			name, err := tx.Users.GetDisplayName(ctx, uid)
			if err != nil {
				return nil, err
			}
			names[uid] = name
		}
	}
	return names, nil
}

// resolveAccessTeamNames maps each distinct team id in rows to its name via the
// admin-pool GetSystem — the org-admin audit view spans teams the admin may not
// belong to (mirrors resolveSpendTeamNames on the spend rollup). A missing team
// maps to "".
func resolveAccessTeamNames(ctx context.Context, tx db.TxStores, orgID string, rows []domain.AccessChange) (map[string]string, error) {
	names := map[string]string{}
	for _, e := range rows {
		if e.TeamID == "" {
			continue
		}
		if _, done := names[e.TeamID]; done {
			continue
		}
		t, err := tx.Teams.GetSystem(ctx, orgID, e.TeamID)
		if err != nil {
			return nil, err
		}
		name := ""
		if t != nil {
			name = t.Name
		}
		names[e.TeamID] = name
	}
	return names, nil
}

// --- label rendering (pure) ---

// accessDetail is the union of every field any action's detail_json carries
// (TFAC-471's builders); a given row populates only the subset for its action.
type accessDetail struct {
	OldRole  string `json:"old_role"`
	NewRole  string `json:"new_role"`
	Kind     string `json:"kind"`
	Host     string `json:"host"`
	Name     string `json:"name"`
	InviteID string `json:"invite_id"`
	Email    string `json:"email"`
	// Source is the sso_jit grant marker. Role is the granted/invited role,
	// shared by the sso_jit grant and invite_created payloads, and deliberately
	// distinct from NewRole (which the team-add / role-change actions carry) —
	// both builders write a "role" key, not "new_role".
	Source string `json:"source"`
	Role   string `json:"role"`
	// Domain is the claimed/verified/removed domain on the sso_domain_* rows.
	// The SSO connection rows' provider_id and the Slack credential row's ids
	// are captured for the raw passthrough but not read here — no label needs
	// them, and an opaque uuid would only make the line harder to scan.
	Domain string `json:"domain"`
	// The api_token_* rows: Name is shared with the credential rows above;
	// Prefix is the visible head of the secret, which is how its owner tells
	// two tokens apart and the only part of it the log ever holds. The three
	// bounds are what the token was minted under — the cap as it stood then,
	// which is the point of recording it, since the cap applies at use
	// against whatever the setting says later. token_id is left to the raw
	// passthrough for the same reason as the other uuids.
	Prefix       string     `json:"prefix"`
	ExpiresAt    *time.Time `json:"expires_at"`
	MaxAgeDays   *int       `json:"max_age_days"`
	AllowedCIDRs []string   `json:"allowed_cidrs"`
}

// accessChangeLabel renders one row's action + detail_json into the human
// predicate shown after the actor + timestamp ("changed Alice from member to
// admin"). targetName/teamName are the pre-resolved display names (each may be
// ""); detail_json is parsed leniently, so a malformed or absent field degrades
// to a generic phrasing rather than failing the whole row.
func accessChangeLabel(e domain.AccessChange, targetName, teamName string) string {
	target := orFallback(targetName, "a user")
	team := orFallback(teamName, "a team")
	d := parseAccessDetail(e.DetailJSON)
	switch e.Action {
	case domain.AccessActionOrgMemberGranted:
		// Invite-accept and SSO JIT both write actor == target (the user joined
		// themselves), which reads as "joined"; JIT carries source=sso_jit so it
		// reads as "joined via SSO". A future direct-grant path (actor != target)
		// reads as "granted ... org membership".
		if e.ActorUserID != "" && e.ActorUserID == e.TargetUserID {
			if d.Source == domain.AccessSourceSSOJIT {
				return "joined the org via SSO"
			}
			return "joined the org"
		}
		return "granted " + target + " org membership"
	case domain.AccessActionOrgRoleChanged:
		return "changed " + target + " from " + orFallback(d.OldRole, "an unknown role") +
			" to " + orFallback(d.NewRole, "an unknown role")
	case domain.AccessActionOrgMemberRevoked:
		return "removed " + target + " from the org"
	case domain.AccessActionOrgOwnershipTransferred:
		return "transferred org ownership to " + target
	case domain.AccessActionTeamMemberAdded:
		if d.NewRole != "" {
			return "added " + target + " to " + team + " as " + d.NewRole
		}
		return "added " + target + " to " + team
	case domain.AccessActionTeamRoleChanged:
		return "changed " + target + " from " + orFallback(d.OldRole, "an unknown role") +
			" to " + orFallback(d.NewRole, "an unknown role") + " on " + team
	case domain.AccessActionTeamMemberRemoved:
		return "removed " + target + " from " + team
	case domain.AccessActionInviteCreated:
		invited := "invited " + orFallback(d.Email, "someone")
		if d.Role != "" {
			return invited + " as " + d.Role
		}
		return invited
	case domain.AccessActionInviteRevoked:
		if d.Email != "" {
			return "revoked the invite for " + d.Email
		}
		return "revoked a pending invite"
	case domain.AccessActionCredentialSet:
		return credentialActionLabel("set the ", d)
	case domain.AccessActionCredentialRemoved:
		return credentialActionLabel("removed the ", d)
	case domain.AccessActionEventSourceDisabled:
		return "turned off " + eventSourcePhrase(d) + " for this org"
	case domain.AccessActionEventSourceEnabled:
		return "turned " + eventSourcePhrase(d) + " back on for this org"
	case domain.AccessActionSSOConnectionCreated:
		return "registered an SSO connection"
	case domain.AccessActionSSOConnectionEnabled:
		return "enabled SSO"
	case domain.AccessActionSSOConnectionDisabled:
		return "disabled SSO"
	case domain.AccessActionSSOEnforcementEnabled:
		return "started requiring SSO for this org"
	case domain.AccessActionSSOEnforcementDisabled:
		return "stopped requiring SSO for this org"
	case domain.AccessActionSSODomainClaimed:
		return "claimed " + ssoDomainPhrase(d) + " for SSO"
	case domain.AccessActionSSODomainVerified:
		return "verified " + ssoDomainPhrase(d)
	case domain.AccessActionSSODomainRemoved:
		return "removed " + ssoDomainPhrase(d)
	case domain.AccessActionSSOBreakGlassAdded:
		return "added " + target + " as an SSO break-glass principal"
	case domain.AccessActionSSOBreakGlassRemoved:
		return "removed " + target + " from the SSO break-glass principals"
	case domain.AccessActionAPITokenCreated:
		return apiTokenCreatedLabel(d)
	case domain.AccessActionAPITokenRevoked:
		if d.Source == domain.AccessSourceMembershipRemoved {
			// The deprovisioning hook wrote this: the actor removed the target
			// from the org, and the token went with the membership. Read as
			// the consequence it is, not as a revoke somebody chose.
			return "revoked " + target + "'s API token " + apiTokenPhrase(d) + " with their org membership"
		}
		return "revoked API token " + apiTokenPhrase(d)
	default:
		// An unrecognized discriminator (forward-compat: the column has no CHECK)
		// shows raw so the row still renders meaningfully.
		return e.Action
	}
}

// parseAccessDetail unmarshals a row's detail_json leniently — empty or malformed
// JSON yields the zero value, so a label never errors on bad detail.
func parseAccessDetail(raw string) accessDetail {
	var d accessDetail
	if raw == "" {
		return d
	}
	_ = json.Unmarshal([]byte(raw), &d)
	return d
}

// ssoDomainPhrase names the domain an sso_domain_* row is about, degrading to a
// bare noun when the detail didn't survive. Written as a whole phrase rather
// than a fallback word so the generic case still reads as English ("removed a
// domain", not "removed the domain a domain").
func ssoDomainPhrase(d accessDetail) string {
	if d.Domain == "" {
		return "a domain"
	}
	return "the domain " + d.Domain
}

// eventSourcePhrase names the source an event_source_* row is about, degrading
// to a bare noun when the detail didn't survive. A whole phrase rather than a
// fallback word, for ssoDomainPhrase's reason: the generic case still has to
// read as English.
func eventSourcePhrase(d accessDetail) string {
	if d.Kind == "" {
		return "an event source"
	}
	return d.Kind + " events"
}

// apiTokenPhrase names a token the way its owner recognises it: the name they
// gave it, and the visible head of the secret in case two share a name (names
// are deliberately not unique — a replacement may share one). Either may be
// missing from an older or malformed row; the phrase degrades rather than
// failing.
func apiTokenPhrase(d accessDetail) string {
	name := orFallback(d.Name, "(unnamed)")
	if d.Prefix == "" {
		return name
	}
	return name + " (" + d.Prefix + "…)"
}

// apiTokenCreatedLabel renders the mint row with the whole of what bounded the
// token at that moment — the expiry asked for, the org cap in force, and the
// allowlist — each omitted when absent, since absent means unbounded.
func apiTokenCreatedLabel(d accessDetail) string {
	s := "created API token " + apiTokenPhrase(d)
	if d.ExpiresAt != nil {
		s += " expiring " + d.ExpiresAt.UTC().Format("2 Jan 2006")
	} else {
		s += " with no expiry"
	}
	if d.MaxAgeDays != nil && *d.MaxAgeDays > 0 {
		s += fmt.Sprintf(", under the org's %d-day cap", *d.MaxAgeDays)
	}
	switch n := len(d.AllowedCIDRs); {
	case n == 1:
		s += ", accepted from 1 IP range"
	case n > 1:
		s += fmt.Sprintf(", accepted from %d IP ranges", n)
	}
	return s
}

// credentialActionLabel renders a credential_set / credential_removed predicate
// from the shared {kind, host, name} detail — verb ("set the " / "removed the ")
// + the kind's human name, qualified by the specific credential's name and/or
// host when the write-point had them ("set the GitHub App acme-bot on
// github.example.com").
func credentialActionLabel(verb string, d accessDetail) string {
	s := verb + credentialKindLabel(d.Kind)
	if d.Name != "" {
		s += " " + d.Name
	}
	if d.Host != "" {
		s += " for " + d.Host
	}
	return s
}

// credentialKindLabel maps a credential kind to its human name.
func credentialKindLabel(kind string) string {
	switch kind {
	case domain.CredentialKindGitHubPAT:
		return "GitHub PAT"
	case domain.CredentialKindGitHubApp:
		return "GitHub App"
	case domain.CredentialKindGitHubIdentity:
		return "personal GitHub identity"
	case domain.CredentialKindJiraOrg:
		return "Jira credential"
	case domain.CredentialKindJiraUser:
		return "personal Jira credential"
	case domain.CredentialKindJiraOAuthApp:
		return "Atlassian OAuth app"
	case domain.CredentialKindAnthropicKey:
		return "Anthropic API key"
	case domain.CredentialKindBedrock:
		return "Bedrock credentials"
	case domain.CredentialKindSlackWorkspace:
		return "Slack workspace"
	default:
		return "credential"
	}
}

// orFallback returns s, or fallback when s is empty.
func orFallback(s, fallback string) string {
	if s == "" {
		return fallback
	}
	return s
}

// rawJSONOrNil wraps a detail_json string for passthrough, returning nil (→
// field omitted) when it's empty OR not valid JSON.
//
// The validity check is load-bearing: detail_json is a free-form TEXT column (no
// JSON CHECK), and json.RawMessage is emitted by the encoder's compaction step
// without per-field error isolation — a single malformed value would fail the
// WHOLE response marshal, and httpx.WriteJSON has already sent a 200 by then, so
// the client gets an empty body and the entire page of audit rows dies. Omitting
// an invalid detail degrades that one row to "no detail" instead; its
// action_label still renders (parseAccessDetail is independently lenient). In
// practice every write-point builds detail_json with json.Marshal, so this only
// fires on a corrupt/hand-edited row — exactly where an audit viewer must not
// fall over.
func rawJSONOrNil(s string) json.RawMessage {
	if s == "" || !json.Valid([]byte(s)) {
		return nil
	}
	return json.RawMessage(s)
}
