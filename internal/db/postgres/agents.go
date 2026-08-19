package postgres

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// agentStore is the Postgres impl of db.AgentStore.
//
// # Pool split
//
//   - app   — tf_app, RLS-active. GetForOrg, Update, SetGitHubPATUser.
//     The agents_select policy gates SELECTs by org access;
//     agents_update/insert/delete gate by org admin.
//   - admin — supabase_admin, BYPASSRLS. Create only. Justified
//     because the only legitimate caller is bootstrap (org-create
//     handler, internal/db/bootstrap.go), and at that moment the
//     founder's org_memberships row is being inserted in the same
//     transaction as the agents row — at the agents-insert moment,
//     tf.user_is_org_admin() returns false and the agents_insert
//     policy would refuse. Same pool-split pattern as PromptStore
//     and EventHandlerStore.
//
// Inside WithTx both fields point at the same *sql.Tx, and inTx is
// true. Create inside WithTx is rejected — escaping to the admin pool
// from inside a caller's tx breaks their tx scope; production bootstrap
// runs outside any user-initiated tx.
type agentStore struct {
	app   queryer
	admin queryer
	inTx  bool
}

func newAgentStore(app, admin queryer) db.AgentStore {
	return &agentStore{app: app, admin: admin}
}

// newTxAgentStore composes a tx-bound AgentStore for WithTx /
// NewForTx. Both pools collapse onto the caller's tx; inTx=true makes
// Create refuse rather than silently bypass the tx scope.
func newTxAgentStore(tx queryer) db.AgentStore {
	return &agentStore{app: tx, admin: tx, inTx: true}
}

var _ db.AgentStore = (*agentStore)(nil)

const pgAgentColumns = `id, display_name, default_model, default_autonomy_suitability,
       github_pat_user_id, github_org_login, github_org_email, jira_service_account_id,
       created_at, updated_at`

func (s *agentStore) GetForOrg(ctx context.Context, orgID string) (*domain.Agent, error) {
	a, err := getAgentForOrg(ctx, s.app, orgID)
	return a, wrapAppPoolPermErr(err, "agents.GetForOrg")
}

func (s *agentStore) GetForOrgSystem(ctx context.Context, orgID string) (*domain.Agent, error) {
	return getAgentForOrg(ctx, s.admin, orgID)
}

