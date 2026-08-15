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

// InstallationRepository is one entry in an installation's repository grant —
// a repository the App can reach through that installation, whether or not any
// team tracks it.
//
// It is deliberately NOT a RepoProfile: the grant is a cache of an external
// fact, rebuilt in full on every reconcile, while a repository row is a registry
// of a TF entity that worktrees, entities and clone state hang off. A grant
// entry with no registry row behind it is the normal case — it is exactly what
// "reach without purpose" means — so this type carries everything a reader
// needs without a join.
type InstallationRepository struct {
	OrgID          string
	InstallationID string
	// Source is the provider that issued the repository (RepoSourceGitHub
	// today); empty on a struct built without one, which the stores normalize on
	// write. It exists so the join to the registry is written against the same
	// key the registry is keyed under, not against a literal.
	Source string
	Owner  string
	Repo   string
	// ExternalID is GitHub's own repository id, which
	// GET /installation/repositories returns for every entry. "" when the source
	// sent none, which is a supported state: the slug matches anyway.
	ExternalID string
	// ObservedAt is when the reconcile that wrote this row ran. Display
	// provenance for "the grant as of…", never a join key.
	ObservedAt time.Time
}

// Slug renders the entry as the "owner/repo" string every slug-keyed column and
// store method uses.
func (r InstallationRepository) Slug() string { return r.Owner + "/" + r.Repo }
