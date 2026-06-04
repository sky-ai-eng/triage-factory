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
// sentinel seeded by the v1.11.0 baseline migration, but the SQL
// makes no assumption about that count so a hypothetical future test
// fixture that inserts additional rows iterates them correctly.
//
// orgsStore — SQLite impl. The constructor accepts two queryers for
// signature parity with the Postgres impl (SKY-296); SQLite has one
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
	return db.SeedLocalTenantRows(ctx, s.q)
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
	)
	err := q.QueryRowContext(ctx, `
		SELECT github_base_url, github_poll_interval, github_clone_protocol,
		       jira_base_url, jira_poll_interval,
		       anthropic_api_key_ref, bedrock_credentials_ref, max_llm_model_tier
		FROM org_settings WHERE org_id = ?
	`, orgID).Scan(
		&ghURL, &ghInterval, &cloneProto,
		&jiraURL, &jiraInterval,
		&anthRef, &bedRef, &maxTier,
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
	return domain.OrgSettings{
		GitHubBaseURL:         ghURL.String,
		GitHubPollInterval:    ghDur,
		GitHubCloneProtocol:   cloneProto,
		JiraBaseURL:           jiraURL.String,
		JiraPollInterval:      jiraDur,
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
	_, err := s.q.ExecContext(ctx, `
		INSERT INTO org_settings (
			org_id, github_base_url, github_poll_interval, github_clone_protocol,
			jira_base_url, jira_poll_interval,
			anthropic_api_key_ref, bedrock_credentials_ref, max_llm_model_tier,
			updated_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, CURRENT_TIMESTAMP)
		ON CONFLICT(org_id) DO UPDATE SET
			github_base_url = excluded.github_base_url,
			github_poll_interval = excluded.github_poll_interval,
			github_clone_protocol = excluded.github_clone_protocol,
			jira_base_url = excluded.jira_base_url,
			jira_poll_interval = excluded.jira_poll_interval,
			anthropic_api_key_ref = excluded.anthropic_api_key_ref,
			bedrock_credentials_ref = excluded.bedrock_credentials_ref,
			max_llm_model_tier = excluded.max_llm_model_tier,
			updated_at = CURRENT_TIMESTAMP
	`,
		orgID,
		nullStringValue(u.GitHubBaseURL),
		u.GitHubPollInterval.String(),
		cloneProto,
		nullStringValue(u.JiraBaseURL),
		u.JiraPollInterval.String(),
		nullStringValue(u.AnthropicAPIKeyRef),
		nullStringValue(u.BedrockCredentialsRef),
		nullStringValue(u.MaxLLMModelTier),
	)
	if err != nil {
		return fmt.Errorf("upsert org_settings: %w", err)
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
