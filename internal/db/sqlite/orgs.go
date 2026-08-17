package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// orgsStore is the SQLite impl of db.OrgsStore. The local-mode orgs
// table has no soft-delete column — every row is considered active.
// In practice this returns the single runmode.LocalDefaultOrgID
// sentinel seeded by the baseline migration, but the SQL
// makes no assumption about that count so a hypothetical future test
// fixture that inserts additional rows iterates them correctly.
//
// orgsStore — SQLite impl. The constructor accepts two queryers for
// signature parity with the Postgres impl; SQLite has one
// connection so both collapse to the same queryer. The `...System`
// variants delegate to their non-System counterparts.
type orgsStore struct{ q queryer }

func newOrgsStore(q, _ queryer) db.OrgsStore { return &orgsStore{q: q} }

var _ db.OrgsStore = (*orgsStore)(nil)

func (s *orgsStore) GetOrg(ctx context.Context, orgID string) (*domain.Org, error) {
	var o domain.Org
	err := s.q.QueryRowContext(ctx, `
		SELECT id, name, slug, created_at FROM orgs WHERE id = ?
	`, orgID).Scan(&o.ID, &o.Name, &o.Slug, &o.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read org: %w", err)
	}
	return &o, nil
}

func (s *orgsStore) GetOrgSystem(ctx context.Context, orgID string) (*domain.Org, error) {
	return s.GetOrg(ctx, orgID)
}

func (s *orgsStore) CreateLocalTenant(ctx context.Context) error {
	// Wrap the multi-statement seed in a single transaction so the tenant
	// appears all-at-once to other goroutines. Without this, each
	// INSERT OR IGNORE auto-commits separately and a background loop that
	// enumerates orgs (poller ListActiveSystem, scorer) could observe the
	// orgs row before its teams / org_settings / team_settings land,
	// causing transient "missing settings" errors. inTx reuses the caller's
	// tx when CreateLocalTenant is composed inside one, or opens a fresh
	// tx against the *sql.DB otherwise (the provision-action path).
	return inTx(ctx, s.q, func(q queryer) error {
		return db.SeedLocalTenantRows(ctx, q)
	})
}

