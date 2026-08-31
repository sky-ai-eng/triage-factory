package domain

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"
)

// The model TF provisions a team with when nobody has chosen one yet, one
// constant per vocabulary. Read by DefaultTeamSettingsFor, and each pinned to
// its dialect's team_settings.default_model column DEFAULT, which is what a
// partial write materializes a row from, so the two can't drift.
//
// Two constants rather than one because the two dialects store different
// vocabularies: Postgres carries the native wire id its runtime sends, SQLite
// the harness alias its subprocess resolves. A single value would be refused by
// one deployment's own save validator.
const (
	DefaultModel      = ModelSonnet
	LocalDefaultModel = ModelAliasSonnet
)

// DefaultModelFor returns the provisioning default in the vocabulary the given
// deployment stores. The mode is a parameter for the same reason it is one on
// EffectiveLLMAuthMethod: the answer is a property of the deployment being
// asked about, not of the process asking.
//
// It is what PROVISIONS a team, never what a resolution falls back to: a team
// whose default is unset has chosen nothing, and dispatching a model on its
// behalf would spend on a choice nobody made.
func DefaultModelFor(multiMode bool) string {
	if multiMode {
		return DefaultModel
	}
	return LocalDefaultModel
}

// LocalBackgroundJobsModel is the background-jobs model a local install starts
// with. Local mode is a single-user, zero-configuration first run, so its knob
// arrives already filled in — the SQLite org_settings column carries this value
// as its DEFAULT, which is what materializes it both for a fresh tenant and for
// a row that predates the column. Multi mode has no such default: an org there
// picks its model during setup, and until it does its background jobs skip.
//
// It is a pre-fill, not a fallback. Nothing resolves to it at job time; the only
// thing that reads it is the schema.
const LocalBackgroundJobsModel = ModelAliasHaiku

// Where an org's Claude credentials come from — the value of
// org_settings.llm_auth_method, and the answer to what it means that an org has
// bound no provider.
//
// The two are not symmetric in how they are established. LLMAuthBYOK is implied
// by the act of binding, because an org cannot hold its own credential and still
// be running on the host's. LLMAuthSystem is only ever an explicit selection (or
// the local column default), and it holds only while no provider ref is bound:
// credential resolution reaches for a stored key whenever one exists, so the two
// together would describe a run that does not happen.
const (
	// LLMAuthSystem — the host's credentials. TF supplies the agent subprocess
	// nothing, and the Claude Code SDK resolves authentication from the
	// inherited environment: a Claude Code subscription login, an exported
	// ANTHROPIC_API_KEY, Bedrock or Vertex variables. Which of those it is, TF
	// cannot see, which is why models under it are reported as assumed rather
	// than as anything a probe could establish.
	//
	// Local mode only. See EffectiveLLMAuthMethod.
	LLMAuthSystem = "system"
	// LLMAuthBYOK — the org's own bound material, whichever provider serves the
	// model. Multi mode is always this.
	LLMAuthBYOK = "byok"
)

// EffectiveLLMAuthMethod resolves the credential source actually in force,
// given the stored org setting and whether the deployment is multi-mode.
//
// Multi-mode is ALWAYS LLMAuthBYOK, independent of the stored value: there are
// no host credentials for a multi-mode deployment to lend, because the operator's
// environment is one environment shared by every tenant. Storing the other
// value there is refused on the way in, so the disagreement should not arise;
// resolving it here as well is what makes a row that arrived some other way
// inert instead of a cross-tenant credential leak.
//
// Local mode treats only the literal LLMAuthBYOK as BYOK and resolves empty or
// any unrecognised value to LLMAuthSystem — the same shape EffectiveCloneProtocol
// uses, and for the same reason: the column is app-validated rather than
// CHECK-constrained, so the read is where a value nothing wrote is decided. It
// resolves toward the host because that is what a local run with nothing bound
// does regardless.
func EffectiveLLMAuthMethod(stored string, multiMode bool) string {
	if multiMode || stored == LLMAuthBYOK {
		return LLMAuthBYOK
	}
	return LLMAuthSystem
}

// DefaultBranchTemplate is the branch-name convention suggested to delegated
// agents as envelope guidance (not enforced). The literal "<ticket-id>" is
// substituted with the conversation's ticket id at prompt-render time.
const DefaultBranchTemplate = "tfac/<ticket-id>"

// Review-posting postures — how a delegated agent's finalized review reaches
// GitHub. The gate is right for some deployments and wrong for others, and what
// decides which is *who the review posts as*: an App-backed review lands as a
// bot (a wrong comment is a bot being wrong), a PAT-backed one lands as the
// borrowed user (a wrong comment is that person being wrong in front of their
// colleagues). Same mechanism, materially different cost of error — hence a
// per-team setting whose default derives from credential identity.
const (
	// ReviewPostureIdentity resolves at finalize time from the acting
	// credential: an App installation posts directly, a borrowed PAT stages for
	// human approval. An indeterminate identity stages too — that is the
	// documented contract of github.IdentityUnknown, not a fallback.
	ReviewPostureIdentity = "identity"
	// ReviewPostureDraft always stages for human approval.
	ReviewPostureDraft = "draft"
	// ReviewPostureAuto always submits on finalize.
	ReviewPostureAuto = "auto"
	// ReviewPostureAutoUnlessBlocking submits unless the review is
	// consequential: a REQUEST_CHANGES verdict, or any staged inline comment at
	// severity BLOCKER, stages for a human instead.
	ReviewPostureAutoUnlessBlocking = "auto_unless_blocking"
)

