package db

import (
	"context"
	"errors"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// ErrOrgSettingsVersion means a versioned settings write lost the race: the
// row's stored version no longer matches the one the caller read, so another
// writer committed in between and nothing was written. The caller refetches
// and re-applies its edit — there is no server-side merge, because the two
// writers disagree about fields neither of them named.
var ErrOrgSettingsVersion = errors.New("org settings version conflict")

// OrgsStore owns the orgs + org_settings tables — the tenancy root
// every other resource hangs off via FK plus its sibling settings row.
// Background services (poller, tracker, repoprofile)
// iterate active orgs at the top of each cycle through ListActiveSystem;
// request handlers and system services read per-org settings via
// GetSettings / GetSettingsSystem.
//
// # Pool split (Postgres)
//
//   - ListActiveSystem, GetSettingsSystem run on the admin pool. The
//     callers are background goroutines launched at boot — they have
//     no JWT-claims context, and the work is by definition a cross-org
//     system-service read.
//   - GetSettings and UpdateSettingsVersioned run on the app pool. The
//     org_settings_select / org_settings_update RLS policies gate
//     reads by org membership and writes by org admin; the request-
//     handler caller has set the JWT claims via the TxRunner.
//
// SQLite collapses the pool split to one connection; the `...System`
// variants delegate to their non-System counterparts.
type OrgsStore interface {
	// GetOrg returns the org's metadata row, or nil if the org does
	// not exist. App pool in Postgres (RLS gates by org membership);
	// SQLite returns the sentinel row.
	GetOrg(ctx context.Context, orgID string) (*domain.Org, error)

	// GetOrgSystem mirrors GetOrg on the admin pool for callers
	// without JWT-claims context. SQLite collapses to GetOrg.
	GetOrgSystem(ctx context.Context, orgID string) (*domain.Org, error)

	// CreateLocalTenant idempotently inserts the synthetic local-mode
	// tenant rows (orgs / users / org_memberships / teams(Default) /
	// memberships / org_settings / team_settings) for the
	// runmode.LocalDefault* sentinels. It is the local "Start your
	// Triage Factory" provision action's first step, before
	// BootstrapNewOrg seeds the team's prompts/blueprints/handlers
	// directly from the shipped defaults.
	// Re-entrant (INSERT OR IGNORE) so a re-run reaches the same end
	// state. Local-mode only: the Postgres (multi-mode) impl returns a
	// clear "not supported in multi mode" error — multi-mode provisions
	// real tenant rows per signup in auth_provision.go.
	//
	// Exempt from the returned-row rule, by shape rather than by decision: it
	// is a bulk multi-table seed (orgs / users / org_memberships / teams /
	// memberships / org_settings / team_settings, each INSERT OR IGNORE) —
	// the same shape as TeamGitHubReposStore.ReplaceForTeam's set
	// reconciliation, not a single-row write with one row to hand back.
	// Nothing renders the seeded rows;
	// callers that need one read it back through the ordinary GetOrg /
	// GetSettings paths.
	CreateLocalTenant(ctx context.Context) error

	// ListActiveSystem returns the IDs of every active org in
	// ascending id order. "Active" means deleted_at IS NULL in
	// Postgres; SQLite has no soft-delete column, so the local-mode
	// impl returns every row (which collapses to the single
	// runmode.LocalDefaultOrgID sentinel seeded at install).
	//
	// Ordering is stable so per-org iteration is reproducible across
	// poll cycles — useful for log/test assertions and means a
	// partial failure in cycle N is followed by the same org order
	// in cycle N+1 unless rows changed.
	ListActiveSystem(ctx context.Context) ([]string, error)

	// GetSettings returns the org's settings row. On sql.ErrNoRows
	// it falls back to domain.DefaultOrgSettings() (matching the
	// schema DEFAULT clauses) so callers see a populated struct
	// rather than the Go zero value with "0s" poll intervals — the
	// row-missing case happens for test fixtures that bypass
	// provisioning; production paths always seed a row at org-create
	// time. Empty GitHubBaseURL / JiraBaseURL / vault refs and a nil
	// EnabledModels reflect NULL columns ("not configured yet" /
	// "use deployment default" / "no preference expressed"). Postgres
	// routes through the app pool (org_settings_select RLS gates by org
	// membership).
	GetSettings(ctx context.Context, orgID string) (domain.OrgSettings, error)

	// GetSettingsSystem mirrors GetSettings but routes through the
	// admin pool in Postgres for callers without a JWT-claims context
	// (pollers, scorer, delegation spawner). SQLite collapses to the
	// same impl. Same defaults-on-ErrNoRows contract.
	GetSettingsSystem(ctx context.Context, orgID string) (domain.OrgSettings, error)

	// UpdateSettingsVersioned writes the org's whole settings row, guarded by
	// the row's optimistic-concurrency token: the write lands only if the
	// stored version still equals expected, and otherwise fails with
	// ErrOrgSettingsVersion — nothing is written. It is the only whole-row
	// writer. A caller passes the version it read, in the same transaction, so
	// a settings write that committed in between fails this one instead of
	// being overwritten by it. A write that owns one value does not come
	// through here; it uses a targeted method that touches nothing else
	// (SetSourceBaseURL, SetGitHubCredentialClass, SetLinearWorkspace,
	// SetAnthropicKeyRef, SetBedrockCredentialsRef).
	//
	// An empty GitHubBaseURL / JiraBaseURL, and a nil EnabledModels, write NULL
	// into the column. An empty GitHubCloneProtocol substitutes "https" — the
	// column CHECK rejects empty strings, and this matches both the column
	// DEFAULT and DefaultOrgSettings, so no door onto it disagrees. Postgres
	// routes through the app pool (org_settings_update RLS gates by org admin).
	//
	// It does NOT write github_credential_class, linear_workspace_id,
	// linear_workspace_url_key, anthropic_api_key_ref or
	// bedrock_credentials_ref: those columns are owned by the credential
	// transitions, not by the settings writer, so the matching fields of
	// updates are ignored and an existing value survives every settings save.
	// See SetGitHubCredentialClass, SetLinearWorkspace, SetAnthropicKeyRef and
	// SetBedrockCredentialsRef.
	//
	// expected 0 asserts "no row yet" and is the ONLY value that may create
	// one, so two callers racing to materialize a settings row resolve to
	// exactly one winner. Any other expected asserts a stored version and never
	// creates: if the row is absent, that is the same conflict a moved version
	// gets, because either way the caller's read no longer describes the world.
	// A read of a missing row reports version 0 (domain.DefaultOrgSettings), so
	// passing the version GetSettings returned covers both cases.
	//
	// The guard is in the statement, not in a preceding read: READ COMMITTED
	// means a re-read inside the caller's own transaction cannot see a
	// concurrent commit, so a Go-side comparison would pass for both racers.
	//
	// On success, returns the persisted settings — the new version is the one
	// thing a successful caller most needs and cannot compute (expected+1 is a
	// guess, not a fact), sourced from RETURNING and projecting GetSettings'
	// column list and scanner. On ErrOrgSettingsVersion the returned settings
	// are the zero value: nothing was written, so there is no row to hand
	// back.
	UpdateSettingsVersioned(ctx context.Context, orgID string, updates domain.OrgSettings, expected int) (domain.OrgSettings, error)

	// SetGitHubCredentialClass writes ONLY org_settings.github_credential_class
	// — which credential system the org's GitHub access belongs to. Surgical by
	// design: it is called from inside the credential transitions' own
	// transactions (PAT bind, App register / import, cutover, switch-to-PAT,
	// discard), so the class and the credential change it describes commit
	// together and no crash can land between them.
	//
	// It is deliberately NOT reachable through UpdateSettingsVersioned.
	// Folding the column into that writer's column lists — which reads as
	// tidiness — would reset the class to the struct's zero value on every
	// bulk settings save, quietly converting a BYO-App org to PAT.
	//
	// The partial INSERT relies on the schema DEFAULT clauses for every other
	// column when no row exists yet, and ON CONFLICT touches only the class, so
	// the org's other settings are never clobbered. Postgres routes through the
	// app pool — every caller is an org-admin-gated handler already inside a
	// claims-bound transaction, which is exactly what org_settings_insert /
	// org_settings_update require.
	//
	// Returns the persisted settings — the insert arm fills every other
	// column from schema defaults, so the row this lands in may be one
	// nobody has read — sourced from RETURNING and projecting GetSettings'
	// column list and scanner.
	SetGitHubCredentialClass(ctx context.Context, orgID string, class domain.GitHubCredentialClass) (domain.OrgSettings, error)

	// SetLinearWorkspace writes ONLY org_settings.linear_workspace_id and
	// linear_workspace_url_key — the Linear workspace the org's credential
	// belongs to, learned from Linear when the credential is bound and
	// cleared ("" writes NULL) when it is unbound. Surgical for the reason
	// SetGitHubCredentialClass is: the credential bind and unbind call it
	// inside their own transactions, and no settings save can reach the
	// columns, so neither can put back a value the other just replaced.
	//
	// It does not bump the row's version. The settings writer cannot touch
	// these columns, so there is nothing for its concurrency token to guard,
	// and bumping it would fail an unrelated settings save that was loaded
	// before the bind.
	//
	// Same partial-INSERT shape, pool and return contract as
	// SetGitHubCredentialClass.
	SetLinearWorkspace(ctx context.Context, orgID, workspaceID, urlKey string) (domain.OrgSettings, error)

	// SetSourceBaseURL writes ONLY org_event_sources.base_url for kind
	// ("github" or "jira", the sources with a host to set; any other kind is
	// an error) — the host a credential bind validated against, or "" to
	// clear it when the credential is unbound. poll_interval and the disabled
	// columns on the same row are left as they are, and a row that does not
	// exist yet is created with their schema defaults.
	//
	// Unlike SetLinearWorkspace it bumps org_settings.version, in the same
	// transaction: the settings PATCH writes these hosts too, so a save loaded
	// before the credential write must conflict rather than put the old host
	// back. An org with no settings row gets one from schema defaults, the
	// way SetGitHubCredentialClass materializes it, so the version still
	// moves off what such an org reads.
	//
	// A host equal to the stored one writes nothing and moves nothing, and the
	// returned row is the current one. A save loaded before such a call has
	// nothing to put back, so a credential rotated on the same host, or an
	// unbind with no host left to clear, never fails an open settings edit.
	//
	// org_settings is written before org_event_sources, the order the
	// settings writers take the two rows in, so concurrent writers on Postgres
	// queue rather than deadlock. Pool and return contract as
	// SetGitHubCredentialClass.
	SetSourceBaseURL(ctx context.Context, orgID, kind, baseURL string) (domain.OrgSettings, error)

	// SetAnthropicKeyRef writes ONLY org_settings.anthropic_api_key_ref — the
	// secret key the org's Anthropic API key is stored under — from inside the
	// Anthropic credential bind and unbind. A non-empty ref also sets
	// llm_auth_method to domain.LLMAuthBYOK in the same statement: holding
	// provider material and running on the host's credentials are mutually
	// exclusive, and the bind is what settles which one the org is doing. ""
	// clears the ref (NULL) and leaves llm_auth_method alone, because removing
	// one provider's key says nothing about where the next run's credential
	// comes from.
	//
	// It bumps org_settings.version. The settings save cannot write the ref,
	// but it validates against it — it refuses llm_auth_method "system" while
	// either ref is set — and it writes llm_auth_method, so a save that ran
	// that check before the bind landed must conflict rather than store
	// "system" beside a credential.
	//
	// A call that would change neither column writes nothing and moves
	// nothing, so it never fails an open settings edit: rotating a key under
	// the same ref, clearing a ref that is already clear, and clearing on an
	// org with no settings row. A bind onto an org with no settings row
	// creates the row from schema defaults, as SetGitHubCredentialClass does.
	// A clear never creates one. A missing row already reads as having no ref,
	// and creating it would move the version and replace what GetSettings
	// reports for a missing row (domain.DefaultOrgSettings) with the column
	// defaults, which differ from it in both dialects.
	//
	// Pool as SetGitHubCredentialClass. A call that writes returns the stored
	// row from RETURNING, projecting GetSettings' column list and scanner; a
	// call that writes nothing returns what GetSettings returns.
	SetAnthropicKeyRef(ctx context.Context, orgID, ref string) (domain.OrgSettings, error)

	// SetBedrockCredentialsRef is SetAnthropicKeyRef for
	// org_settings.bedrock_credentials_ref — the key of the Bedrock shape the
	// org has bound, which also names that shape. Same llm_auth_method rule,
	// version rule, missing-row rule and return contract.
	SetBedrockCredentialsRef(ctx context.Context, orgID, ref string) (domain.OrgSettings, error)
}
