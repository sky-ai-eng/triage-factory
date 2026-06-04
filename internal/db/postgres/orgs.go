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
}

func getOrg(ctx context.Context, q queryer, orgID string) (*domain.Org, error) {
	var (
		o          domain.Org
		owner      sql.NullString
		isPersonal sql.NullBool
	)
	err := q.QueryRowContext(ctx, `
		SELECT id::text, name, slug, owner_user_id::text, is_personal, created_at
		  FROM orgs WHERE id = $1
	`, orgID).Scan(&o.ID, &o.Name, &o.Slug, &owner, &isPersonal, &o.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read org: %w", err)
	}
	o.OwnerUserID = owner.String
	o.IsPersonal = isPersonal.Valid && isPersonal.Bool
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
		       anthropic_api_key_ref, bedrock_credentials_ref, max_llm_model_tier
		FROM org_settings WHERE org_id = $1
	`, orgID).Scan(
		&ghURL, &ghSecs, &cloneProto,
		&jiraURL, &jiraSecs,
		&anthRef, &bedRef, &maxTier,
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
	return domain.OrgSettings{
		GitHubBaseURL:         ghURL.String,
		GitHubPollInterval:    secondsToDuration(ghSecs),
		GitHubCloneProtocol:   cloneProto,
		JiraBaseURL:           jiraURL.String,
		JiraPollInterval:      secondsToDuration(jiraSecs),
		AnthropicAPIKeyRef:    anthRef.String,
		BedrockCredentialsRef: bedRef.String,
		MaxLLMModelTier:       maxTier.String,
	}, nil
}

func (s *orgsStore) UpdateSettings(ctx context.Context, orgID string, u domain.OrgSettings) error {
	cloneProto := u.GitHubCloneProtocol
	if cloneProto == "" {
		cloneProto = "ssh"
	}
	// make_interval(secs => $N) takes a numeric second count and
	// returns a properly-typed interval — avoids hand-rolling the
	// "X seconds"::interval string concat.
	_, err := s.app.ExecContext(ctx, `
		INSERT INTO org_settings (
			org_id, github_base_url, github_poll_interval, github_clone_protocol,
			jira_base_url, jira_poll_interval,
			anthropic_api_key_ref, bedrock_credentials_ref, max_llm_model_tier,
			updated_at
		) VALUES (
			$1, $2, make_interval(secs => $3), $4,
			$5, make_interval(secs => $6),
			$7, $8, $9,
			now()
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
			updated_at = now()
	`,
		orgID,
		nullString(u.GitHubBaseURL),
		u.GitHubPollInterval.Seconds(),
		cloneProto,
		nullString(u.JiraBaseURL),
		u.JiraPollInterval.Seconds(),
		nullString(u.AnthropicAPIKeyRef),
		nullString(u.BedrockCredentialsRef),
		nullString(u.MaxLLMModelTier),
	)
	if err != nil {
		return fmt.Errorf("upsert org_settings: %w", err)
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
