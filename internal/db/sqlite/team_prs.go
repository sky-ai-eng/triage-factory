package sqlite

import (
	"context"
	"fmt"
	"strings"

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

	// The member leg is correlated INs rather than a resolved account list:
	// one statement, and it reads as the join it is. A member is matched as
	// sqliteGitHubAuthoredBy matches an account: by GitHub's id when both the
	// snapshot and the binding record one, by login otherwise. Host
	// resolution matches the Postgres twin (EffectiveGitHubHost); an unset
	// github_base_url is no host, so no member matches.
	const members = `
		          FROM user_github_identities gh
		          JOIN memberships m ON m.user_id = gh.user_id
		          WHERE m.team_id = ? AND gh.github_base_url = ? AND gh.login != ''`
	where := `
		WHERE e.source = 'github' AND ` + sqlitePRStateSnapshotGuard + `
		  AND (
		      e.owning_team_id = ?
		      OR json_extract(e.snapshot_json, '$.author_id') IN (
		          SELECT gh.github_user_id` + members + ` AND gh.github_user_id IS NOT NULL
		      )
		      OR json_extract(e.snapshot_json, '$.author') IN (
		          SELECT gh.login` + members + `
		            AND (gh.github_user_id IS NULL OR json_extract(e.snapshot_json, '$.author_id') IS NULL)
		      )
		  )` + stateClause
	host := db.EffectiveGitHubHost(githubBaseURL)
	args := []any{teamID, teamID, host, teamID, host}

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

// sqliteGitHubAuthoredBy is the predicate "the pull request entity e was
// authored by one of accounts", appending its parameters to args — the twin of
// the Postgres pgGitHubAuthoredBy, with the same three arms: the author's
// GitHub id, the login when the snapshot records no id, and the login of an
// account whose binding recorded none. idx_entities_github_author_id and
// idx_entities_github_author serve the arms.
func sqliteGitHubAuthoredBy(args []any, accounts []domain.GitHubAccount) (string, []any) {
	var arms []string
	in := func(expr string, vals []string) {
		if len(vals) == 0 {
			return
		}
		ph := make([]string, len(vals))
		for i, v := range vals {
			ph[i] = "?"
			args = append(args, v)
		}
		arms = append(arms, fmt.Sprintf(expr, strings.Join(ph, ", ")))
	}
	var ids, loginsWithID, loginsWithoutID []string
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
	in(`json_extract(e.snapshot_json, '$.author_id') IN (%s)`, ids)
	in(`(json_extract(e.snapshot_json, '$.author_id') IS NULL AND json_extract(e.snapshot_json, '$.author') IN (%s))`, loginsWithID)
	in(`json_extract(e.snapshot_json, '$.author') IN (%s)`, loginsWithoutID)
	if len(arms) == 0 {
		return "0", args
	}
	return "(" + strings.Join(arms, " OR ") + ")", args
}
