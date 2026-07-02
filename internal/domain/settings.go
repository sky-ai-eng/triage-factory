package domain

import (
	"fmt"
	"slices"
	"strings"
	"time"
)

// DefaultModel is the model tier TF falls back to when a team has no
// explicit default. Shared by DefaultTeamSettings (the stored default) and
// EffectiveModel (the resolution fallback) so the two can't drift.
const DefaultModel = "sonnet"

// DefaultBranchTemplate is the branch-name convention suggested to delegated
// agents as envelope guidance (not enforced). The literal "<ticket-id>" is
// substituted with the run's ticket id at prompt-render time.
const DefaultBranchTemplate = "tfac/<ticket-id>"

// OrgSettings is the org-scope settings row (org_settings table).
//
// Field nullability:
//   - GitHubPollInterval / JiraPollInterval / GitHubCloneProtocol ship
//     NOT NULL with defaults; a freshly-inserted row always carries
//     them populated.
//   - GitHubBaseURL / JiraBaseURL / AnthropicAPIKeyRef /
//     BedrockCredentialsRef / MaxLLMModelTier are nullable columns.
//     Empty string round-trips "" ↔ NULL: "not configured yet" (base
//     URLs), "use deployment default" (vault refs), or "no cap" (max
//     tier). Callers never need to distinguish "" from NULL.
//   - MaxDailyCostUSD is a nullable numeric column (TFAC-477). 0
//     round-trips 0 ↔ NULL — "no cap". Callers never need to
//     distinguish 0 from NULL.
//
// GitHubCloneProtocol is "ssh" or "https" only — enforced by a CHECK
// on both backends. An empty string from a caller is treated as
// "leave the default in place" by UpdateSettings (substitutes "ssh"),
// never written to the column.
type OrgSettings struct {
	GitHubBaseURL       string
	GitHubPollInterval  time.Duration
	GitHubCloneProtocol string

	JiraBaseURL      string
	JiraPollInterval time.Duration

	AnthropicAPIKeyRef    string
	BedrockCredentialsRef string
	MaxLLMModelTier       string // app-validated, NOT DB-constrained; known values "haiku" | "sonnet" | "opus" | "" (no cap) — not an exhaustive set

	// MaxDailyCostUSD is the org-wide daily LLM spend cap (TFAC-477). 0 = no
	// cap (round-trips 0 ↔ NULL). When today's org spend (UTC calendar day,
	// summed across every category) is >= this value, the delegation choke
	// point refuses all new agent runs. A runaway-spend fuse.
	MaxDailyCostUSD float64

	// MarketplaceEnabled is the ship-dark org toggle for the within-org
	// prompt marketplace (TFAC-535 / TFAC-92 scoping decision 4). NOT NULL
	// DEFAULT false on both backends — no NULL-round-trip subtlety like the
	// fields above. UI/enforcement of this flag land in TFAC-539; this
	// ticket only carries the column.
	MarketplaceEnabled bool
}

// DefaultOrgSettings returns the NOT NULL DEFAULT values from the
// org_settings schema as a Go struct. Used by:
//
//   - OrgsStore.GetSettings / GetSettingsSystem as the fallback when
//     no row exists yet (test fixtures that bypass provisioning, or
//     reads on a fresh DB before the first auth flow has run).
//   - Provisioning paths (server/auth_provision.go, baseline migration
//     seed rows) that want to materialize the schema defaults
//     explicitly in Go.
//
// Nullable fields (base URLs, vault refs, max tier) stay empty —
// "not configured yet" semantics are preserved. Keep this in sync
// with the schema DEFAULT clauses in baseline migration.
func DefaultOrgSettings() OrgSettings {
	return OrgSettings{
		GitHubPollInterval:  5 * time.Minute,
		GitHubCloneProtocol: "ssh",
		JiraPollInterval:    5 * time.Minute,
	}
}

// EffectiveCloneProtocol resolves the clone protocol actually in force, given
// the stored org setting and whether the deployment is multi-mode.
//
// Multi-mode is ALWAYS "https", independent of the stored value: a GitHub App
// installation token is an HTTPS bearer credential that cannot be used over
// SSH at all, and the hosted runtime container has no ssh-agent / key /
// known_hosts. The stored value may still read "ssh" — DefaultOrgSettings
// returns "ssh" (correct for local), and a legacy row could carry it —
// so it must not be honored in multi.
//
// Local mode honors the stored value, treating only the literal "ssh" as SSH
// and defaulting empty / "https" / any stale value to "https" — the same
// semantics backend clone-URL selection and the API view already used.
func EffectiveCloneProtocol(stored string, multiMode bool) string {
	if multiMode {
		return "https"
	}
	if stored == "ssh" {
		return "ssh"
	}
	return "https"
}