// DefaultReviewPosture is the posture a team gets with no explicit choice —
// derive from the credential identity. NOT NULL with a schema DEFAULT of the
// same literal; the write path coalesces a blank to it so an empty string never
// persists.
const DefaultReviewPosture = ReviewPostureIdentity

// ValidReviewPostures is the accepted value set for TeamSettings.ReviewPosture.
// Validated app-side rather than with a DB CHECK (the GitHubCredentialClass
// precedent): a CHECK buys little over a closed handler and would force
// SQLite's whole table-rebuild dance to ever change.
var ValidReviewPostures = []string{
	ReviewPostureIdentity,
	ReviewPostureDraft,
	ReviewPostureAuto,
	ReviewPostureAutoUnlessBlocking,
}

// ValidReviewPosture reports whether s is one of ValidReviewPostures.
func ValidReviewPosture(s string) bool { return slices.Contains(ValidReviewPostures, s) }

// Base-branch push policies — whether a delegated agent may push to a repo's
// base / default branch (main, master, the repository row's default, the configured
// base). The default refuses, which is right for a team that reviews through
// pull requests and wrong for trunk-based teams, docs repos, config repos and
// generated-file bots — hence a setting rather than a hard-coded rule.
//
// This is a safety guard against a MISTAKEN agent, not a control against a
// hostile one. The managed Git path enforces it at the per-run git proxy's ref
// gate in both modes. A local process can deliberately discard its run-scoped
// routing and use the operator's machine directly; nothing here makes local
// mode a security boundary.
const (
	// BaseBranchPushNever refuses every push to a protected ref.
	BaseBranchPushNever = "never"
	// BaseBranchPushManualOnly permits it only when a human dispatched the
	// conversation. An event-triggered conversation — one minted from a PR
	// body, an issue comment or a label, all externally authored — is refused.
	BaseBranchPushManualOnly = "manual_only"
	// BaseBranchPushAlways permits it on every conversation.
	BaseBranchPushAlways = "always"
)

// DefaultBaseBranchPushPolicy is what a team gets with no explicit choice:
// refuse. NOT NULL with a schema DEFAULT of the same literal; the write path
// coalesces a blank to it so an empty string never persists.
const DefaultBaseBranchPushPolicy = BaseBranchPushNever

// ValidBaseBranchPushPolicies is the accepted value set for
// TeamSettings.BaseBranchPushPolicy. Validated app-side rather than with a DB
// CHECK, for the same reason as ValidReviewPostures.
var ValidBaseBranchPushPolicies = []string{
	BaseBranchPushNever,
	BaseBranchPushManualOnly,
	BaseBranchPushAlways,
}

// ValidBaseBranchPushPolicy reports whether s is one of
// ValidBaseBranchPushPolicies.
func ValidBaseBranchPushPolicy(s string) bool {
	return slices.Contains(ValidBaseBranchPushPolicies, s)
}

// MaxConcurrentClaimsCeiling is the largest value OrgSettings.MaxConcurrentRuns
// accepts. It's a sanity bound far beyond any real fleet (per-executor
// concurrency tops out in the low hundreds; even a large fleet stays in the
// tens of thousands), chosen so a validated value always fits the Postgres
// int4 column — an oversized input is rejected with a clean 400 at the handler
// rather than an "integer out of range" 500 at the DB. The frontend mirrors
// this bound so Save blocks before the round-trip.
const MaxConcurrentClaimsCeiling = 1_000_000

// The band OrgSettings.APITokenMaxAgeDays accepts, mirrored by the Postgres
// column's own CHECK and by the frontend input so Save blocks before the
// round-trip. A day is the smallest cap worth expressing (anything shorter is
// a per-token expires_at, which the minter already controls) and a year the
// longest that still reads as a policy rather than as no policy at all — an
// org that wants neither clears the setting.
const (
	APITokenMaxAgeDaysMin = 1
	APITokenMaxAgeDaysMax = 365
)

