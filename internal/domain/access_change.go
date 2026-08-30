package domain

import (
	"encoding/json"
	"time"
)

// AccessChange is one row in access_change_log — a governance action that has
// no external entity: org/team membership & role grants/changes/revokes, the
// invite lifecycle, and credential bind/rotate/remove (GitHub PAT + App +
// per-user identity, Jira org + per-user + OAuth app, Anthropic key, Bedrock).
// Append-only, low-volume, org-scoped. The capture layer for the
// org-governance / team-activity audit view.
//
// Each row is written in the SAME transaction as the action it records, so the
// log can't diverge from reality — a log-write failure rolls the action back.
//
// Actor is the request's authenticated user (ClaimsFrom(ctx).Subject); empty
// serializes to SQL NULL for the rare system/bootstrap write with no actor. On
// invite-accept the actor is the invitee, with the invite id carried in
// DetailJSON. Target/Team/DetailJSON are likewise optional (empty → NULL).
// OrgID + ID + CreatedAt are populated on read; Record takes orgID as a
// separate argument and lets the column DEFAULTs stamp id + created_at.
type AccessChange struct {
	ID    string `json:"id"`
	OrgID string `json:"org_id"`
	// ActorUserID is who performed the action (claims subject). Empty for a
	// system/bootstrap write with no human actor → SQL NULL.
	ActorUserID string `json:"actor_user_id,omitempty"`
	// Action is the discriminator — one of the Access* constants below. Free
	// text (extensible — no CHECK constraint on the column).
	Action string `json:"action"`
	// TargetUserID is the subject of a membership/role action (the member
	// granted/changed/revoked). Empty for credential actions → SQL NULL.
	TargetUserID string `json:"target_user_id,omitempty"`
	// TeamID is set for team-scoped actions (and for an org grant that also
	// places the member on a team). Empty → SQL NULL.
	TeamID string `json:"team_id,omitempty"`
	// DetailJSON carries the action-specific payload, e.g.
	// {"old_role":"member","new_role":"admin"} for a role change,
	// {"kind":"github_pat","host":"..."} for a credential set,
	// {"invite_id":"..."} for an invite-accept grant. Empty → SQL NULL.
	DetailJSON string    `json:"detail_json,omitempty"`
	CreatedAt  time.Time `json:"created_at"`
}

// Access-change action discriminators (text, extensible — no CHECK constraint).
const (
	AccessActionOrgMemberGranted        = "org_member_granted"
	AccessActionOrgRoleChanged          = "org_role_changed"
	AccessActionOrgMemberRevoked        = "org_member_revoked"
	AccessActionOrgOwnershipTransferred = "org_ownership_transferred"
	AccessActionTeamMemberAdded         = "team_member_added"
	AccessActionTeamRoleChanged         = "team_role_changed"
	AccessActionTeamMemberRemoved       = "team_member_removed"
	AccessActionInviteCreated           = "invite_created"
	AccessActionInviteRevoked           = "invite_revoked"
	AccessActionCredentialSet           = "credential_set"
	AccessActionCredentialRemoved       = "credential_removed"
	// The event-source pause and its undo. Beside the credential pair
	// deliberately: an admin reaching for one of these is often deciding
	// between them, and the log has to make plain which one they picked —
	// unbinding a credential also cuts the agent's access to the source, while
	// a pause leaves it alone and only stops events.
	AccessActionEventSourceDisabled = "event_source_disabled"
	AccessActionEventSourceEnabled  = "event_source_enabled"
	// The API-token lifecycle. A token is a credential — it authenticates as
	// its owner within one org — so it records here rather than in auth_events,
	// which holds logins: one home per fact. Mint and revoke are separate
	// discriminators for the same reason the SSO pairs are: an admin scanning
	// this log is looking for the moment access widened.
	AccessActionAPITokenCreated = "api_token_created"
	AccessActionAPITokenRevoked = "api_token_revoked"
)

