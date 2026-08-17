package postgres

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// orgsStore is the Postgres impl of db.OrgsStore. Holds both pools —
// see the OrgsStore interface comment for the pool-split rationale.
//
//   - admin: ListActiveSystem, GetSettingsSystem. Background services
//     iterating the active org set or reading per-org settings without
//     a JWT-claims context.
//   - app: GetSettings, UpdateSettings. Request-handler reads/writes
//     gated by the org_settings_select / org_settings_update RLS
//     policies (org membership / org admin).
type orgsStore struct {
	app   queryer
	admin queryer
}

func newOrgsStore(app, admin queryer) db.OrgsStore {
	return &orgsStore{app: app, admin: admin}
}

var _ db.OrgsStore = (*orgsStore)(nil)

func (s *orgsStore) GetOrg(ctx context.Context, orgID string) (*domain.Org, error) {
	return getOrg(ctx, s.app, orgID)
}

func (s *orgsStore) GetOrgSystem(ctx context.Context, orgID string) (*domain.Org, error) {
	return getOrg(ctx, s.admin, orgID)
}

// CreateLocalTenant is local-mode only — multi-mode provisions real
// tenant rows per signup in auth_provision.go, never the synthetic
// LocalDefault* sentinels. In Postgres (multi mode) this should never be called;
// return a clear error so incorrect callers fail loudly.
func (s *orgsStore) CreateLocalTenant(ctx context.Context) error {
	return fmt.Errorf("db: CreateLocalTenant is not supported in multi mode")
}

func getOrg(ctx context.Context, q queryer, orgID string) (*domain.Org, error) {
	var (
		o     domain.Org
		owner sql.NullString
	)
	err := q.QueryRowContext(ctx, `
		SELECT id::text, name, slug, owner_user_id::text, created_at
		  FROM orgs WHERE id = $1
	`, orgID).Scan(&o.ID, &o.Name, &o.Slug, &owner, &o.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read org: %w", err)
	}
	o.OwnerUserID = owner.String
	return &o, nil
}

func (s *orgsStore) ListActiveSystem(ctx context.Context) ([]string, error) {
	rows, err := s.admin.QueryContext(ctx, `
		SELECT id FROM orgs
		WHERE deleted_at IS NULL
		ORDER BY id ASC
	`)
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
	return getOrgSettings(ctx, s.app, orgID)
}

func (s *orgsStore) GetSettingsSystem(ctx context.Context, orgID string) (domain.OrgSettings, error) {
	return getOrgSettings(ctx, s.admin, orgID)
}