// OrgSettings is the org-scope settings row — composed from org_settings and,
// for the four fields below, org_event_sources.
//
// GitHubBaseURL / GitHubPollInterval / JiraBaseURL / JiraPollInterval are
// stored on org_event_sources (org_id, kind) as base_url / poll_interval,
// not on org_settings — they are the uniform per-source settings
// consolidated off the org_settings singleton, keyed by kind ("github" /
// "jira") instead of by a column-name prefix. GetSettings composes them into
// this struct so every existing reader keeps working unchanged, and
// UpdateSettings / UpdateSettingsVersioned keep accepting and writing them —
// they are ordinary read-write fields on this struct, exactly as before;
// only their storage moved. NULL on org_event_sources means "no override
// recorded", and GetSettings resolves that to "" for a base URL (not
// configured yet) or DefaultOrgSettings()'s 5-minute cadence for a poll
// interval — the same fallback org_settings' NOT NULL DEFAULT columns gave
// for free, made explicit at the read.
//
// Field nullability:
//   - GitHubPollInterval / JiraPollInterval / GitHubCloneProtocol always
//     come back populated — the first two via the org_event_sources
//     NULL-to-default resolution above, GitHubCloneProtocol as a genuine
//     NOT NULL org_settings column.
//   - GitHubBaseURL / JiraBaseURL / AnthropicAPIKeyRef /
//     BedrockCredentialsRef round-trip "" for "not configured yet" (base
//     URLs) / "use deployment default" (vault refs). Callers never need a
//     second sentinel for absence.
//   - EnabledModels is nil for a NULL column — the org has expressed no
//     preference, which OrgModelSet resolves to every model this deployment
//     offers. A stored set is never empty; the write refuses one.
//   - MaxDailyCostUSD is a nullable numeric column (TFAC-477). 0
//     round-trips 0 ↔ NULL — "no cap". Callers never need to
//     distinguish 0 from NULL.
//   - MaxConcurrentRuns is a nullable integer column. 0 round-trips
//     0 ↔ NULL — "unlimited". Same convention as MaxDailyCostUSD;
//     the claim treats <= 0 as unlimited too.
//
// GitHubCloneProtocol is "ssh" or "https" only — enforced by a CHECK
// on both backends. An empty string from a caller is treated as
// "leave the default in place" by UpdateSettings (substitutes "https"),
// never written to the column.
type OrgSettings struct {
	GitHubBaseURL       string
	GitHubPollInterval  time.Duration
	GitHubCloneProtocol string

	JiraBaseURL      string
	JiraPollInterval time.Duration

	AnthropicAPIKeyRef    string
	BedrockCredentialsRef string
	// EnabledModels is the org's enable-set: the model keys its teams may pick
	// from, stored as a JSON array. nil is the absent value and means the org
	// has expressed no preference, which resolves to every model this
	// deployment offers — so a model a later release adds is enabled for that
	// org the day it ships, while a stored set stays frozen at what it names.
	//
	// The keys are this deployment's own execution vocabulary, the same one
	// BackgroundJobsModel and TeamSettings.DefaultModel are written in. Nothing
	// translates a stored value, so a set never mixes the two.
	//
	// Never store the resolved set. "The org chose nothing" and "the org chose
	// everything" are different facts about what happens next, and collapsing
	// them at the column is what would make the first one impossible to spell.
	//
	// Read through OrgModelSet, which is also where the absent-value decision
	// lives, so every surface answers the same.
	EnabledModels []string

	// BackgroundJobsModel is the model the two headless system jobs — the
	// scorer and the repo profiler — run on. A key from this deployment's model
	// universe (internal/modelcatalog), validated on write against that
	// universe and against the providers the org has connected.
	//
	// One knob for all three, org-level: they are the same kind of work (short,
	// toolless, no transcript) bought from the same budget, and a per-job knob
	// would be three ways to answer one question.
	//
	// "" is unset, and unset means the jobs do not run: there is no shipped
	// fallback model anywhere in TF, so a job with no model skips its cycle and
	// says so rather than spending someone's money on a model nobody chose.
	// Local installs are pre-filled by the column default (see
	// LocalBackgroundJobsModel); a multi-mode org picks one during setup.
	BackgroundJobsModel string

	// LLMAuthMethod is where this org's Claude credentials come from:
	// LLMAuthSystem (the host's, resolved by the SDK from the inherited
	// environment) or LLMAuthBYOK (the org's own bound material). Read through
	// EffectiveLLMAuthMethod, which is what makes multi mode's single legal
	// value hold whatever the column says.
	//
	// It is a separate fact from AnthropicAPIKeyRef / BedrockCredentialsRef,
	// not a summary of them: those name WHICH providers are bound, and this
	// names what it means that none are. An org on LLMAuthBYOK with neither ref
	// set has said it brings its own key and has not bound one — a setup gap,
	// where the same emptiness under LLMAuthSystem is a working configuration.
	LLMAuthMethod string

	// MaxDailyCostUSD is the org-wide daily LLM spend cap (TFAC-477). 0 = no
	// cap (round-trips 0 ↔ NULL). When today's org spend (UTC calendar day,
	// summed across every category) is >= this value, the delegation choke
	// point refuses all new agent conversations. A runaway-spend fuse.
	MaxDailyCostUSD float64

	// MaxConcurrentRuns is the org-wide ceiling on how many runs the org may
	// have executing at once across the executor fleet. 0 = unlimited
	// (round-trips 0 ↔ NULL; the claim also treats <= 0 as unlimited). Read
	// live in the Postgres claim — a queued run is invisible to claims while
	// the org's active-run count is at or above this value — so an event storm
	// can't monopolize the fleet's slots. The instantaneous-concurrency sibling
	// of the daily *spend* cap. Enforced multi-mode only (SQLite/local is N=1).
	//
	// The store read clamps a stray negative (no DB CHECK guards the column) up
	// to 0, so callers always see "unlimited" as 0 and never a negative. Writes
	// are validated in [0, MaxConcurrentClaimsCeiling] at the handler.
	MaxConcurrentRuns int

	// APITokenMaxAgeDays is the ceiling on how long any of the org's API
	// tokens may live, in days, within [APITokenMaxAgeDaysMin,
	// APITokenMaxAgeDaysMax]. 0 = uncapped (round-trips 0 ↔ NULL) — the same
	// convention as the two caps above, and the reason the band starts at 1
	// rather than 0.
	//
	// Applied at USE, never stamped into a token at mint: a token's effective
	// expiry is min(its stored expires_at, its created_at + this cap) computed
	// against the CURRENT value. So lowering it immediately shortens every
	// existing token in the org — a token already older than the new cap stops
	// authenticating on its next request — and raising or clearing it extends
	// only tokens whose minter did not pin an earlier expires_at of their own.
	// There is no grandfathering and no sweep job.
	//
	// Multi-mode policy: the tokens it governs are a Postgres-only credential,
	// like sessions. The column is dual-dialect anyway (one column list per
	// dialect is how a read shape drifts from its twin), so it round-trips in
	// local mode and nothing reads it there.
	APITokenMaxAgeDays int

	// MarketplaceEnabled was originally scoped as a ship-dark toggle for the
	// within-org prompt marketplace (TFAC-535); that turned out to be the
	// wrong surface to gate — the within-org marketplace has no toggle and
	// is always on for every multi-mode org (internal/server/
	// marketplace_handler.go's gateMarketplace doesn't read this field).
	// This column is reserved for the future cross-org marketplace gate
	// instead (TFAC-92 phase 2 / TFAC-539). NOT NULL DEFAULT false on both
	// backends — no NULL-round-trip subtlety like the fields above.
	MarketplaceEnabled bool

	// GitHubCredentialClass names which credential system this org's GitHub
	// access belongs to. See the GitHubCredentialClass type doc for the values
	// and the invariant.
	//
	// READ-ONLY THROUGH THIS STRUCT. UpdateSettings does NOT own the column and
	// deliberately omits it from its upsert's column lists, so a bulk settings
	// save leaves whatever the credential transitions wrote. Setting this field
	// and calling UpdateSettings silently does nothing; the only writer is
	// OrgsStore.SetGitHubCredentialClass, called from the credential
	// transitions inside their own transaction.
	GitHubCredentialClass GitHubCredentialClass

	// Version is the row's optimistic-concurrency token. The settings save is a
	// read-modify-write over the whole struct, so two admins editing different
	// sections of one page would otherwise silently overwrite each other; the
	// API hands this out on the read and requires it on the write, refusing a
	// write whose token is stale.
	//
	// READ-ONLY THROUGH THIS STRUCT, like GitHubCredentialClass above but for a
	// different reason: the counter belongs to the store, not the caller.
	// UpdateSettings ignores the field and bumps the stored value; the only way
	// to assert a version is UpdateSettingsVersioned's explicit argument, which
	// is what makes the assertion visible at the call site rather than smuggled
	// in a struct field a caller could forget to carry.
	//
	// 0 means "no row" — the value DefaultOrgSettings() carries, and what a
	// missing-row read hands back. A materialized row is always >= 1.
	Version int
}

