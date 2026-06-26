package domain

import "time"

type OrgGitHubApp struct {
	OrgID            string
	AppID            string
	Slug             string
	ClientID         string
	ClientSecretRef  string
	PEMRef           string
	WebhookSecretRef string
	// OwnerType is "user" when the App was registered under a personal
	// GitHub account, "org" when registered under an organization. Captured
	// from the signed registration state at callback time (not a fresh query
	// param) and surfaced through the status endpoint so Setup/Settings can
	// seed the "App account type" summary instead of re-defaulting to
	// "Personal account" on every page load. Mirrors the
	// org_github_apps.owner_type column (NOT NULL DEFAULT 'user').
	OwnerType          string
	RegisteredAt       time.Time
	RegisteredByUserID string
	Active             bool
	// BotUserID is the numeric GitHub *user-account* id of the App's bot
	// (the account "<slug>[bot]"), fetched best-effort at registration via
	// GET /users/<slug>[bot]. It is NOT the App id — the App id and the bot
	// user id are distinct numbers. 0 means unknown (an App registered before
	// this column existed, or a registration-time fetch failure); the commit
	// identity then falls back to the plain "<slug>[bot]@users.noreply.github.com"
	// form. When known, the resolver builds the numeric-id noreply email
	// "<bot_user_id>+<slug>[bot]@users.noreply.github.com", the only form that
	// links a bot's commits to its account on GitHub's contribution graph
	// (TFAC-474). Mirrors org_github_apps.bot_user_id (nullable → 0).
	BotUserID int64
}

// NormalizedOwnerType returns the App's owner_type, folding an unset value
// to "user" so the store never writes an empty string into the NOT NULL
// owner_type column (whose DB default is likewise "user"). The registration
// callback always supplies a validated "user"/"org"; this guards any other
// caller that constructs the struct without setting the field.
func (a OrgGitHubApp) NormalizedOwnerType() string {
	if a.OwnerType == "" {
		return "user"
	}
	return a.OwnerType
}

// OrgGitHubAppInstallation is one place the org's App is installed —
// a GitHub account (org or user) that ran the install flow. Mirrored
// from GitHub via the installation webhook (multi mode) or API
// backfill (local mode). One App can have many installations.
type OrgGitHubAppInstallation struct {
	InstallationID string
	OrgID          string
	AccountType    string
	AccountLogin   string
	InstalledAt    time.Time
}