// TeamSettings is the team-scope settings row (team_settings table).
// JiraProjects holds the team's tracked Jira project keys — the full
// per-project rule rows live in jira_project_status_rules and are
// owned by JiraStatusRulesStore, not this struct. JiraProjects on
// this row is a denormalized fast path for "which projects to poll"
// without joining; the rules table is the source of truth for the
// per-project status semantics.
//
// DefaultModel + AutoDelegateEnabled moved off user_settings in SKY-354:
// the team owns the AI behavior policy, users do not override in v1.
type TeamSettings struct {
	JiraProjects               []string
	AIReprioritizeThreshold    int
	AIPreferenceUpdateInterval int
	DefaultModel               string // "haiku" | "sonnet" | "opus"
	AutoDelegateEnabled        bool

	// PermissionAbsentGraceMS + PermissionAbsentAutodenyEnabled gate the
	// presence-aware fast auto-deny for unattended permission prompts (TFAC-392).
	// When the toggle is on and a delegated run raises an off-allowlist tool
	// prompt with no answer-capable, focused tab present in the run's org, the
	// backend denies after this grace window (ms) instead of waiting the full
	// permTimeout(). When off, the prompt keeps the full-timeout behavior exactly.
	// The grace is clamped at spawn to [1s, permTimeout()) so it can never invert
	// the "total wait < idleTimeout()" invariant.
	PermissionAbsentGraceMS         int
	PermissionAbsentAutodenyEnabled bool

	// MaxDailyCostUSD is the per-team daily LLM spend cap (TFAC-482), the
	// team-scoped sibling of OrgSettings.MaxDailyCostUSD. 0 = no cap (round-trips
	// 0 ↔ NULL). When today's team spend (UTC calendar day, summed over the
	// team's own rows — system overhead + non-team curator carry a NULL team_id
	// and never count) is >= this value AND the governance entitlement is active,
	// the delegation choke point refuses new agent runs for that team. Org-admin-
	// configured: a team admin cannot set their own team's cap (the team-settings
	// write path never touches this field — only the org-admin cap endpoint does),
	// so a team-admin save round-trips the stored value untouched.
	MaxDailyCostUSD float64

	// BranchTemplate is the team's branch-name convention (TFAC-498), rendered
	// into the delegated agent's prompt as envelope guidance — it is NOT
	// enforced. The literal "<ticket-id>" is substituted with the run's ticket
	// id at prompt-render time. NOT NULL with a schema DEFAULT; defaults to
	// DefaultBranchTemplate. The write path coalesces an empty string to the
	// default so a blank never persists.
	BranchTemplate string
}

// DefaultTeamSettings returns the NOT NULL DEFAULT values from the
// team_settings schema as a Go struct. Same pattern as
// DefaultOrgSettings — read-side fallback for missing rows, plus an
// explicit Go-side baseline for provisioning paths.
//
// AutoDelegateEnabled defaults false here (matching the schema
// DEFAULT and the multi-mode "new teams require explicit opt-in"
// rule). The local-mode sentinel team flips this to true via its
// baseline seed row so the local-first happy path keeps auto-
// delegation on out of the box.
func DefaultTeamSettings() TeamSettings {
	return TeamSettings{
		AIReprioritizeThreshold:         5,
		AIPreferenceUpdateInterval:      20,
		DefaultModel:                    DefaultModel,
		AutoDelegateEnabled:             false,
		PermissionAbsentGraceMS:         15000,
		PermissionAbsentAutodenyEnabled: true,
		BranchTemplate:                  DefaultBranchTemplate,
	}
}

// UserSettings is the user-scope settings row (user_settings table).
// Reserved for future per-user prefs (theme, notification destinations,
// swipe sensitivity, onboarding state). Empty for v1 post-SKY-354
// cleanup — the AI model + auto-delegate toggle that used to live here
// moved to TeamSettings. The struct stays so the store API can grow
// fields without a signature change.
type UserSettings struct{}

// JiraProjectStatusRules is one row of jira_project_status_rules —
// the team's status configuration for a single Jira project. Multiple
// rows per team (keyed `(team_id, project_key)`) so two projects on
// the same team can have different workflows. The CHECK constraints
// on the table guarantee any persisted row carries a non-empty pickup
// set + members + canonical for both write-target rules.
type JiraProjectStatusRules struct {
	// TeamID is the owning team (the PK's first column). The List* store
	// methods populate it; ReplaceForTeam ignores it (the team is a
	// parameter). Needed by the poller's per-project member merge (to
	// break canonical ties by lowest team_id) and the router's team↔project
	// gate (to look up the handler's team).
	TeamID              string
	ProjectKey          string
	PickupMembers       []string
	InProgressMembers   []string
	InProgressCanonical string
	DoneMembers         []string
	DoneCanonical       string
}