// GitHubCredentialClass names which credential system an org's GitHub access
// belongs to. It is a projection of the credential choice the user already
// made — binding a PAT, registering or importing an App — never a separate
// setting, and there is no product surface that picks it.
//
// It exists because the class cannot be inferred from the presence of an
// org_github_apps row. A row is absent for a PAT org today, and it would also
// be absent for an org riding a deployment-level shared App (org_github_apps
// is keyed one row per org with a UNIQUE app_id, so N orgs cannot each hold a
// row pointing at one shared app). Reading "no row" as "PAT" is therefore an
// inference that is right by accident, and every site that makes it would be
// silently wrong the day a second rowless class exists — degrading an org to a
// credential it does not have rather than failing where someone would notice.
//
// The class names WHICH CREDENTIAL SYSTEM the org is in; org_github_apps.active
// names WHICH CREDENTIAL IS LIVE. They are orthogonal, and the staged window of
// a PAT→App switch is where they visibly differ: the class is byo_app from the
// moment the App is registered, while the PAT stays live until cutover. Do not
// collapse them.
//
// Values on THIS column are app-validated, not CHECK-constrained — the same
// convention the other open-set text columns follow
// (org_settings.background_jobs_model, prompts.source), so a new class costs no
// DDL here on either backend.
//
// Here, and nowhere else. A class is not free of DDL merely because the column
// that states it is: other tables MIRROR the value under their own constraints,
// and reachable_repositories is the live example — its credential_class carries
// a CHECK naming the accepted values, a row-shape CHECK pairing each value with
// the scope column its rows must hold, and two partial unique indexes each
// predicated on a literal class. A value that table has not been taught is one
// it cannot store, and (past the CHECK) one it cannot keep unique either. Ask
// what mirrors the class before assuming a new one is a Go-only change.
//
// "managed_app" — an org riding the deployment's own shared App — had no
// constant here for as long as the value was unreachable: a constant is a thing
// a writer can reach for, and the shared App needed a resolver tier, a scoped
// reconcile, and a static webhook path before any org could legitimately carry
// the value. The resolver tier now exists, so the constant below is safe to
// define — but STILL NOTHING WRITES IT. The bind ceremony is what first sets an
// org's class to managed_app; until then the value is one this build can
// resolve and no path can produce. The rule that withheld it is worth keeping:
// a class becomes reachable when the machinery behind it exists, not when a
// caller wants the name.
//
// That is also why every switch on this type carries an explicit unknown arm
// that refuses instead of falling through to the pat arm: a build that meets a
// class it does not know must fail where it is seen.
type GitHubCredentialClass string