// SSO policy action discriminators. Declared here, in core, even though every
// write-point lives in ee/sso — the same split as EventSlackMessage: an
// out-of-core surface produces the rows, but core's audit viewer must render
// them, and the open-core boundary points inward (core can never import ee). The
// constants are the contract the two halves meet on.
//
// Enable/disable and enforce/unenforce get their own discriminators rather than
// one action with a boolean in the detail, because the direction is the whole
// point of the row: an admin scanning this log is looking for the moment access
// widened, and the viewer tones a row on its action alone.
const (
	AccessActionSSOConnectionCreated   = "sso_connection_created"
	AccessActionSSOConnectionEnabled   = "sso_connection_enabled"
	AccessActionSSOConnectionDisabled  = "sso_connection_disabled"
	AccessActionSSOEnforcementEnabled  = "sso_enforcement_enabled"
	AccessActionSSOEnforcementDisabled = "sso_enforcement_disabled"
	AccessActionSSODomainClaimed       = "sso_domain_claimed"
	AccessActionSSODomainVerified      = "sso_domain_verified"
	AccessActionSSODomainRemoved       = "sso_domain_removed"
	AccessActionSSOBreakGlassAdded     = "sso_break_glass_added"
	AccessActionSSOBreakGlassRemoved   = "sso_break_glass_removed"
)

// Access-change "source" values carried in an org_member_granted action's
// DetailJSON {"source":...} when the grant was AUTOMATED rather than an
// interactive invite-accept. The viewer uses it to render "joined via SSO"
// instead of the plain "joined" an invite-accept shows; an empty source (the
// invite-accept case) keeps the generic phrasing. SCIM (future) grafts onto the
// same grantOrgMembership seam and records its own source.
const (
	AccessSourceSSOJIT = "sso_jit"
)

// AccessSourceMembershipRemoved is the "source" carried in an
// api_token_revoked action's DetailJSON when the revocation was the
// deprovisioning hook rather than the owner deleting their own token: removing
// a member from an org revokes the tokens they held in it, one row per token.
// An absent source means a human revoked it deliberately, which is the case
// the viewer phrases plainly.
const (
	AccessSourceMembershipRemoved = "membership_removed"
)

// Credential kinds carried in a credential_set / credential_removed action's
// DetailJSON {"kind":...}. Bind-vs-rotate isn't reliably distinguishable at the
// write-point, so every credential write records credential_set and lets the
// detail carry the kind (and host / name where cheap); an unbind, clear, or
// teardown records credential_removed with the same kind.
//
// GitHubIdentity is the per-user GitHub *identity* binding (PAT_2 / the Connect
// OAuth flow). No token is retained for it — the binding itself is the access
// fact an admin audits ("this TF user acts as @login"), so it shares the
// credential category rather than getting a third one.
//
// SlackWorkspace is the whole connected workspace, not one token: a Slack
// connect binds a bot token and (per transport) a signing secret and app-level
// token, and a disconnect sweeps all three together. They are acquired,
// rotated, and destroyed as a unit from a single admin action, so they record
// as one row — three rows per connect would be noise in a log an admin scans.
const (
	CredentialKindGitHubPAT      = "github_pat"
	CredentialKindGitHubApp      = "github_app"
	CredentialKindGitHubIdentity = "github_identity"
	CredentialKindJiraOrg        = "jira_org"
	CredentialKindJiraUser       = "jira_user"
	CredentialKindJiraOAuthApp   = "jira_oauth_app"
	CredentialKindAnthropicKey   = "anthropic_key"
	CredentialKindBedrock        = "bedrock"
	CredentialKindSlackWorkspace = "slack_workspace"
)

// AccessChangeListOpts bounds a ListByOrg read for the audit viewer. Rows come
// back newest-first (matching the (org_id, created_at DESC) index).
type AccessChangeListOpts struct {
	// Limit is the page size. A value ≤ 0 falls back to the store impls' own
	// internal default (100) — but the HTTP viewer resolves a concrete page size
	// before it calls, so that 100 fallback only applies to a direct ListByOrg
	// caller. The viewer requests Limit+1 and peeks at the extra row to learn
	// whether a next page exists, without a separate COUNT.
	Limit int
	// Offset skips the first N rows for pagination. 0 (or negative, clamped by
	// the caller) is the first page.
	Offset int
	// Category, when one of AccessCategory*, narrows the read to that bucket's
	// action discriminators (via AccessActionsInCategory). An empty or
	// unrecognized value means no category filter — every action.
	Category string
}