func getOrgSettings(ctx context.Context, q queryer, orgID string) (domain.OrgSettings, error) {
	var (
		ghURL, jiraURL, anthRef, bedRef, maxTier sql.NullString
		ghSecs, jiraSecs                         float64
		cloneProto                               string
		maxDailyCost                             sql.NullFloat64
		maxConcurrentRuns                        sql.NullInt64
		marketplaceEnabled                       bool
		credentialClass                          string
		version                                  int
	)
	// EXTRACT(EPOCH FROM interval) returns numeric in PG13+; the
	// ::double precision cast pins the row-out type so pgx can scan
	// straight into float64 without a string detour. Cleaner round-
	// trip than ::text + time.ParseDuration (which can't parse the
	// Postgres "HH:MM:SS" interval rendering anyway).
	err := q.QueryRowContext(ctx, `
		SELECT github_base_url,
		       EXTRACT(EPOCH FROM github_poll_interval)::double precision,
		       github_clone_protocol,
		       jira_base_url,
		       EXTRACT(EPOCH FROM jira_poll_interval)::double precision,
		       anthropic_api_key_ref, bedrock_credentials_ref, max_llm_model_tier,
		       max_daily_cost_usd, max_concurrent_runs, marketplace_enabled,
		       github_credential_class, version
		FROM org_settings WHERE org_id = $1
	`, orgID).Scan(
		&ghURL, &ghSecs, &cloneProto,
		&jiraURL, &jiraSecs,
		&anthRef, &bedRef, &maxTier,
		&maxDailyCost, &maxConcurrentRuns, &marketplaceEnabled,
		&credentialClass, &version,
	)
	if errors.Is(err, sql.ErrNoRows) {
		// Provisioning seeds org_settings rows at org-create time
		// (auth provisioning); this fallback covers the narrow window
		// before the first signup runs (or test fixtures that build a
		// DB without going through provisioning). Matches the schema
		// DEFAULT clauses.
		return domain.DefaultOrgSettings(), nil
	}
	if err != nil {
		return domain.OrgSettings{}, fmt.Errorf("read org_settings: %w", err)
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
		GitHubPollInterval:    secondsToDuration(ghSecs),
		GitHubCloneProtocol:   cloneProto,
		JiraBaseURL:           jiraURL.String,
		JiraPollInterval:      secondsToDuration(jiraSecs),
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
	_, err := s.upsertSettings(ctx, orgID, u, nil)
	if err != nil {
		return fmt.Errorf("upsert org_settings: %w", err)
	}
	return nil
}

// UpdateSettingsVersioned is UpdateSettings under the row's concurrency token.
// The guard rides the ON CONFLICT DO UPDATE's WHERE, so a losing writer updates
// zero rows rather than overwriting the winner; expected 0 (no row) resolves
// through the INSERT arm, where the unique index does the same job.
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
		nullString(u.GitHubBaseURL),
		u.GitHubPollInterval.Seconds(),
		cloneProto,
		nullString(u.JiraBaseURL),
		u.JiraPollInterval.Seconds(),
		nullString(u.AnthropicAPIKeyRef),
		nullString(u.BedrockCredentialsRef),
		nullString(u.MaxLLMModelTier),
		nullFloat(u.MaxDailyCostUSD),
		nullInt(u.MaxConcurrentRuns),
		u.MarketplaceEnabled,
	}
	guard := ""
	if expected != nil {
		guard = "WHERE org_settings.version = $13"
		args = append(args, *expected)
	}
	// make_interval(secs => $N) takes a numeric second count and
	// returns a properly-typed interval — avoids hand-rolling the
	// "X seconds"::interval string concat.
	return s.app.ExecContext(ctx, `
		INSERT INTO org_settings (
			org_id, github_base_url, github_poll_interval, github_clone_protocol,
			jira_base_url, jira_poll_interval,
			anthropic_api_key_ref, bedrock_credentials_ref, max_llm_model_tier,
			max_daily_cost_usd, max_concurrent_runs, marketplace_enabled,
			version, updated_at
		) VALUES (
			$1, $2, make_interval(secs => $3), $4,
			$5, make_interval(secs => $6),
			$7, $8, $9,
			$10, $11, $12,
			1, now()
		)
		ON CONFLICT (org_id) DO UPDATE SET
			github_base_url = EXCLUDED.github_base_url,
			github_poll_interval = EXCLUDED.github_poll_interval,
			github_clone_protocol = EXCLUDED.github_clone_protocol,
			jira_base_url = EXCLUDED.jira_base_url,
			jira_poll_interval = EXCLUDED.jira_poll_interval,
			anthropic_api_key_ref = EXCLUDED.anthropic_api_key_ref,
			bedrock_credentials_ref = EXCLUDED.bedrock_credentials_ref,
			max_llm_model_tier = EXCLUDED.max_llm_model_tier,
			max_daily_cost_usd = EXCLUDED.max_daily_cost_usd,
			max_concurrent_runs = EXCLUDED.max_concurrent_runs,
			marketplace_enabled = EXCLUDED.marketplace_enabled,
			version = org_settings.version + 1,
			updated_at = now()
		`+guard, args...)
}

// SetGitHubCredentialClass upserts ONLY org_settings.github_credential_class —
// which credential system the org's GitHub access belongs to. See the
// OrgsStore interface doc for why this is a separate writer from
// UpdateSettings.
//
// App pool, unlike the team-settings cap writer it otherwise mirrors: every
// caller is an org-admin-gated handler already running inside a claims-bound
// transaction alongside the credential write this class describes, which is
// exactly what org_settings_insert / org_settings_update ask for. Reaching for
// the admin pool here would take the write out of that transaction's RLS
// context for no reason.
//
// The partial INSERT relies on the schema DEFAULT clauses for every other
// org_settings column when no row exists yet, and ON CONFLICT touches only the
// class, so the org's other settings are never clobbered.
func (s *orgsStore) SetGitHubCredentialClass(ctx context.Context, orgID string, class domain.GitHubCredentialClass) error {
	if _, err := s.app.ExecContext(ctx, `
		INSERT INTO org_settings (org_id, github_credential_class, updated_at)
		VALUES ($1, $2, now())
		ON CONFLICT (org_id) DO UPDATE SET
			github_credential_class = EXCLUDED.github_credential_class,
			updated_at = now()
	`, orgID, string(class)); err != nil {
		return fmt.Errorf("set org github credential class: %w", err)
	}
	return nil
}

// secondsToDuration converts a Postgres EXTRACT(EPOCH FROM interval)
// reading (seconds, double precision) to time.Duration. The naive
// time.Duration(secs * float64(time.Second)) truncates the float-to-int
// conversion, drifting by up to a nanosecond per round-trip. Rounding
// to the nearest nanosecond pins the value at the precision Go's
// Duration actually represents — and stays exact for the
// minute-granularity poll intervals we round-trip in practice.
func secondsToDuration(secs float64) time.Duration {
	return time.Duration(math.Round(secs * float64(time.Second)))
}
