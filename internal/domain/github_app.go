package domain

import (
	"fmt"
	"strings"
	"time"
)

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
	// AccountID is GitHub's numeric id for the account the App is installed
	// on, in its text form (the convention entities.source_id and
	// InstallationID already follow). Unlike AccountLogin it survives a
	// rename, so it — not the login — is what credential resolution matches
	// on when both sides know it. "" means unknown: a row mirrored before
	// this was captured, which the next reconcile or webhook fills in.
	AccountID    string
	AccountLogin string
	// GitHubHost is the GitHub deployment this installation lives on — the
	// org's github_base_url normalized the way every other GitHub host key is
	// (db.EffectiveGitHubHost), so the same GitHub is the same string here as
	// in user_github_identities. It is on the row because an installation id is
	// unique per deployment and not universally: a self-host aggregating orgs
	// across two GHES instances can hold the same numeric id twice, meaning two
	// unrelated installations, and telling them apart through a join to the
	// owning org's settings is a join too many for any comparison that spans
	// orgs. "" on a struct handed to a writer means "resolve it" — the store
	// folds it to the public host, which is what an org with no configured base
	// URL is on — so reads never see one.
	GitHubHost  string
	InstalledAt time.Time
	// SuspendedAt is when the account owner suspended this installation, zero
	// when it is not suspended. A suspension is not an uninstall: the grant
	// survives, the row stays active (removed_at is untouched), and the
	// installation resumes on unsuspend — but every token minted from it is
	// refused while it lasts, so a suspended installation must be
	// distinguishable from a working one wherever the mirror is read.
	SuspendedAt time.Time
	// SuspendedBy is the GitHub login that suspended the installation, from the
	// payload's suspended_by user object. Display provenance only — nothing
	// resolves on it — and "" when the source named no one.
	SuspendedBy string
	// RepositorySelection is RepositorySelectionAll or
	// RepositorySelectionSelected — whether the grant is every repository on the
	// account or an enumerated set. It is what says whether scope drift is even
	// POSSIBLE for this installation: an 'all' grant reaches everything the
	// account owns, so a tracked repository on that account cannot sit outside
	// it, and "no drift found" there means something different from "no drift
	// found" under a selective grant.
	//
	// "" means unknown — a row mirrored before this was captured — and takes the
	// account id's fill-in-only write rule rather than the login's overwrite
	// one, because a writer that omits it is a writer that did not look.
	RepositorySelection string
}

// The two values GitHub reports for an installation's repository_selection.
const (
	RepositorySelectionAll      = "all"
	RepositorySelectionSelected = "selected"
)

// NormalizeRepositorySelection canonicalizes GitHub's repository_selection for
// storage. Empty means the caller didn't say, which is a supported state (the
// column is nullable and NULL reads back as ""). Anything outside GitHub's two
// values is refused rather than stored: the CHECK constraint would refuse it
// anyway, and failing here names the bad value instead of surfacing a driver
// error from three layers down.
func NormalizeRepositorySelection(selection string) (string, error) {
	s := strings.ToLower(strings.TrimSpace(selection))
	switch s {
	case "", RepositorySelectionAll, RepositorySelectionSelected:
		return s, nil
	default:
		return "", fmt.Errorf("unknown repository selection %q", selection)
	}
}

// Suspended reports whether the installation is currently suspended. The
// timestamp is the state (there is no separate flag to fall out of step with
// it), so every reader asks this rather than re-deriving the zero check.
func (i OrgGitHubAppInstallation) Suspended() bool { return !i.SuspendedAt.IsZero() }

// GrantsEveryRepository reports whether this installation's grant covers every
// repository on the account. Callers asking "can this installation drift?" ask
// this rather than comparing the string, and an unknown selection ("") answers
// false — the conservative side, since it renders as "we have not established
// that drift is impossible" rather than as an all-clear.
func (i OrgGitHubAppInstallation) GrantsEveryRepository() bool {
	return i.RepositorySelection == RepositorySelectionAll
}