func getAgentForOrg(ctx context.Context, q queryer, orgID string) (*domain.Agent, error) {
	if !isValidUUID(orgID) {
		return nil, nil
	}
	row := q.QueryRowContext(ctx, `
		SELECT `+pgAgentColumns+`
		FROM agents
		WHERE org_id = $1
	`, orgID)
	a, err := scanAgentRowPG(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &a, nil
}

func (s *agentStore) Create(ctx context.Context, orgID string, a domain.Agent) (string, error) {
	if s.inTx {
		// Same justification as EventHandlerStore.Seed:
		// admin-pool escape from inside a caller's tx silently breaks
		// the caller's transaction scope. Production bootstrap runs
		// outside any user tx (the org-create handler will open its
		// own tx for tenancy rows; the agent insert is a separate
		// admin-pool call).
		return "", errors.New("postgres agents: Create must not be called inside WithTx; call stores.Agents.Create directly")
	}
	displayName := a.DisplayName
	if displayName == "" {
		displayName = "Triage Factory Bot"
	}
	// The row id is implementation-defined — always db.BootstrapAgentID(orgID).
	// Caller-supplied a.ID is ignored. UNIQUE(org_id) here would catch
	// a duplicate insert either way, but on conflict the caller's custom
	// id would be silently dropped (we'd return the existing row's id
	// instead), and the resulting "the id you asked for isn't the id you
	// got" inconsistency is more surprising than just ignoring the field
	// to start with. Matches the SQLite impl's stricter same-contract
	// shape (where there's no UNIQUE constraint to catch the duplicate
	// at all).
	id := db.BootstrapAgentID(orgID)
	// ON CONFLICT (org_id) DO NOTHING; the RETURNING clause is then
	// empty on conflict, so we follow up with a SELECT to fetch the
	// existing id when no row was inserted.
	var insertedID string
	err := s.admin.QueryRowContext(ctx, `
		INSERT INTO agents
			(id, org_id, display_name, default_model, default_autonomy_suitability,
			 github_pat_user_id, jira_service_account_id)
		VALUES ($1, $2, $3, $4, $5, $6, $7)
		ON CONFLICT (org_id) DO NOTHING
		RETURNING id
	`, id, orgID, displayName, nullString(a.DefaultModel), a.DefaultAutonomySuitability,
		nullUUID(a.GitHubPATUserID), nullString(a.JiraServiceAccountID),
	).Scan(&insertedID)
	if err == nil {
		return insertedID, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return "", err
	}
	// Conflict path — look up the existing row's id via the UNIQUE
	// constraint we just collided on.
	var existing string
	if err := s.admin.QueryRowContext(ctx,
		`SELECT id FROM agents WHERE org_id = $1`, orgID,
	).Scan(&existing); err != nil {
		return "", err
	}
	return existing, nil
}

func (s *agentStore) Update(ctx context.Context, orgID string, a domain.Agent) error {
	if !isValidUUID(a.ID) {
		return nil
	}
	_, err := s.app.ExecContext(ctx, `
		UPDATE agents
		SET display_name = $1,
		    default_model = $2,
		    default_autonomy_suitability = $3,
		    jira_service_account_id = $4
		WHERE org_id = $5 AND id = $6
	`, a.DisplayName, nullString(a.DefaultModel), a.DefaultAutonomySuitability,
		nullString(a.JiraServiceAccountID), orgID, a.ID)
	return err
}

func (s *agentStore) SetGitHubPATUser(ctx context.Context, orgID, agentID, userID string) error {
	if !isValidUUID(agentID) {
		return nil
	}
	// "" = caller-intentional clear, valid UUID = caller-intentional set.
	// Any other shape (e.g. "alice@example.com" passed by mistake) is a
	// programmer bug we refuse loudly rather than silently treating as
	// clear — the underlying github_pat_user_id column is a UUID FK and
	// the cast would 22P02-error anyway, but refusing up front gives a
	// caller-friendly error shape.
	if userID != "" && !isValidUUID(userID) {
		return fmt.Errorf("postgres agents: SetGitHubPATUser: userID %q is not empty and not a valid UUID", userID)
	}
	_, err := s.app.ExecContext(ctx, `
		UPDATE agents
		SET github_pat_user_id = $1
		WHERE org_id = $2 AND id = $3
	`, nullUUID(userID), orgID, agentID)
	return err
}

func (s *agentStore) SetGitHubOrgIdentity(ctx context.Context, orgID, agentID, login, email string) error {
	if !isValidUUID(agentID) {
		return nil
	}
	// login is a free-form GitHub login (e.g. "octocat" or "acme-bot[bot]"),
	// stored in the text column github_org_login — no UUID shape check, unlike
	// SetGitHubPATUser's user FK. The pair is all-or-nothing: either empty input
	// clears both columns so no caller can persist a partial commit identity.
	if login == "" || email == "" {
		login, email = "", ""
	}
	_, err := s.app.ExecContext(ctx, `
		UPDATE agents
		SET github_org_login = $1,
		    github_org_email = $2
		WHERE org_id = $3 AND id = $4
	`, nullString(login), nullString(email), orgID, agentID)
	return err
}

// nullString returns nil when s is empty so the column ends up SQL NULL.
func nullString(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// nullFloat returns nil when f is 0 so the column ends up SQL NULL — the
// numeric analog of nullString. Used for nullable cost/limit columns whose
// Go zero value (0) means "unset" (e.g. org_settings.max_daily_cost_usd, where
// 0 / NULL both mean "no cap").
func nullFloat(f float64) any {
	if f == 0 {
		return nil
	}
	return f
}

// nullInt returns nil when n is 0 so the column ends up SQL NULL — the integer
// analog of nullFloat. Used for nullable ceiling columns whose Go zero value
// (0) means "unset" (e.g. org_settings.max_concurrent_runs, where 0 / NULL both
// mean "unlimited").
func nullInt(n int) any {
	if n == 0 {
		return nil
	}
	return n
}

// nullUUID returns nil for empty / non-UUID input, otherwise the string
// for direct pgx UUID binding. Distinguished from nullString because
// passing an empty string into a UUID column would 22P02-error.
func nullUUID(s string) any {
	if s == "" || !isValidUUID(s) {
		return nil
	}
	return s
}

func scanAgentRowPG(row *sql.Row) (domain.Agent, error) {
	var a domain.Agent
	var defaultModel, jiraSvc sql.NullString
	var ghPATUser, ghOrgLogin, ghOrgEmail sql.NullString
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
