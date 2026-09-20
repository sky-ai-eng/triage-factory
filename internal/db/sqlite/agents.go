package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// agentStore is the SQLite impl of db.AgentStore. The
// SQLite schema carries org_id with an FK to orgs(id), so every method
// filters by org_id structurally instead of by the BootstrapAgentID-
// derived row id. That matches the Postgres impl shape and removes
// the convention burden of assertLocalOrg-at-every-method-entry.
//
// The runtime assertLocalOrg call survives at startup as a defense-
// in-depth check (the sentinel rows must exist + match the runmode
// constants); per-method entries don't need it because the org_id
// column physically constrains writes/reads to the one synthetic org.
// agentStore — SQLite impl. The constructor accepts two queryers for
// signature parity with the Postgres impl; SQLite has one
// connection so both collapse to the same queryer. The
// `...System` variants delegate to their non-System counterparts.
type agentStore struct{ q queryer }

func newAgentStore(q, _ queryer) db.AgentStore { return &agentStore{q: q} }

var _ db.AgentStore = (*agentStore)(nil)

const sqliteAgentColumns = `id, display_name, default_model, default_autonomy_suitability,
       github_pat_user_id, github_org_login, github_org_email, jira_service_account_id,
       created_at, updated_at`

func (s *agentStore) GetForOrgSystem(ctx context.Context, orgID string) (*domain.Agent, error) {
	return s.GetForOrg(ctx, orgID)
}

func (s *agentStore) GetForOrg(ctx context.Context, orgID string) (*domain.Agent, error) {
	row := s.q.QueryRowContext(ctx, `
		SELECT `+sqliteAgentColumns+`
		FROM agents
		WHERE org_id = ?
	`, orgID)
	a, err := scanAgentRow(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &a, nil
}

func (s *agentStore) Create(ctx context.Context, orgID string, a domain.Agent) (string, error) {
	now := time.Now().UTC()
	// The row id is implementation-defined — always db.BootstrapAgentID(orgID).
	// Caller-supplied a.ID is ignored; see agents_store.go for the rationale.
	id := db.BootstrapAgentID(orgID)
	displayName := a.DisplayName
	if displayName == "" {
		displayName = "Triage Factory Bot"
	}
	// In local mode the agent borrows the lone user's PAT (the users
	// sentinel row exists, so the FK is satisfied). The default-on-insert
	// shape keeps "the local bot has the local user's identity" true from
	// the moment the row appears.
	patUser := a.GitHubPATUserID
	if patUser == "" && orgID == runmode.LocalDefaultOrgID {
		patUser = runmode.LocalDefaultUserID
	}
	// INSERT OR IGNORE handles the idempotency case where the row
	// already exists (UNIQUE(org_id) enforces "one per org"); the
	// follow-up SELECT returns the established id either way.
	_, err := s.q.ExecContext(ctx, `
		INSERT OR IGNORE INTO agents
			(id, org_id, display_name, default_model, default_autonomy_suitability,
			 github_pat_user_id, jira_service_account_id,
			 created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
	`, id, orgID, displayName, nullString(a.DefaultModel), a.DefaultAutonomySuitability,
		nullString(patUser), nullString(a.JiraServiceAccountID),
		now, now)
	if err != nil {
		return "", err
	}
	// Look up the established id — handles both fresh-insert and
	// existing-row paths uniformly.
	var existing string
	if err := s.q.QueryRowContext(ctx,
		`SELECT id FROM agents WHERE org_id = ?`, orgID,
	).Scan(&existing); err != nil {
		return "", err
	}
	return existing, nil
}

// scanUpdatedAgent decodes an id-keyed UPDATE … RETURNING. No row scanned
// means the (org, id) pair named nothing, which is db.ErrNoSuchAgent.
func scanUpdatedAgent(row *sql.Row) (domain.Agent, error) {
	a, err := scanAgentRow(row)
	if errors.Is(err, sql.ErrNoRows) {
		return domain.Agent{}, db.ErrNoSuchAgent
	}
	return a, err
}

func (s *agentStore) SetGitHubOrgIdentity(ctx context.Context, orgID, agentID, login, email string) (domain.Agent, error) {
	// login is a free-form GitHub login (e.g. "octocat" or "acme-bot[bot]"),
	// not a UUID — no shape validation. The pair is all-or-nothing: either
	// empty input clears both columns so no caller can persist a partial commit
	// identity.
	if login == "" || email == "" {
		login, email = "", ""
	}
	return scanUpdatedAgent(s.q.QueryRowContext(ctx, `
		UPDATE agents
		SET github_org_login = ?,
		    github_org_email = ?,
		    updated_at = ?
		WHERE org_id = ? AND id = ?
		RETURNING `+sqliteAgentColumns,
		nullString(login), nullString(email), time.Now().UTC(), orgID, agentID))
}

// nullString returns NULL when s is empty so the column scans back as
// NULL on the next Get rather than as an empty TEXT.
func nullString(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func scanAgentRow(row *sql.Row) (domain.Agent, error) {
	var a domain.Agent
	var defaultModel, ghPATUser, ghOrgLogin, ghOrgEmail, jiraSvc sql.NullString
	var defAutonomy sql.NullFloat64
	if err := row.Scan(&a.ID, &a.DisplayName, &defaultModel, &defAutonomy,
		&ghPATUser, &ghOrgLogin, &ghOrgEmail, &jiraSvc, &a.CreatedAt, &a.UpdatedAt); err != nil {
		return a, err
	}
	a.DefaultModel = defaultModel.String
	if defAutonomy.Valid {
		v := defAutonomy.Float64
		a.DefaultAutonomySuitability = &v
	}
	a.GitHubPATUserID = ghPATUser.String
	a.GitHubOrgLogin = ghOrgLogin.String
	a.GitHubOrgEmail = ghOrgEmail.String
	a.JiraServiceAccountID = jiraSvc.String
	return a, nil
}