const (
	// GitHubCredentialClassPAT — the org's GitHub credential is a personal
	// access token in the secret store. No org_github_apps row exists.
	GitHubCredentialClassPAT GitHubCredentialClass = "pat"

	// GitHubCredentialClassBYOApp — the org brought its own GitHub App and owns
	// its private key; an org_github_apps row exists. Set at registration /
	// import, including while the App is still staged behind a live PAT.
	GitHubCredentialClassBYOApp GitHubCredentialClass = "byo_app"

	// GitHubCredentialClassManagedApp — the org rides the deployment's own
	// shared GitHub App, whose identity and key live in the operator's
	// environment. NO org_github_apps row exists, and none can: that table is
	// one row per org with a UNIQUE app_id, so N orgs cannot each hold a row
	// naming the one shared App. The org's own rows are its installations —
	// which accounts it bound the shared App on.
	//
	// Multi mode only in effect: a distributed local binary ships no shared
	// key, so githubapp.DeploymentAppFromEnv returns the zero App there and a
	// local org resolves nothing under this class.
	GitHubCredentialClassManagedApp GitHubCredentialClass = "managed_app"
)

// Known reports whether c is a class this build understands. A stored value
// that isn't — a future class written by a newer peer, a hand-edited row — is
// never coerced to a default; callers refuse instead, so an org resolves no
// credential rather than the wrong one.
func (c GitHubCredentialClass) Known() bool {
	switch c {
	case GitHubCredentialClassPAT, GitHubCredentialClassBYOApp, GitHubCredentialClassManagedApp:
		return true
	default:
		return false
	}
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
		GitHubPollInterval: 5 * time.Minute,
		// HTTPS is the only protocol that can carry the org's OWN credential —
		// a PAT and an App installation token are both HTTPS bearer
		// credentials — so it is what an org that has expressed no preference
		// should get. SSH stays available and is honored end to end when
		// chosen; it just authenticates as whoever owns the operator's key,
		// which is not a thing to land on by default.
		GitHubCloneProtocol: "https",
		JiraPollInterval:    5 * time.Minute,
		// Matches the column's NOT NULL DEFAULT 'pat'. An org with no
		// settings row has bound no credential at all, and "the PAT system,
		// with nothing in it yet" is the state a fresh org is in.
		GitHubCredentialClass: GitHubCredentialClassPAT,
		// Matches the SQLite column's NOT NULL DEFAULT, and correct for local
		// the way GitHubCloneProtocol above is: multi mode overrides it at the
		// read (EffectiveLLMAuthMethod) rather than storing something else.
		LLMAuthMethod: LLMAuthSystem,
	}
}

// EffectiveCloneProtocol resolves the clone protocol actually in force, given
// the stored org setting and whether the deployment is multi-mode.
//
// Multi-mode is ALWAYS "https", independent of the stored value: a GitHub App
// installation token is an HTTPS bearer credential that cannot be used over
// SSH at all, and the multi-mode runtime container has no ssh-agent / key /
// known_hosts. The write path refuses an "ssh" value in multi, but a stored one
// can still reach here — a legacy row, or a row written outside the API — so the
// read coerces rather than trusting the column.
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
// DefaultModel + AutoDelegateEnabled moved off user_settings:
// the team owns the AI behavior policy, users do not override in v1.
type TeamSettings struct {
	JiraProjects               []string
	AIReprioritizeThreshold    int
	AIPreferenceUpdateInterval int
	DefaultModel               string // a model catalog key; "" inherits
	AutoDelegateEnabled        bool
	// AutoModeEnabled starts SDK-runtime delegated conversations in Claude
	// Code's auto permission mode. Local mode honors the stored team choice;
	// multi-mode SDK conversations always enable auto mode, and the native
	// runtime ignores this field entirely.
	AutoModeEnabled bool

	// PermissionAbsentGraceMS + PermissionAbsentAutodenyEnabled gate the
	// presence-aware fast auto-deny for unattended permission prompts (TFAC-392).
	// When the toggle is on and a delegated conversation raises an
	// off-allowlist tool prompt with no answer-capable, focused tab present in
	// the conversation's org, the backend denies after this grace window (ms) instead of waiting the full
	// permTimeout(). When off, the prompt keeps the full-timeout behavior exactly.
	// The grace is clamped at spawn to [1s, permTimeout()) so it can never invert
	// the "total wait < idleTimeout()" invariant.
	PermissionAbsentGraceMS         int
	PermissionAbsentAutodenyEnabled bool

	// MaxDailyCostUSD is the per-team daily LLM spend cap (TFAC-482), the
	// team-scoped sibling of OrgSettings.MaxDailyCostUSD. 0 = no cap (round-trips
	// 0 ↔ NULL). When today's team spend (UTC calendar day, summed over the
	// team's own rows — system overhead carries a NULL team_id
	// and never counts) is >= this value AND the governance entitlement is active,
	// the delegation choke point refuses new agent conversations for that team. Org-admin-
	// configured: a team admin cannot set their own team's cap (the team-settings
	// write path never touches this field — only the org-admin cap endpoint does),
	// so a team-admin save round-trips the stored value untouched.
	MaxDailyCostUSD float64

	// BranchTemplate is the team's branch-name convention (TFAC-498), rendered
	// into the delegated agent's prompt as envelope guidance — it is NOT
	// enforced. The literal "<ticket-id>" is substituted with the conversation's ticket
	// id at prompt-render time. NOT NULL with a schema DEFAULT; defaults to
	// DefaultBranchTemplate. The write path coalesces an empty string to the
	// default so a blank never persists.
	BranchTemplate string

	// ReviewPosture is how this team's delegated reviews reach GitHub — one of
	// ValidReviewPostures, read at finalize time by the agenthost's review
	// finalize choke point. NOT NULL with a schema DEFAULT; defaults to
	// DefaultReviewPosture ("identity" — derive from the acting credential).
	// The write path coalesces an empty string to the default so a blank never
	// persists. Deliberately team-grained, with no per-prompt override: a
	// prompt author must not be able to opt their conversations out of the team's gate.
	ReviewPosture string

	// EnabledModels is the team's enable-set: which of the models its org
	// enables this team may pick from, stored as a JSON array of model keys in
	// the same vocabulary as DefaultModel above. nil is the absent value and
	// inherits the org's effective set whole.
	//
	// A subset of the org's set at every save — the write refuses a superset —
	// and narrowed to it again at every read, because the org may shrink its own
	// set afterwards and nothing rewrites a team row when it does. TeamModelSet
	// is where both halves meet.
	//
	// It is forward-acting. Narrowing a team does not rewrite the model already
	// pinned on its prompts or stored as its default — those are caught at the
	// next dispatch, which refuses by name rather than substituting a model
	// nobody chose.
	EnabledModels []string

	// BaseBranchPushPolicy is whether this team's delegated agents may push to
	// a repo's base / default branch — one of ValidBaseBranchPushPolicies, read
	// by the per-run git-proxy ref gate (with the pre-push hook as a no-proxy
	// fallback) through internal/pushpolicy. NOT NULL with a
	// schema DEFAULT; defaults to DefaultBaseBranchPushPolicy ("never"). The
	// write path coalesces an empty string to the default so a blank never
	// persists. Team-grained with no per-prompt override, for the same reason
	// as ReviewPosture and more sharply: task context is minted from PR bodies,
	// issue comments and labels, so task text saying "push to main" must never
	// be sufficient authority to unlock a base-branch push.
	BaseBranchPushPolicy string
}

