package db

import (
	"context"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// OrgsStore owns the orgs + org_settings tables — the tenancy root
// every other resource hangs off via FK plus its sibling settings row.
// Background services (poller, tracker, projectclassify, repoprofile)
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
//   - GetSettings and UpdateSettings run on the app pool. The
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
	// BootstrapNewOrg seeds the template + materializes the team.
	// Re-entrant (INSERT OR IGNORE) so a re-run reaches the same end
	// state. Local-mode only: the Postgres impl returns
	// ErrNotApplicableInLocal — multi-mode creates real tenant rows per
	// signup in auth_provision.go.
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
	// time. Empty GitHubBaseURL / JiraBaseURL / vault refs /
	// MaxLLMModelTier reflect NULL columns ("not configured yet" /
	// "use deployment default" / "no cap"). Postgres routes through
	// the app pool (org_settings_select RLS gates by org membership).
	GetSettings(ctx context.Context, orgID string) (domain.OrgSettings, error)

	// GetSettingsSystem mirrors GetSettings but routes through the
	// admin pool in Postgres for callers without a JWT-claims context
	// (pollers, scorer, delegation spawner). SQLite collapses to the
	// same impl. Same defaults-on-ErrNoRows contract.
	GetSettingsSystem(ctx context.Context, orgID string) (domain.OrgSettings, error)

	// UpdateSettings upserts the org's settings row. An empty
	// GitHubBaseURL / JiraBaseURL / AnthropicAPIKeyRef /
	// BedrockCredentialsRef / MaxLLMModelTier writes NULL into the
	// column. An empty GitHubCloneProtocol substitutes "ssh" — the
	// column CHECK rejects empty strings. Postgres routes through
	// the app pool (org_settings_update RLS gates by org admin).
	UpdateSettings(ctx context.Context, orgID string, updates domain.OrgSettings) error
}
