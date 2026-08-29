package postgres

import (
	"context"
	"fmt"
	"strings"

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
// The member leg's logins cannot be: user_github_identities_select is
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
	logins, err := s.memberLogins(ctx, orgID, teamID, githubBaseURL)
	if err != nil {
		return nil, 0, err
	}

	// $1 org, $2 team, then one placeholder per bound member login. Logins
	// bind individually rather than as an array literal: they are values
	// GitHub chose, and a per-team roster is small enough that an IN list
	// costs nothing to build.
	args := []any{orgID, teamID}
	memberLeg := ""
	if len(logins) > 0 {
		ph := make([]string, len(logins))
		for i, login := range logins {
			args = append(args, login)
			ph[i] = fmt.Sprintf("$%d", len(args))
		}
		memberLeg = fmt.Sprintf(" OR e.snapshot_json ->> 'author' IN (%s)", strings.Join(ph, ", "))
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

// memberLogins resolves the GitHub logins the subject team's members have
// bound on the org's host. Admin pool (see the type doc), scoped by the
// memberships join to this team and by the teams join to this org — with RLS
// bypassed the WHERE clause is the entire boundary, so both stay in it.
//
// A member who has bound nothing on this host contributes no login, which is
// the accepted caveat: their pull requests join the team's list when they
// bind, and /api/me is where that state is surfaced.
func (s *teamPRStore) memberLogins(ctx context.Context, orgID, teamID, githubBaseURL string) ([]string, error) {
	rows, err := s.admin.QueryContext(ctx, `
		SELECT gh.login
		FROM user_github_identities gh
		JOIN memberships m ON m.user_id = gh.user_id
		JOIN teams t ON t.id = m.team_id
		WHERE m.team_id = $1 AND t.org_id = $2 AND gh.github_base_url = $3 AND gh.login <> ''
	`, teamID, orgID, db.EffectiveGitHubHost(githubBaseURL))
	if err != nil {
		return nil, fmt.Errorf("list team member github logins: %w", err)
	}
	defer rows.Close()
	var logins []string
	for rows.Next() {
		var login string
		if err := rows.Scan(&login); err != nil {
			return nil, fmt.Errorf("scan team member github login: %w", err)
		}
		logins = append(logins, login)
	}
	return logins, rows.Err()
}