// DefaultTeamSettingsFor returns the NOT NULL DEFAULT values from the
// team_settings schema as a Go struct. Same pattern as
// DefaultOrgSettings — read-side fallback for missing rows, plus an
// explicit Go-side baseline for provisioning paths.
//
// It takes the mode because one of those DEFAULTs diverges by dialect:
// default_model is stored in the vocabulary that dialect's runtime dispatches,
// so a mode-blind answer would hand a caller a model its own deployment refuses.
// The dialect is the mode, so a store passes the one it is.
//
// AutoDelegateEnabled defaults true (matching the schema DEFAULT): a
// team that has a trigger enabled means the run to fire, and every
// shipped trigger is off until a human opts in, so a second global
// gate defaulting off just silently swallows the trigger they enabled
// (a Slack channel claim, for instance, seeds an enabled mention
// trigger that never fired). The per-team toggle stays, for teams that
// want review-before-run.
func DefaultTeamSettingsFor(multiMode bool) TeamSettings {
	return TeamSettings{
		AIReprioritizeThreshold:         5,
		AIPreferenceUpdateInterval:      20,
		DefaultModel:                    DefaultModelFor(multiMode),
		AutoDelegateEnabled:             true,
		AutoModeEnabled:                 true,
		PermissionAbsentGraceMS:         15000,
		PermissionAbsentAutodenyEnabled: true,
		BranchTemplate:                  DefaultBranchTemplate,
		ReviewPosture:                   DefaultReviewPosture,
		BaseBranchPushPolicy:            DefaultBaseBranchPushPolicy,
	}
}

// UserSettings is the user-scope settings row (user_settings table): what a
// person sets about their own view. Anything a team decides for its members —
// the AI model, the auto-delegate toggle — is TeamSettings, not here.
type UserSettings struct {
	// OverviewSeenAt is when this user last opened the Overview — the anchor
	// its away line reads from ("you were last here at 18:40 yesterday") and
	// the point its counts are measured since.
	//
	// nil means never opened, which is a different sentence rather than a very
	// old one: the page says so and falls back to midnight.
	//
	// The client writes it, and the row is keyed by user alone, so a multi-org
	// person carries one marker across their orgs. Both are accepted because
	// the value anchors a line of prose read at minute resolution, not a query.
	OverviewSeenAt *time.Time `json:"overview_seen_at"`
}

// JiraStatusRef is one workflow status as the rules store it: the id Jira's
// workflow references it by, plus the display name captured beside it.
//
// The id is the identity. A workflow points at the status entity, so the id
// survives a rename and the name does not — which is what makes an id-built
// rule stable in the two places TF acts on one: the discovery JQL
// (`status IN (10001, 10002)` is valid JQL) and the transition performed at
// claim and complete time. The name rides along so a rule renders without a
// live fetch, and it is refreshed from Jira every time the rule is saved.
//
// A ref carrying a name and no id is a row written before statuses were
// identified. Nothing rewrites those in place — resolving an id means asking
// Jira, and no background job does that on a team's behalf — so readers fall
// back to matching on the name and the ids fill on the row's next save.
type JiraStatusRef struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// IsZero reports whether the ref names nothing at all — an unset canonical,
// which is what an unmapped write-target rule carries.
func (r JiraStatusRef) IsZero() bool { return r.ID == "" && r.Name == "" }

// SameStatus reports whether two refs name the same status. Ids decide when
// both carry one; otherwise the names do, which is what lets a legacy
// name-only ref still compare equal to itself.
func (r JiraStatusRef) SameStatus(other JiraStatusRef) bool {
	if r.ID != "" && other.ID != "" {
		return r.ID == other.ID
	}
	return r.Name != "" && r.Name == other.Name
}