// ReachableRepository is one entry in an org's reachable-repo cache — a
// repository the org's GitHub credentials can reach, whether or not any team
// tracks it.
//
// It is deliberately NOT a Repository: reachability is a cache of an external
// fact, rebuilt in full on every refresh, while a repository row is a registry
// of a TF entity that worktrees, entities and clone state hang off. A reachable
// entry with no registry row behind it is the normal case — under an App it is
// exactly what "reach without purpose" means, and under a PAT it is every
// repository the user can see but nobody tracks — so this type carries
// everything a reader needs without a join.
type ReachableRepository struct {
	OrgID string
	// CredentialClass is which credential system observed this reach. Every
	// read filters on the org's current class, so a tier switch cannot serve the
	// previous tier's answer alongside the new one.
	CredentialClass GitHubCredentialClass
	// InstallationID scopes a byo_app entry to the installation it was read
	// through; empty on a pat entry, which has no installation row to hang off.
	InstallationID string
	// Host scopes a pat entry to the GitHub deployment it was read against;
	// empty on a byo_app entry, which inherits the host from its installation.
	Host string
	// Source is the provider that issued the repository (RepoSourceGitHub
	// today); empty on a struct built without one, which the stores normalize on
	// write. It exists so the join to the registry is written against the same
	// key the registry is keyed under, not against a literal.
	Source string
	Owner  string
	Repo   string
	// ExternalID is GitHub's own repository id, which both enumerations return
	// for every entry. "" when the source sent none, which is a supported state:
	// the slug matches anyway.
	ExternalID string
	// The display fields the repository picker renders, mirrored from the same
	// repository objects the enumeration already returns.
	Description string
	Language    string
	HTMLURL     string
	// PushedAt is GitHub's own timestamp string, passed through verbatim. It is
	// never parsed or compared here.
	PushedAt string
	Private  bool
	// ObservedAt is when the refresh that wrote this row ran — display
	// provenance for "as of…" and the staleness gate the TTL and the write gate
	// both read.
	ObservedAt time.Time
}

// Slug renders the entry as the "owner/repo" string every slug-keyed column and
// store method uses.
func (r ReachableRepository) Slug() string { return r.Owner + "/" + r.Repo }

// ReachableCacheState is what a consumer needs to know about an org's cache
// without reading a single row of it: whether it has ever been refreshed, how
// much is in it, and how old the stalest part is.
//
// Refreshed and Count are separate on purpose, and conflating them is the bug
// this type exists to prevent. A scope that genuinely reaches NOTHING — an App
// installed on an account and then narrowed to no repositories — leaves zero
// rows behind, exactly like a refresh that never ran. Read off the row count,
// that org's picker would say "discovering repositories…" forever and kick a
// refresh on every open, which is the unbounded enumeration the TTL exists to
// prevent. Refreshed answers "have we looked"; Count answers "what did we find".
//
// ObservedAt is the OLDEST instant across the class's refreshed scopes, not the
// newest. A cache is only as fresh as its stalest scope: an org with two App
// installations, one refreshed a minute ago and one a week ago, is a week stale
// for the repositories on the second account, and taking the newest would report
// it fresh and never refresh the half that needs it.
type ReachableCacheState struct {
	Refreshed  bool
	Count      int
	ObservedAt time.Time
}

// Known reports whether any scope of this org and class has been refreshed. It
// is what the picker's readiness discriminator answers off — false is "we have
// not looked yet", never "you have no repositories".
func (s ReachableCacheState) Known() bool { return s.Refreshed }

// StaleAt reports whether the cache is older than ttl as of now. A cache nobody
// has refreshed is stale by definition — there is nothing to serve and nothing
// to date — so a caller that wants to distinguish the two checks Known first.
func (s ReachableCacheState) StaleAt(now time.Time, ttl time.Duration) bool {
	if !s.Refreshed || s.ObservedAt.IsZero() {
		return true
	}
	return now.Sub(s.ObservedAt) >= ttl
}

// GitHubPendingBind is one initiated bind ceremony: an admin clicked Connect,
// TF minted a nonce, and the browser left for GitHub carrying it in a cookie.
// The row is what the callback consumes to learn WHICH workspace the returning
// installation belongs to — the redirect from GitHub cannot say, because its
// installation_id is an unsigned, guessable query parameter GitHub's own
// documentation says not to trust.
//
// It is durable rather than in-process because the pod that serves the Connect
// click is not necessarily the pod that serves the callback. An in-memory map
// would work in development and fail intermittently under load, which is the
// worst failure shape a security control can have.
//
// NonceHash is the SHA-256 of the nonce in lowercase hex — never the nonce
// itself, so a database read yields nothing that can complete a bind. The
// browser's cookie holds the only copy of the plaintext.
type GitHubPendingBind struct {
	NonceHash string
	OrgID     string
	// UserID is the admin who started the ceremony. The callback requires the
	// returning session to be that same person, so a cookie replayed into
	// somebody else's browser consumes nothing.
	UserID    string
	CreatedAt time.Time
	ExpiresAt time.Time
	// ConsumedAt is when the callback spent this record, zero while it is
	// unspent. Consumption is a conditional update rather than a read then a
	// write, so two concurrent callbacks cannot both find it unspent.
	ConsumedAt time.Time
}
