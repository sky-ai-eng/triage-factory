package postgres

import (
	"context"
	"fmt"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// teamPRStore is the Postgres impl of db.TeamPRStore — the team arm of the
// pull-request list.
//
// It holds both pools for the same reason the team roster does. The page
// itself runs on the app pool, where the tracked-set semi-join is bounded by
// team_github_repos RLS (a caller who is not on the team matches no tracked
// repo and reads an empty list, which is the whole point of running it there).
// The member leg's accounts cannot be: user_github_identities_select is
// self-only, so under RLS a member could only ever see their own binding and
// "the team's pull requests" would collapse to "mine". That lookup runs on the
// admin pool narrowed by a memberships join to exactly the subject team — the
// same shape TeamsStore.ListMembers uses to enrich a roster with peers'
// bindings.
type teamPRStore struct {
	q     queryer
	admin queryer
}

func newTeamPRStore(q, admin queryer) db.TeamPRStore { return &teamPRStore{q: q, admin: admin} }

var _ db.TeamPRStore = (*teamPRStore)(nil)

func (s *teamPRStore) TeamPRs(ctx context.Context, orgID, teamID, githubBaseURL string, f db.PRListFilter, opts db.ListOpts) ([]domain.PRSummaryRow, int, error) {
	stateClause, err := pgPRStateClause(f.States)
	if err != nil {
		return nil, 0, err
	}
	accounts, err := s.memberAccounts(ctx, orgID, teamID, githubBaseURL)
	if err != nil {
		return nil, 0, err
	}

	// $1 org, $2 team, then the member accounts' ids and logins
	// (pgGitHubAuthoredBy).
	args := []any{orgID, teamID}
	memberLeg := ""
	if len(accounts) > 0 {
		var leg string
		leg, args = pgGitHubAuthoredBy(args, accounts)
		memberLeg = " OR " + leg
	}

	// The tracked set is the outer filter; owner-or-member is the inner OR.
	// A team that tracks a repo sees the pull requests its people opened
	// there and the ones TF opened on its behalf — and nothing from a repo
	// nobody attached to it, however familiar the author.
	where := `
		WHERE e.org_id = $1 AND e.source = 'github' AND e.snapshot_json IS NOT NULL
		  AND ` + factoryGitHubRepoTrackedForTeams("$2") + `
		  AND (e.owning_team_id = $2` + memberLeg + `)` + stateClause

	var total int
	if err := s.q.QueryRowContext(ctx, `SELECT COUNT(*) FROM entities e`+where, args...).Scan(&total); err != nil {
		return nil, 0, err
	}
	if opts.CountOnly {
		return []domain.PRSummaryRow{}, total, nil
	}

	query := `SELECT e.snapshot_json FROM entities e` + where + pgPRSummaryOrder
	if opts.Limit > 0 {
		query += fmt.Sprintf(` LIMIT $%d OFFSET $%d`, len(args)+1, len(args)+2)
		args = append(args, opts.Limit, opts.Offset)
	}
	rows, err := s.q.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, 0, err
	}
	prs, err := pgScanPRSummaries(rows)
	if err != nil {
		return nil, 0, err
	}
	return prs, total, nil
}

// memberAccounts resolves the GitHub accounts (login, and GitHub's id when the
// binding recorded one) the subject team's members have bound on the org's
// host. Admin pool (see the type doc), scoped by the memberships join to this
// team and by the teams join to this org — with RLS bypassed the WHERE clause
// is the entire boundary, so both stay in it.
//
// A member who has bound nothing on this host contributes no account, which
// is the accepted caveat: their pull requests join the team's list when they
// bind, and /api/me is where that state is surfaced.
func (s *teamPRStore) memberAccounts(ctx context.Context, orgID, teamID, githubBaseURL string) ([]domain.GitHubAccount, error) {
	rows, err := s.admin.QueryContext(ctx, `
		SELECT gh.login, COALESCE(gh.github_user_id, '')
		FROM user_github_identities gh
		JOIN memberships m ON m.user_id = gh.user_id
		JOIN teams t ON t.id = m.team_id
		WHERE m.team_id = $1 AND t.org_id = $2 AND gh.github_base_url = $3 AND gh.login <> ''
	`, teamID, orgID, db.EffectiveGitHubHost(githubBaseURL))
	if err != nil {
		return nil, fmt.Errorf("list team member github accounts: %w", err)
	}
	defer rows.Close()
	var accounts []domain.GitHubAccount
	for rows.Next() {
		var a domain.GitHubAccount
		if err := rows.Scan(&a.Login, &a.ID); err != nil {
			return nil, fmt.Errorf("scan team member github account: %w", err)
		}
		accounts = append(accounts, a)
	}
	return accounts, rows.Err()
}

// pgGitHubAuthoredBy is the predicate "the pull request entity e was authored
// by one of accounts", appending its parameters to args. A pull request whose
// snapshot records the author's GitHub id matches an account with that id; a
// snapshot stored before ids were recorded matches by login; and an account
// whose binding recorded no id matches by login whatever the snapshot holds.
// So a renamed author still matches, and a login GitHub has handed to another
// account does not. The arms are served by idx_entities_github_author_id and
// idx_entities_github_author. Logins compare exactly, as the snapshot and the
// binding both spell them as GitHub returned them.
func pgGitHubAuthoredBy(args []any, accounts []domain.GitHubAccount) (string, []any) {
	ids, loginsWithID, loginsWithoutID := []string{}, []string{}, []string{}
	for _, a := range accounts {
		switch {
		case a.ID != "":
			ids = append(ids, a.ID)
			if a.Login != "" {
				loginsWithID = append(loginsWithID, a.Login)
			}
		case a.Login != "":
			loginsWithoutID = append(loginsWithoutID, a.Login)
		}
	}
	args = append(args, ids, loginsWithID, loginsWithoutID)
	n := len(args)
	return fmt.Sprintf(`(e.snapshot_json ->> 'author_id' = ANY($%d::text[])
		OR (e.snapshot_json ->> 'author_id' IS NULL AND e.snapshot_json ->> 'author' = ANY($%d::text[]))
		OR e.snapshot_json ->> 'author' = ANY($%d::text[]))`, n-2, n-1, n), args
}