// JiraStatusNames renders refs as their display names — for the surfaces that
// compare against a name because a name is all they hold (a poll snapshot's
// status, a live issue's current status).
func JiraStatusNames(refs []JiraStatusRef) []string {
	out := make([]string, 0, len(refs))
	for _, r := range refs {
		if r.Name != "" {
			out = append(out, r.Name)
		}
	}
	return out
}

// JiraStatusIDs renders refs as their identifiers, skipping the ones that
// carry none — for the surfaces that can match on an id but must not treat a
// name-only ref as having an empty one.
func JiraStatusIDs(refs []JiraStatusRef) []string {
	out := make([]string, 0, len(refs))
	for _, r := range refs {
		if r.ID != "" {
			out = append(out, r.ID)
		}
	}
	return out
}

// --- the stored form of a status rule's members ---
//
// Rules persist as JSON in both dialects (SQLite TEXT, Postgres jsonb), so the
// encoding lives here with the type rather than twice in the store impls.
//
// Both decoders accept the shape written before statuses were identified — a
// bare name where an object now sits — because those rows are live data, not a
// migration that has not run yet: there is no id to backfill without asking
// Jira, and nothing asks on a team's behalf. They decode to a ref with a name
// and no id, which is exactly what the readers' name fallback expects, and the
// next save of that rule writes the object form.

// MarshalJiraStatusRefs renders a rule's members for storage. A nil slice
// renders as [] rather than null, so the stored value is always a JSON array
// and the CHECK constraints have one empty form to recognize.
func MarshalJiraStatusRefs(refs []JiraStatusRef) (string, error) {
	if refs == nil {
		refs = []JiraStatusRef{}
	}
	raw, err := json.Marshal(refs)
	if err != nil {
		return "", err
	}
	return string(raw), nil
}

// UnmarshalJiraStatusRefs reads a rule's members back.
func UnmarshalJiraStatusRefs(raw string) ([]JiraStatusRef, error) {
	if strings.TrimSpace(raw) == "" {
		return []JiraStatusRef{}, nil
	}
	var elems []json.RawMessage
	if err := json.Unmarshal([]byte(raw), &elems); err != nil {
		return nil, err
	}
	out := make([]JiraStatusRef, 0, len(elems))
	for _, elem := range elems {
		ref, err := decodeJiraStatusRef(elem)
		if err != nil {
			return nil, err
		}
		out = append(out, ref)
	}
	return out, nil
}

// MarshalJiraStatusRef renders a canonical for storage, or "" for an unset one
// — which the stores write as SQL NULL.
func MarshalJiraStatusRef(ref JiraStatusRef) (string, error) {
	if ref.IsZero() {
		return "", nil
	}
	raw, err := json.Marshal(ref)
	if err != nil {
		return "", err
	}
	return string(raw), nil
}

// UnmarshalJiraStatusRef reads a canonical back. An empty column is an unset
// canonical — an unarmed write-target rule — not an error.
func UnmarshalJiraStatusRef(raw string) (JiraStatusRef, error) {
	if strings.TrimSpace(raw) == "" {
		return JiraStatusRef{}, nil
	}
	return decodeJiraStatusRef(json.RawMessage(raw))
}

func decodeJiraStatusRef(raw json.RawMessage) (JiraStatusRef, error) {
	trimmed := strings.TrimSpace(string(raw))
	switch {
	case strings.HasPrefix(trimmed, "{"):
		var ref JiraStatusRef
		if err := json.Unmarshal(raw, &ref); err != nil {
			return JiraStatusRef{}, err
		}
		return ref, nil
	case strings.HasPrefix(trimmed, `"`):
		// A name inside a members array, which was a JSON array of strings
		// before statuses were identified.
		var name string
		if err := json.Unmarshal(raw, &name); err != nil {
			return JiraStatusRef{}, err
		}
		return JiraStatusRef{Name: name}, nil
	default:
		// Not JSON at all: the bare display name a canonical column held
		// before statuses were identified, which was never quoted because the
		// column stored the name itself rather than a document.
		return JiraStatusRef{Name: trimmed}, nil
	}
}

// JiraStatusDedupKey is the value a ref is deduplicated and set-compared on:
// the id when it has one, else the name under a prefix so a legacy name-only
// ref can never collide with an id that happens to read like it.
func JiraStatusDedupKey(r JiraStatusRef) string {
	if r.ID != "" {
		return "id:" + r.ID
	}
	return "name:" + r.Name
}

