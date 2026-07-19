package domain

import "time"

// AuthEvent is one row in auth_events — the SOC2 authentication audit log of
// record. Durable, append-only capture of every authentication / session
// outcome: login, logout, refresh failure, JWT-verify failure, SSO enforcement,
// break-glass. The AUTHENTICATION sibling of AccessChange (the
// authorization-CHANGE log); together they are the two halves of the SOC2
// access-controls surface (CC6.1 logical access, CC7.2 monitoring).
//
// Uniform BEST-EFFORT: a row is recorded AFTER the auth action it describes; a
// write failure is logged at ERROR and swallowed, never failing or rolling back
// the auth action (the recorder helper owns that contract — see
// (*server.Server).recordAuthEvent). Append-only — never UPDATE/DELETE.
//
// Every optional string field is empty → SQL NULL. OrgID is empty for a pre-auth
// failure (no org yet); UserID is empty for a pre-identity failure (bad state
// cookie, exchange/verify failure before the principal resolves); SessionID is
// empty when no session exists (a failure, or a pre-session event). ID +
// CreatedAt are populated on read — the column DEFAULTs stamp them (PG
// gen_random_uuid()/now(), SQLite app-generated id / CURRENT_TIMESTAMP). See
// TFAC-76.
type AuthEvent struct {
	ID         string    `json:"id"`
	OrgID      string    `json:"org_id,omitempty"`     // nullable → NULL
	UserID     string    `json:"user_id,omitempty"`    // nullable → NULL
	SessionID  string    `json:"session_id,omitempty"` // nullable → NULL
	EventType  string    `json:"event_type"`
	IPAddress  string    `json:"ip_address,omitempty"`  // nullable → NULL
	UserAgent  string    `json:"user_agent,omitempty"`  // nullable → NULL
	DetailJSON string    `json:"detail_json,omitempty"` // nullable → NULL
	CreatedAt  time.Time `json:"created_at"`
}

// Authentication-event discriminators (text, extensible — no CHECK constraint on
// the column, Go consts here). login_failure is ONE type with the failing branch
// in detail_json {"reason":…} — NOT a type per branch. The …Rejected /
// BreakGlass pair are Enterprise-Edition events recorded through the
// RecordAuthEvent extension seam (ee/sso has no direct store access). See
// TFAC-76 §write-sites.
const (
	AuthEventLoginSuccess = "login_success"
	AuthEventLoginFailure = "login_failure"
	// AuthEventSeatLimitRejected marks a login denied by the per-seat license
	// cap (maxSeats) — a new distinct user beyond the committed cap for the
	// billing period. Recorded by CORE (not the EE extension seam): enforcement
	// lives in the core login path and reads the cap through the entitlements
	// seam. Detail carries {"max_seats":N,"active_users":M,"period":"YYYY-MM"}.
	AuthEventSeatLimitRejected      = "seat_limit_rejected"
	AuthEventLogout                 = "logout"
	AuthEventLogoutAll              = "logout_all"
	AuthEventRefreshFailure         = "refresh_failure"
	AuthEventJWTVerifyFailure       = "jwt_verify_failure"
	AuthEventSSOEnforcementRejected = "sso_enforcement_rejected" // EE
	AuthEventBreakGlassLogin        = "break_glass_login"        // EE
	// Deployment-operator grant/revoke (TFAC-589), recorded by the
	// `triagefactory operator` CLI. Org-less (an operator is deployment
	// config, not a member role), so these land here with a NULL org rather
	// than in the org-scoped access_change_log — auth_events is the durable
	// SOC2 record with nullable org, the right home for a deployment-wide
	// access change. UserID is empty (a shell operator, not a session user);
	// the granted email + the OS actor ride detail_json.
	AuthEventOperatorGranted = "operator_granted"
	AuthEventOperatorRevoked = "operator_revoked"
)

// AuthEventListOpts bounds + filters a read for tests and the deferred viewer
// (no HTTP surface in TFAC-76). Rows come back newest-first (matching the
// (…, created_at DESC) indexes). The zero value lists every event with no filter
// or paging.
type AuthEventListOpts struct {
	// Limit caps rows returned (≤ 0 → no limit; a viewer passes a page size).
	// Offset skips the first N rows for limit/offset paging (only meaningful
	// alongside a positive Limit).
	Limit, Offset int
	// EventType is an optional exact-match filter on event_type; "" = all.
	EventType string
	// Since / Until bound created_at to the half-open window [Since, Until).
	// Each side applies only when non-zero, so the zero value is unbounded.
	Since, Until time.Time
}
