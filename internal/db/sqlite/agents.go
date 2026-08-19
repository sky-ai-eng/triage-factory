package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"

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
	// sentinel row exists, so the FK is satisfied). Caller
	// can override via SetGitHubPATUser later; the default-on-insert
	// shape just keeps "the local bot has the local user's identity"
	// true from the moment the row appears.
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

func (s *agentStore) Update(ctx context.Context, orgID string, a domain.Agent) error {
	_, err := s.q.ExecContext(ctx, `
		UPDATE agents
		SET display_name = ?,
		    default_model = ?,
		    default_autonomy_suitability = ?,
		    jira_service_account_id = ?,
		    updated_at = ?
		WHERE org_id = ? AND id = ?
	`, a.DisplayName, nullString(a.DefaultModel), a.DefaultAutonomySuitability,
		nullString(a.JiraServiceAccountID), time.Now().UTC(), orgID, a.ID)
	return err
}

func (s *agentStore) SetGitHubPATUser(ctx context.Context, orgID, agentID, userID string) error {
	// Match the Postgres impl's input contract: empty = intentional
	// clear, valid UUID = intentional set, anything else is a caller
	// bug. SQLite's github_pat_user_id has an FK to users(id) post-269
	// so a non-UUID would also fail at the column layer, but
	// rejecting up front keeps the error shape friendly + matches
	// Postgres exactly.
	if userID != "" && !isValidUUIDLike(userID) {
		return fmt.Errorf("sqlite agents: SetGitHubPATUser: userID %q is not empty and not a valid UUID", userID)
	}
	_, err := s.q.ExecContext(ctx, `
		UPDATE agents
		SET github_pat_user_id = ?,
		    updated_at = ?
		WHERE org_id = ? AND id = ?
	`, nullString(userID), time.Now().UTC(), orgID, agentID)
	return err
}

func (s *agentStore) SetGitHubOrgIdentity(ctx context.Context, orgID, agentID, login, email string) error {
	// login is a free-form GitHub login (e.g. "octocat" or "acme-bot[bot]"),
	// not a UUID — no shape validation. The pair is all-or-nothing: either
	// empty input clears both columns so no caller can persist a partial commit
	// identity.
	if login == "" || email == "" {
		login, email = "", ""
	}
	_, err := s.q.ExecContext(ctx, `
		UPDATE agents
		SET github_org_login = ?,
		    github_org_email = ?,
		    updated_at = ?
		WHERE org_id = ? AND id = ?
	`, nullString(login), nullString(email), time.Now().UTC(), orgID, agentID)
	return err
}

// isValidUUIDLike is a thin local mirror of postgres/uuid.go:isValidUUID.
// Duplicated rather than reaching across packages because the SQLite
// store has no other reason to import postgres-internal helpers; it's
// four lines of code and the shape is unlikely to drift.
func isValidUUIDLike(s string) bool {
	_, err := uuid.Parse(s)
	return err == nil
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