// JiraProjectStatusRules is one row of jira_project_status_rules —
// the team's status configuration for a single Jira project. Multiple
// rows per team (keyed `(team_id, project_key)`) so two projects on
// the same team can have different workflows.
//
// A row is the team's commitment to WATCH the project. Whether it is ARMED —
// see Armed — is a second, later state: the rules may be entirely empty, and
// each of them is independently complete-or-empty, which the table's CHECK
// constraints mirror for the three write-target rules.
type JiraProjectStatusRules struct {
	// TeamID is the owning team (the PK's first column). The List* store
	// methods populate it; ReplaceForTeam ignores it (the team is a
	// parameter). Needed by the poller's per-project member merge (to
	// break canonical ties by lowest team_id) and the router's team↔project
	// gate (to look up the handler's team).
	TeamID     string
	ProjectKey string
	// The four rules. A canonical is always one of its rule's members, so it
	// carries no information its member entry doesn't — it is stored as a full
	// ref anyway so a caller performing a transition has the id and the name in
	// hand without a lookup.
	//
	// InReview names the status a ticket sits in while the work it tracks
	// awaits human review. It is optional, and it is the one rule here that
	// feeds nothing TF polls or classifies on. In particular no Jira status is
	// ever read back into TF's in_review board column: that column is a fact
	// about a RUN (agent work awaiting a human) rather than about a ticket, so
	// a ticket somebody moved to "Code Review" by hand belongs on no TF board
	// at all. Its members reach neither the discovery JQL nor the stock deck's
	// buckets, which is why a status may sit in BOTH InProgressMembers and
	// InReviewMembers — "counts as actively worked on" is true of a ticket
	// under review.
	//
	// TODO(TFAC-883): nothing writes this status onto a ticket. The rule is
	// stored and settable; whether TF should act on it, and off which signal,
	// is still open.
	PickupMembers       []JiraStatusRef
	InProgressMembers   []JiraStatusRef
	InProgressCanonical JiraStatusRef
	InReviewMembers     []JiraStatusRef
	InReviewCanonical   JiraStatusRef
	DoneMembers         []JiraStatusRef
	DoneCanonical       JiraStatusRef
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
// that the router's team↔repo gate consults and that repositories is
// the org-wide UNION of. Distinct from TeamGitHubGroup, which maps
// CODEOWNERS review-routing teams — this is tracking scope. The Owner is
// stored as-typed for display fidelity (GitHub logins are
// case-insensitive); matching against event metadata is done
// case-insensitively at the gate.
type TeamGitHubRepo struct {
	Owner string
	Repo  string
}

// Slug returns the canonical "owner/repo" form used as the repositories
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
// repositories.id and the GitHub clone URL verbatim), so dedup is
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
	// reconcile, in repositories (a double-polled repo). First-seen
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

// Armed reports whether this project's rules are complete enough for TF to act
// on the project: pickup members to discover with, and members-plus-canonical
// on the in-progress and done write targets. An unarmed project is WATCHED but
// not yet mapped — a valid stored state, reached by adding it from the picker
// and left there until someone maps its workflow's statuses — and it
// contributes nothing to the discovery JQL, so the poller skips it.
//
// InReview is deliberately excluded: it gates nothing the poller asks, and a
// project that leaves it unmapped is completely configured, so requiring it
// here would un-arm every project already armed.
func (r JiraProjectStatusRules) Armed() bool {
	return len(r.PickupMembers) > 0 &&
		len(r.InProgressMembers) > 0 && !r.InProgressCanonical.IsZero() &&
		len(r.DoneMembers) > 0 && !r.DoneCanonical.IsZero()
}

// The membership tests take a full ref and compare through SameStatus,
// so the id decides whenever both sides carry one and a status renamed in Jira
// keeps matching. The name is the fallback, and it is not a transitional one:
// a rule seeded from the headless env vars is name-only by contract, and so is
// any snapshot captured before status ids were recorded.

// PickupContains reports whether status is a member of the Pickup rule.
func (r JiraProjectStatusRules) PickupContains(status JiraStatusRef) bool {
	return containsStatus(r.PickupMembers, status)
}

// InProgressContains reports whether status is a member of the InProgress rule.
func (r JiraProjectStatusRules) InProgressContains(status JiraStatusRef) bool {
	return containsStatus(r.InProgressMembers, status)
}

// InReviewContains reports whether status is a member of the InReview rule.
// The rule is optional, and an unmapped one has no members, so this is false
// everywhere for a project that never mapped it.
func (r JiraProjectStatusRules) InReviewContains(status JiraStatusRef) bool {
	return containsStatus(r.InReviewMembers, status)
}

// DoneContains reports whether status is a member of the Done rule.
func (r JiraProjectStatusRules) DoneContains(status JiraStatusRef) bool {
	return containsStatus(r.DoneMembers, status)
}

func containsStatus(refs []JiraStatusRef, status JiraStatusRef) bool {
	return slices.ContainsFunc(refs, func(r JiraStatusRef) bool { return r.SameStatus(status) })
}

// ContainsStatus reports whether a set of refs holds one naming the same
// status — the free-standing form of the membership tests, for the callers
// that hold a union of several rules' members rather than one rule.
func ContainsStatus(refs []JiraStatusRef, status JiraStatusRef) bool {
	return containsStatus(refs, status)
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
func JiraAllPickupMembers(rules []JiraProjectStatusRules) []JiraStatusRef {
	return jiraUnionMembers(rules, func(p JiraProjectStatusRules) []JiraStatusRef { return p.PickupMembers })
}

// JiraAllDoneMembers returns the union of every project's done members.
// Used by JQL queries that exclude terminal tickets across the team's
// full project list.
func JiraAllDoneMembers(rules []JiraProjectStatusRules) []JiraStatusRef {
	return jiraUnionMembers(rules, func(p JiraProjectStatusRules) []JiraStatusRef { return p.DoneMembers })
}

func jiraUnionMembers(rules []JiraProjectStatusRules, pick func(JiraProjectStatusRules) []JiraStatusRef) []JiraStatusRef {
	seen := map[string]bool{}
	out := make([]JiraStatusRef, 0)
	for _, p := range rules {
		for _, m := range pick(p) {
			if key := JiraStatusDedupKey(m); !seen[key] {
				seen[key] = true
				out = append(out, m)
			}
		}
	}
	return out
}
