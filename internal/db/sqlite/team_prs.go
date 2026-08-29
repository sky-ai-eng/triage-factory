package sqlite

import (
	"context"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// teamPRStore is the SQLite impl of db.TeamPRStore — the team arm of the
// pull-request list.
//
// Local mode is N=1: one org, one team, and every repo the install polls is
// that team's, so the tracked-set outer filter the Postgres twin applies has
// nothing to narrow and is left out, the same posture as sqlite/factory.go and
// sqlite/team_activity.go. Both identity legs stay, because they are not about
// scoping: the member leg is a real join through the sole user's identity
// binding (an install whose operator has bound no GitHub login has no authored
// pull requests to claim), and the owning-team leg is what makes a pull
// request TF opened this team's without an author to resolve.
type teamPRStore struct{ q queryer }

func newTeamPRStore(q queryer) db.TeamPRStore { return &teamPRStore{q: q} }

var _ db.TeamPRStore = (*teamPRStore)(nil)

func (s *teamPRStore) TeamPRs(ctx context.Context, orgID, teamID, githubBaseURL string, f db.PRListFilter, opts db.ListOpts) ([]domain.PRSummaryRow, int, error) {
	if err := assertLocalOrg(orgID); err != nil {
		return nil, 0, err
	}
	stateClause, err := sqlitePRStateClause(f.States)
	if err != nil {
		return nil, 0, err
	}

	// The member leg is a correlated IN rather than a resolved login list:
	// one statement, and it reads as the join it is. Host resolution matches
	// the Postgres twin (EffectiveGitHubHost), so an unset github_base_url
	// looks under the public host where a login-claim binding actually lands
	// rather than under "".
	where := `
		WHERE e.source = 'github' AND ` + sqlitePRStateSnapshotGuard + `
		  AND (
		      e.owning_team_id = ?
		      OR json_extract(e.snapshot_json, '$.author') IN (
		          SELECT gh.login
		          FROM user_github_identities gh
		          JOIN memberships m ON m.user_id = gh.user_id
		          WHERE m.team_id = ? AND gh.github_base_url = ? AND gh.login != ''
		      )
		  )` + stateClause
	args := []any{teamID, teamID, db.EffectiveGitHubHost(githubBaseURL)}

	var total int
	if err := s.q.QueryRowContext(ctx, `SELECT COUNT(*) FROM entities e`+where, args...).Scan(&total); err != nil {
		return nil, 0, err
	}
	if opts.CountOnly {
		return []domain.PRSummaryRow{}, total, nil
	}

	query := `SELECT e.snapshot_json FROM entities e` + where + sqlitePRSummaryOrder
	if opts.Limit > 0 {
		query += ` LIMIT ? OFFSET ?`
		args = append(args, opts.Limit, opts.Offset)
	}
	rows, err := s.q.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, 0, err
	}
	prs, err := sqliteScanPRSummaries(rows)
	if err != nil {
		return nil, 0, err
	}
	return prs, total, nil
}