// TeamGitHubGroup is one row of team_github_groups — a fully-qualified
// GitHub team (org login + team slug) mapped to a TF team for routing
// human GitHub-team review requests to the right board. Dumb string
// labels only: no membership resolution, no nested-team traversal, no
// sync of GitHub's team graph. Fully-qualified with the org login so
// @acme/frontend and @beta/frontend don't collide. Many of these can
// sit under one TF team (the "funnel" direction).
type TeamGitHubGroup struct {
	OrgLogin string
	TeamSlug string
}

// NormalizeTeamGitHubGroups lowercase-trims every group's org login +
// team slug, drops entries with an empty field, and de-duplicates —
// the canonical form persisted by SetForTeam and matched by routing
// lookups. GitHub team slugs are already lowercase and org logins are
// case-insensitive, so normalizing on the way in keeps routing matches
// reliable regardless of how the admin typed them. Returns an error
// only if an entry has one field populated and the other empty (a
// half-specified group, which is a caller bug rather than a value to
// silently drop).
func NormalizeTeamGitHubGroups(groups []TeamGitHubGroup) ([]TeamGitHubGroup, error) {
	out := make([]TeamGitHubGroup, 0, len(groups))
	seen := map[TeamGitHubGroup]bool{}
	for i, g := range groups {
		login := strings.ToLower(strings.TrimSpace(g.OrgLogin))
		slug := strings.ToLower(strings.TrimSpace(g.TeamSlug))
		if login == "" && slug == "" {
			continue
		}
		if login == "" || slug == "" {
			return nil, fmt.Errorf("groups[%d]: github group needs both org login and team slug, got %q/%q", i, g.OrgLogin, g.TeamSlug)
		}
		n := TeamGitHubGroup{OrgLogin: login, TeamSlug: slug}
		if seen[n] {
			continue
		}
		seen[n] = true
		out = append(out, n)
	}
	return out, nil
}

// TeamGitHubRepo is one row of team_github_repos — a single GitHub repo
// (owner + name) a TF team has declared it tracks. The GitHub
// tracking-scope twin of JiraProjectStatusRules: the per-team selection
// that the router's team↔repo gate consults and that repo_profiles is
// the org-wide UNION of. Distinct from TeamGitHubGroup, which maps
// CODEOWNERS review-routing teams — this is tracking scope. The Owner is
// stored as-typed for display fidelity (GitHub logins are
// case-insensitive); matching against event metadata is done
// case-insensitively at the gate.
type TeamGitHubRepo struct {
	Owner string
	Repo  string
}

// Slug returns the canonical "owner/repo" form used as the repo_profiles
// id and the shape every repo-list caller passes around.
func (r TeamGitHubRepo) Slug() string { return r.Owner + "/" + r.Repo }

// TrackedRepoTeams is one tracked (owner, repo) in an org together with the
// display names of every team that tracks it. It backs the GitHub-access
// switch reachability preflights (TFAC-328): when a switch would leave a
// tracked repo unreachable by the new credential, the diff names which teams
// own that now-dark repo so the admin knows who's affected. The team list is
// deterministic (ordered by team name); a repo with no teams never appears
// (it wouldn't be tracked).
type TrackedRepoTeams struct {
	Owner string
	Repo  string
	Teams []string
}

// Slug returns the canonical "owner/repo" form, matching TeamGitHubRepo.Slug.
func (r TrackedRepoTeams) Slug() string { return r.Owner + "/" + r.Repo }

// NormalizeTeamGitHubRepos trims every repo's owner + name, drops entries
// with an empty field, and de-duplicates on (owner, repo) — the
// canonical form persisted by ReplaceForTeam. Unlike the github-team
// normalizer this keeps the original case (a repo slug round-trips into
// repo_profiles.id and the GitHub clone URL verbatim), so dedup is
// case-sensitive on the full slug. Returns an error only for a
// half-specified entry (one field populated, the other empty) — a caller
// bug rather than a value to silently drop. Splitting "owner/repo" slugs
// is the caller's job (see TeamGitHubReposFromSlugs); this works on the
// already-split struct.
func NormalizeTeamGitHubRepos(repos []TeamGitHubRepo) ([]TeamGitHubRepo, error) {
	out := make([]TeamGitHubRepo, 0, len(repos))
	// Key on the case-folded "owner/repo" — GitHub owners and repo names
	// are case-insensitive, so Acme/API and acme/api are the same repo.
	// Storing both would double the row in team_github_repos and, via the
	// reconcile, in repo_profiles (a double-polled repo). First-seen
	// casing is kept for display.
	seen := map[string]bool{}
	for i, r := range repos {
		owner := strings.TrimSpace(r.Owner)
		repo := strings.TrimSpace(r.Repo)
		if owner == "" && repo == "" {
			continue
		}
		if owner == "" || repo == "" {
			return nil, fmt.Errorf("repos[%d]: github repo needs both owner and name, got %q/%q", i, r.Owner, r.Repo)
		}
		// Reject extra path segments. A GitHub owner or repo name never
		// contains a slash, so "owner/repo/extra" (which TeamGitHubReposFromSlugs
		// would split into owner + "repo/extra") is an impossible repo
		// that would be polled forever. Same exact-shape contract the
		// pinned-repo validator enforces.
		if strings.ContainsRune(owner, '/') || strings.ContainsRune(repo, '/') {
			return nil, fmt.Errorf("repos[%d]: github repo must be exactly owner/repo, got %q/%q", i, owner, repo)
		}
		key := strings.ToLower(owner) + "/" + strings.ToLower(repo)
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, TeamGitHubRepo{Owner: owner, Repo: repo})
	}
	return out, nil
}