func (s *orgsStore) ListActiveSystem(ctx context.Context) ([]string, error) {
	rows, err := s.q.QueryContext(ctx, `SELECT id FROM orgs ORDER BY id ASC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []string{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

func (s *orgsStore) GetSettings(ctx context.Context, orgID string) (domain.OrgSettings, error) {
	return getOrgSettings(ctx, s.q, orgID)
}

func (s *orgsStore) GetSettingsSystem(ctx context.Context, orgID string) (domain.OrgSettings, error) {
	return getOrgSettings(ctx, s.q, orgID)
}

func getOrgSettings(ctx context.Context, q queryer, orgID string) (domain.OrgSettings, error) {
	var (
		ghURL, jiraURL, anthRef, bedRef, maxTier sql.NullString
		ghInterval, jiraInterval                 string
		cloneProto                               string
		maxDailyCost                             sql.NullFloat64
		maxConcurrentRuns                        sql.NullInt64
		marketplaceEnabled                       bool
		credentialClass                          string
		version                                  int
	)
	err := q.QueryRowContext(ctx, `
		SELECT github_base_url, github_poll_interval, github_clone_protocol,
		       jira_base_url, jira_poll_interval,
		       anthropic_api_key_ref, bedrock_credentials_ref, max_llm_model_tier,
		       max_daily_cost_usd, max_concurrent_runs, marketplace_enabled,
		       github_credential_class, version
		FROM org_settings WHERE org_id = ?
	`, orgID).Scan(
		&ghURL, &ghInterval, &cloneProto,
		&jiraURL, &jiraInterval,
		&anthRef, &bedRef, &maxTier,
		&maxDailyCost, &maxConcurrentRuns, &marketplaceEnabled,
		&credentialClass, &version,
	)
	if errors.Is(err, sql.ErrNoRows) {
		// Provisioning is meant to seed an org_settings row at org-
		// create time (baseline migration for the local sentinel,
		// auth provisioning for multi-mode tenants). The defaults
		// here are a belt-and-suspenders fallback so test fixtures
		// that build a raw DB without going through provisioning
		// still see sensible values (5m poll intervals, ssh clone
		// protocol). Matches the schema DEFAULT clauses.
		return domain.DefaultOrgSettings(), nil
	}
	if err != nil {
		return domain.OrgSettings{}, fmt.Errorf("read org_settings: %w", err)
	}
	ghDur, err := time.ParseDuration(ghInterval)
	if err != nil {
		return domain.OrgSettings{}, fmt.Errorf("parse org_settings github_poll_interval %q: %w", ghInterval, err)
	}
	jiraDur, err := time.ParseDuration(jiraInterval)
	if err != nil {
		return domain.OrgSettings{}, fmt.Errorf("parse org_settings jira_poll_interval %q: %w", jiraInterval, err)
	}
	// Clamp a stray negative up to 0. No DB CHECK guards this column, and
	// everything downstream (the claim) reads <= 0 as unlimited — so surface a
	// negative as 0 here too, keeping "unlimited" consistently 0 and never
	// handing the settings UI a value it rejects.
	concurrentRuns := int(maxConcurrentRuns.Int64) // NULL → 0 (unlimited)
	if concurrentRuns < 0 {
		concurrentRuns = 0
	}
	return domain.OrgSettings{
		GitHubBaseURL:         ghURL.String,
		GitHubPollInterval:    ghDur,
		GitHubCloneProtocol:   cloneProto,
		JiraBaseURL:           jiraURL.String,
		JiraPollInterval:      jiraDur,
		AnthropicAPIKeyRef:    anthRef.String,
		BedrockCredentialsRef: bedRef.String,
		MaxLLMModelTier:       maxTier.String,
		MaxDailyCostUSD:       maxDailyCost.Float64, // NULL → 0 (no cap)
		MaxConcurrentRuns:     concurrentRuns,
		MarketplaceEnabled:    marketplaceEnabled,
		// Surfaced verbatim, never coerced to a known value: callers switch on
		// it and refuse what they don't recognise, which is the whole point of
		// storing the class instead of inferring it.
		GitHubCredentialClass: domain.GitHubCredentialClass(credentialClass),
		Version:               version,
	}, nil
}

// UpdateSettings upserts every org_settings column this writer owns.
//
// github_credential_class is deliberately absent from BOTH the INSERT column
// list and the ON CONFLICT SET list, and must stay that way. Absent from both,
// the column takes its DEFAULT on insert and is left untouched on update —
// exactly the behaviour required, because the class is owned by the credential
// transitions (SetGitHubCredentialClass), not by the settings writer. Adding it
// here would look like tidiness and would instead reset the class to the
// struct's zero value on every bulk settings save, silently converting a
// BYO-App org to PAT. u.GitHubCredentialClass is read-only; it is ignored here.
func (s *orgsStore) UpdateSettings(ctx context.Context, orgID string, u domain.OrgSettings) error {
	// No version guard: an unguarded save is last-writer-wins on purpose. Its
	// callers are the credential transitions, which own the specific fields
	// they touch and have nothing to lose a race about.
	if _, err := s.upsertSettings(ctx, orgID, u, nil); err != nil {
		return fmt.Errorf("upsert org_settings: %w", err)
	}
	return nil
}

// UpdateSettingsVersioned is UpdateSettings under the row's concurrency token.
// The guard rides the ON CONFLICT DO UPDATE's WHERE, so a losing writer updates
// zero rows rather than overwriting the winner; expected 0 (no row) resolves
// through the INSERT arm, where the primary key does the same job.
//
// Local mode is N=1, so this practically never fires here — it exists so the
// two dialects answer the settings API identically rather than having the
// contract hold on one backend and be a comment on the other.
func (s *orgsStore) UpdateSettingsVersioned(ctx context.Context, orgID string, u domain.OrgSettings, expected int) error {
	res, err := s.upsertSettings(ctx, orgID, u, &expected)
	if err != nil {
		// A racing INSERT commits between our conflict probe and ours only in
		// the expected==0 arm, where there is no row for the WHERE to guard;
		// the primary key catches it, and it is the same conflict.
		if isUniqueViolation(err) {
			return db.ErrOrgSettingsVersion
		}
		return fmt.Errorf("upsert org_settings: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("upsert org_settings: %w", err)
	}
	if n == 0 {
		return db.ErrOrgSettingsVersion
	}
	return nil
}

// upsertSettings writes every org_settings column this writer owns and bumps
// the row's version. A non-nil expected adds the concurrency guard to the
// ON CONFLICT arm — the one difference between the guarded and unguarded
// writers, kept as a parameter so the two can never drift in which columns
// they write.
func (s *orgsStore) upsertSettings(ctx context.Context, orgID string, u domain.OrgSettings, expected *int) (sql.Result, error) {
	cloneProto := u.GitHubCloneProtocol
	if cloneProto == "" {
		cloneProto = "ssh"
	}
	args := []any{
		orgID,
		nullStringValue(u.GitHubBaseURL),
		u.GitHubPollInterval.String(),
		cloneProto,
		nullStringValue(u.JiraBaseURL),
		u.JiraPollInterval.String(),
		nullStringValue(u.AnthropicAPIKeyRef),
		nullStringValue(u.BedrockCredentialsRef),
		nullStringValue(u.MaxLLMModelTier),
		nullFloatValue(u.MaxDailyCostUSD),
		nullIntValue(u.MaxConcurrentRuns),
		u.MarketplaceEnabled,
	}
	guard := ""
	if expected != nil {
		guard = "WHERE org_settings.version = ?"
		args = append(args, *expected)
	}
	return s.q.ExecContext(ctx, `
		INSERT INTO org_settings (
			org_id, github_base_url, github_poll_interval, github_clone_protocol,
			jira_base_url, jira_poll_interval,
			anthropic_api_key_ref, bedrock_credentials_ref, max_llm_model_tier,
			max_daily_cost_usd, max_concurrent_runs, marketplace_enabled,
			version, updated_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 1, CURRENT_TIMESTAMP)
		ON CONFLICT(org_id) DO UPDATE SET
			github_base_url = excluded.github_base_url,
			github_poll_interval = excluded.github_poll_interval,
			github_clone_protocol = excluded.github_clone_protocol,
			jira_base_url = excluded.jira_base_url,
			jira_poll_interval = excluded.jira_poll_interval,
			anthropic_api_key_ref = excluded.anthropic_api_key_ref,
			bedrock_credentials_ref = excluded.bedrock_credentials_ref,
			max_llm_model_tier = excluded.max_llm_model_tier,
			max_daily_cost_usd = excluded.max_daily_cost_usd,
			max_concurrent_runs = excluded.max_concurrent_runs,
			marketplace_enabled = excluded.marketplace_enabled,
			version = org_settings.version + 1,
			updated_at = CURRENT_TIMESTAMP
		`+guard, args...)
}

// SetGitHubCredentialClass upserts ONLY org_settings.github_credential_class —
// which credential system the org's GitHub access belongs to. See the
// OrgsStore interface doc for why this is a separate writer from
// UpdateSettings. SQLite is N=1 / no RLS so it's a plain write; the Postgres
// twin runs on the app pool under the caller's claims.
//
// The partial INSERT relies on the schema DEFAULT clauses for every other
// org_settings column when no row exists yet, and ON CONFLICT touches only the
// class, so the org's other settings are never clobbered.
func (s *orgsStore) SetGitHubCredentialClass(ctx context.Context, orgID string, class domain.GitHubCredentialClass) error {
	if _, err := s.q.ExecContext(ctx, `
		INSERT INTO org_settings (org_id, github_credential_class, updated_at)
		VALUES (?, ?, CURRENT_TIMESTAMP)
		ON CONFLICT(org_id) DO UPDATE SET
			github_credential_class = excluded.github_credential_class,
			updated_at = CURRENT_TIMESTAMP
	`, orgID, string(class)); err != nil {
		return fmt.Errorf("set org github credential class: %w", err)
	}
	return nil
}

// nullStringValue returns nil when s is empty so the column lands SQL
// NULL — matches the Postgres impl's nullString helper. Renamed locally
// to avoid colliding with the existing agents.go nullString in this
// package.
func nullStringValue(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// nullFloatValue returns nil when f is 0 so the column lands SQL NULL — the
// numeric analog of nullStringValue (mirrors the Postgres impl's nullFloat).
// Used for nullable cost columns whose Go zero value means "unset"
// (org_settings.max_daily_cost_usd: 0 / NULL both mean "no cap").
func nullFloatValue(f float64) any {
	if f == 0 {
		return nil
	}
	return f
}

// nullIntValue returns nil when n is 0 so the column lands SQL NULL — the
// integer analog of nullFloatValue. Used for nullable ceiling columns whose
// Go zero value means "unset" (org_settings.max_concurrent_runs: 0 / NULL both
// mean "unlimited").
func nullIntValue(n int) any {
	if n == 0 {
		return nil
	}
	return n
}

// marshalJSONArray is shared by the team_settings + jira rules upserts:
// SQLite has no array type, so [string] is stored as a JSON text blob.
// orEmpty keeps "no projects configured" stable as "[]" rather than
// nil → NULL (the column ships NOT NULL DEFAULT '[]').
func marshalJSONArray(in []string) (string, error) {
	if in == nil {
		in = []string{}
	}
	b, err := json.Marshal(in)
	if err != nil {
		return "", err
	}
	return string(b), nil
}