// Access-change filter categories for the audit viewer. The viewer
// groups the action discriminators into buckets so an org admin can narrow the
// log to membership/role/ownership changes vs credential binds/rotations vs
// org-wide access policy.
//
// Policy is the bucket for org-wide switches that name no member and store no
// secret — the SSO connection lifecycle, its enforcement switch, the verified
// domains that decide who auto-provisions, the break-glass exemptions from
// enforcement, and the per-source event pause. Break-glass names a target user
// but grants no membership: it exempts an existing member from the SSO
// requirement, which is a property of the policy, not of their standing in the
// org. The event-source pause names no principal at all — it decides what the
// org ingests, not who gets in — and lands here for the same reason: it is a
// deliberate org-wide setting an admin flipped, and an admin auditing "what
// changed about how this org behaves" is looking for exactly that.
const (
	AccessCategoryMembership = "membership"
	AccessCategoryCredential = "credential"
	AccessCategoryPolicy     = "policy"
)

// AccessActionsInCategory returns the action discriminators that make up a
// filter category, or nil for "" / an unrecognized category (the caller treats
// nil as "no filter — every action"). Membership covers every org/team
// grant / role-change / revoke / ownership-transfer plus the invite lifecycle
// that precedes a grant; credential covers binds and removals alike. Keeping the
// grouping here, next to the action constants, means a newly-added action is
// classified in exactly one place.
func AccessActionsInCategory(category string) []string {
	switch category {
	case AccessCategoryMembership:
		return []string{
			AccessActionOrgMemberGranted,
			AccessActionOrgRoleChanged,
			AccessActionOrgMemberRevoked,
			AccessActionOrgOwnershipTransferred,
			AccessActionTeamMemberAdded,
			AccessActionTeamRoleChanged,
			AccessActionTeamMemberRemoved,
			AccessActionInviteCreated,
			AccessActionInviteRevoked,
		}
	case AccessCategoryCredential:
		return []string{
			AccessActionCredentialSet, AccessActionCredentialRemoved,
			AccessActionAPITokenCreated, AccessActionAPITokenRevoked,
		}
	case AccessCategoryPolicy:
		return []string{
			AccessActionSSOConnectionCreated,
			AccessActionSSOConnectionEnabled,
			AccessActionSSOConnectionDisabled,
			AccessActionSSOEnforcementEnabled,
			AccessActionSSOEnforcementDisabled,
			AccessActionSSODomainClaimed,
			AccessActionSSODomainVerified,
			AccessActionSSODomainRemoved,
			AccessActionSSOBreakGlassAdded,
			AccessActionSSOBreakGlassRemoved,
			AccessActionEventSourceDisabled,
			AccessActionEventSourceEnabled,
		}
	default:
		return nil
	}
}

// --- detail_json builders shared across the open-core boundary ---
//
// The audit row's detail payload is a contract between whoever writes the row
// and the viewer that renders it. Where those two sit in different halves of
// the open-core split — every ee write-point — the shape has to live somewhere
// both can import, which is here. Core-only payloads (role changes, the invite
// lifecycle, the SSO JIT grant) stay next to their write-points in the server
// package; only the shapes ee produces are exported.

// AccessDetailCredential builds the {kind, host?, name?} payload every
// credential_set / credential_removed row carries. host and name are omitted
// when empty — a host-less credential (an API key) and an unnamed one are both
// ordinary.
func AccessDetailCredential(kind, host, name string) string {
	b, _ := json.Marshal(struct {
		Kind string `json:"kind"`
		Host string `json:"host,omitempty"`
		Name string `json:"name,omitempty"`
	}{Kind: kind, Host: host, Name: name})
	return string(b)
}

// AccessDetailEventSource builds the payload for an event-source pause or its
// undo: the source kind, and nothing else. Which direction it went is the
// action discriminator's job, not a field's — a boolean here would let a row
// say "enabled" and "disabled" at once.
func AccessDetailEventSource(kind string) string {
	b, _ := json.Marshal(struct {
		Kind string `json:"kind"`
	}{Kind: kind})
	return string(b)
}