// TeamGitHubReposFromSlugs splits "owner/repo" slugs into TeamGitHubRepo
// structs and normalizes the result. Malformed slugs (no slash → empty
// half; extra segments → a slash inside the name) surface as an error
// from NormalizeTeamGitHubRepos so the HTTP layer can 400 rather than
// silently persist an impossible repo. The split is on the first slash;
// any remaining slash is caught by NormalizeTeamGitHubRepos's exact
// owner/repo shape check.
func TeamGitHubReposFromSlugs(slugs []string) ([]TeamGitHubRepo, error) {
	repos := make([]TeamGitHubRepo, 0, len(slugs))
	for _, s := range slugs {
		owner, repo, _ := strings.Cut(strings.TrimSpace(s), "/")
		repos = append(repos, TeamGitHubRepo{Owner: owner, Repo: repo})
	}
	return NormalizeTeamGitHubRepos(repos)
}

// NormalizeGitHubTeamSlugs lowercase-trims a list of GitHub team slugs
// and drops blanks — the form PruneMissingSystem compares the stored
// rows against.
func NormalizeGitHubTeamSlugs(slugs []string) []string {
	out := make([]string, 0, len(slugs))
	for _, s := range slugs {
		s = strings.ToLower(strings.TrimSpace(s))
		if s != "" {
			out = append(out, s)
		}
	}
	return out
}

// PickupContains reports whether status is a member of the Pickup rule.
func (r JiraProjectStatusRules) PickupContains(status string) bool {
	return slices.Contains(r.PickupMembers, status)
}

// InProgressContains reports whether status is a member of the InProgress rule.
func (r JiraProjectStatusRules) InProgressContains(status string) bool {
	return slices.Contains(r.InProgressMembers, status)
}

// DoneContains reports whether status is a member of the Done rule.
func (r JiraProjectStatusRules) DoneContains(status string) bool {
	return slices.Contains(r.DoneMembers, status)
}

// RuleForProject returns the per-project rule for the given key, or
// nil when no rule with that key is in the slice. Callers degrade
// gracefully on nil ("no rules configured" — no terminal check, no
// transitions).
func RuleForProject(rules []JiraProjectStatusRules, key string) *JiraProjectStatusRules {
	for i := range rules {
		if rules[i].ProjectKey == key {
			return &rules[i]
		}
	}
	return nil
}

// JiraProjectKeys returns the ordered list of project keys with empty
// entries filtered out. Mirrors the helper the deleted config package
// exposed for poller dispatch and JQL queries.
func JiraProjectKeys(rules []JiraProjectStatusRules) []string {
	keys := make([]string, 0, len(rules))
	for _, p := range rules {
		if p.ProjectKey != "" {
			keys = append(keys, p.ProjectKey)
		}
	}
	return keys
}

// JiraAllPickupMembers returns the union of every project's pickup
// members, in first-seen order, each member deduped. Used by JQL
// queries that span every project a team tracks.
func JiraAllPickupMembers(rules []JiraProjectStatusRules) []string {
	return jiraUnionMembers(rules, func(p JiraProjectStatusRules) []string { return p.PickupMembers })
}

// JiraAllDoneMembers returns the union of every project's done members.
// Used by JQL queries that exclude terminal tickets across the team's
// full project list.
func JiraAllDoneMembers(rules []JiraProjectStatusRules) []string {
	return jiraUnionMembers(rules, func(p JiraProjectStatusRules) []string { return p.DoneMembers })
}

func jiraUnionMembers(rules []JiraProjectStatusRules, pick func(JiraProjectStatusRules) []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0)
	for _, p := range rules {
		for _, m := range pick(p) {
			if !seen[m] {
				seen[m] = true
				out = append(out, m)
			}
		}
	}
	return out
}
