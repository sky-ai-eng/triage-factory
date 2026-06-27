package server

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/entitlements"
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

const (
	// accessLogDefaultLimit is the page size when the caller omits / malforms
	// limit; accessLogMaxLimit caps it so one request can't scan the whole log.
	accessLogDefaultLimit = 50
	accessLogMaxLimit     = 200
)

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

// accessLogResponse is one page of the audit log, newest-first. HasMore drives
// the viewer's "Older" pager without a COUNT — the handler reads Limit+1 rows and
// reports HasMore when the extra row came back.
type accessLogResponse struct {
	Items   []accessChangeJSON `json:"items"`
	Limit   int                `json:"limit"`
	Offset  int                `json:"offset"`
	HasMore bool               `json:"has_more"`
}

// handleUsageAccessLog serves the EE access & credential change-log viewer.
//
// Gate: org admin AND FeatureGovernance. The order is deliberate — the org-admin
// check runs first (403 on a non-admin), THEN the entitlement check (a 404-and-
// hide when unlicensed). Admin-first means a non-admin always gets a 403 and
// never learns the deployment's license tier; an org admin on an unlicensed build
// gets a 404 that reads as "no such route", so the feature stays invisible to
// everyone who isn't entitled to see it. Local mode short-circuits the admin gate
// to allowed (N=1), so a licensed local build still serves it.
//
// The read runs on the app pool under the admin's claims: access_change_log's
// org-scoped RLS (org_id = current_org_id() AND user_has_org_access) admits the
// admin's own org and nothing else — the org-admin gate is the authorization for
// the org-wide lens, RLS the defense-in-depth.
//
// GET /api/usage/org/access-log?limit=&offset=&category=
func (h *usageHandler) handleUsageAccessLog(w http.ResponseWriter, r *http.Request) {
	orgID, userID, ok := h.resolveCaller(w, r)
	if !ok {
		return
	}
	if !h.az.RequireOrgAdminRole(w, r, orgID, userID) {
		return
	}
	if !entitlements.Active().Has(entitlements.FeatureGovernance) {
		http.NotFound(w, r)
		return
	}

	q := r.URL.Query()
	limit := parseAccessLogLimit(q.Get("limit"))
	offset := parseAccessLogOffset(q.Get("offset"))
	category := q.Get("category")

	var (
		rows  []domain.AccessChange
		names map[string]string // user id -> display name (actors + targets)
		teams map[string]string // team id -> team name
	)
	if err := h.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		var e error
		// Peek one row past the page so HasMore needs no separate COUNT.
		rows, e = tx.AccessChangeLog.ListByOrg(r.Context(), orgID, domain.AccessChangeListOpts{
			Limit: limit + 1, Offset: offset, Category: category,
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

	hasMore := len(rows) > limit
	if hasMore {
		rows = rows[:limit]
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
	writeJSON(w, http.StatusOK, accessLogResponse{
		Items: items, Limit: limit, Offset: offset, HasMore: hasMore,
	})
}

// --- paging param parsing ---

// parseAccessLogLimit resolves the page size: a missing / malformed / non-positive
// value falls back to the default, and anything over the cap is clamped down.
func parseAccessLogLimit(s string) int {
	n, err := strconv.Atoi(s)
	if err != nil || n <= 0 {
		return accessLogDefaultLimit
	}
	if n > accessLogMaxLimit {
		return accessLogMaxLimit
	}
	return n
}

// parseAccessLogOffset resolves the row offset: a missing / malformed / negative
// value is the first page (0).
func parseAccessLogOffset(s string) int {
	n, err := strconv.Atoi(s)
	if err != nil || n < 0 {
		return 0
	}
	return n
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
	InviteID string `json:"invite_id"`
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
		// Invite-accept writes actor == target (the invitee redeemed the link),
		// which reads as "joined"; a future direct-grant path would differ.
		if e.ActorUserID != "" && e.ActorUserID == e.TargetUserID {
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
	case domain.AccessActionCredentialSet:
		kind := credentialKindLabel(d.Kind)
		if d.Host != "" {
			return "set the " + kind + " for " + d.Host
		}
		return "set the " + kind
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

// credentialKindLabel maps a credential_set kind to its human name.
func credentialKindLabel(kind string) string {
	switch kind {
	case domain.CredentialKindGitHubPAT:
		return "GitHub PAT"
	case domain.CredentialKindJiraOrg:
		return "Jira credential"
	case domain.CredentialKindJiraUser:
		return "personal Jira credential"
	case domain.CredentialKindAnthropicKey:
		return "Anthropic API key"
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