// AccessDetailSlackWorkspace builds the credential payload for a Slack
// workspace connect/disconnect. The workspace name is the handle an admin
// recognizes, so it rides as the row's name; the two ids ride alongside because
// the name is a Slack-side display string that can change (and two workspaces
// may share one), while (workspace_id, api_app_id) is what actually identifies
// the connection.
func AccessDetailSlackWorkspace(name, workspaceID, apiAppID string) string {
	b, _ := json.Marshal(struct {
		Kind        string `json:"kind"`
		Name        string `json:"name,omitempty"`
		WorkspaceID string `json:"workspace_id,omitempty"`
		APIAppID    string `json:"api_app_id,omitempty"`
	}{
		Kind:        CredentialKindSlackWorkspace,
		Name:        name,
		WorkspaceID: workspaceID,
		APIAppID:    apiAppID,
	})
	return string(b)
}

// AccessDetailSSOConnection builds the payload for the SSO connection lifecycle
// rows. The GoTrue provider id is the only durable handle on the connection —
// the sso_connections row may be gone by the time the log is read.
func AccessDetailSSOConnection(providerID string) string {
	b, _ := json.Marshal(struct {
		ProviderID string `json:"provider_id,omitempty"`
	}{ProviderID: providerID})
	return string(b)
}

// AccessDetailSSODomain builds the payload for the domain claim/verify/remove
// rows. The domain itself is the fact being audited, and the row outlives the
// sso_domains row it describes, so it is captured rather than referenced by id.
func AccessDetailSSODomain(domain string) string {
	b, _ := json.Marshal(struct {
		Domain string `json:"domain,omitempty"`
	}{Domain: domain})
	return string(b)
}

// AccessDetailAPITokenCreated builds the payload for an api_token_created row:
// which token, what it was called, and the whole of what bounds it — the
// expiry the minter asked for, the org cap in force at that moment, and any IP
// allowlist. The cap is captured because it is applied at USE against whatever
// the setting says then; recording it here is what lets an admin read back the
// policy the token was minted under rather than today's.
//
// Every bound is omitted when absent, and absent means unbounded: no expiry, no
// cap, no allowlist. Only the token's id, name and prefix are ever required —
// the secret itself exists in one place and it is not this one.
//
// This builder lives in domain rather than beside the audit helpers in
// internal/server because internal/apitokens writes the row (it composes the
// audit INSERT into the same transaction as the token write, and importing the
// server package for a shape would be a cycle) while the audit viewer renders
// it. Two writers, one shape.
func AccessDetailAPITokenCreated(tokenID, name, prefix string, expiresAt *time.Time, maxAgeDays *int, allowedCIDRs []string) string {
	b, _ := json.Marshal(struct {
		TokenID      string     `json:"token_id"`
		Name         string     `json:"name"`
		Prefix       string     `json:"prefix"`
		ExpiresAt    *time.Time `json:"expires_at,omitempty"`
		MaxAgeDays   *int       `json:"max_age_days,omitempty"`
		AllowedCIDRs []string   `json:"allowed_cidrs,omitempty"`
	}{
		TokenID:      tokenID,
		Name:         name,
		Prefix:       prefix,
		ExpiresAt:    expiresAt,
		MaxAgeDays:   maxAgeDays,
		AllowedCIDRs: allowedCIDRs,
	})
	return string(b)
}

// AccessDetailAPITokenRevoked builds the payload for an api_token_revoked row.
// It names the token the same way the created row did, so the two ends of a
// token's life read as one story even after the row itself is reaped.
//
// source is AccessSourceMembershipRemoved when the deprovisioning hook revoked
// the token, and empty when its owner did — the distinction the viewer needs to
// say "revoked" versus "revoked because they left the org".
func AccessDetailAPITokenRevoked(tokenID, name, prefix, source string) string {
	b, _ := json.Marshal(struct {
		TokenID string `json:"token_id"`
		Name    string `json:"name"`
		Prefix  string `json:"prefix"`
		Source  string `json:"source,omitempty"`
	}{TokenID: tokenID, Name: name, Prefix: prefix, Source: source})
	return string(b)
}
