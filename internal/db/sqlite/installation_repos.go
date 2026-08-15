package sqlite

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// installationReposStore is the SQLite (local-mode) impl of
// db.InstallationReposStore. One connection, no RLS, one org — so the
// admin/app split the Postgres impl draws collapses, and every method
// re-asserts the local org sentinel the way the rest of this package does.
type installationReposStore struct{ q queryer }

func newInstallationReposStore(q queryer) db.InstallationReposStore {
	return &installationReposStore{q: q}
}

var _ db.InstallationReposStore = (*installationReposStore)(nil)

// ReplaceForInstallationSystem swaps one installation's mirror for repos inside
// a single transaction: delete the installation's rows, insert the new answer.
//
// Delete-then-insert rather than a mark-and-sweep is the shape the data wants —
// the mirror is a cache of an external fact with no durable identity, so there
// is nothing on a row worth preserving across passes and no epoch column whose
// tie-breaking could go wrong. Both halves are in one transaction, so a reader
// never observes the intermediate empty state and a failure mid-insert rolls
// back to the previous answer rather than to nothing.
//
// The insert conflicts on the case-folded identity index, which is what makes
// two casings of one repository in GitHub's own answer collapse to one row
// instead of raising.
func (s *installationReposStore) ReplaceForInstallationSystem(ctx context.Context, orgID, installationID string, repos []domain.InstallationRepository) error {
	if err := assertLocalOrg(orgID); err != nil {
		return err
	}
	if installationID == "" {
		return fmt.Errorf("replace installation repositories: empty installation id")
	}
	// Normalize before opening the transaction: a bad source is a caller bug,
	// and discovering it halfway through the replace would leave the work to a
	// rollback that only exists to undo a mistake we could refuse outright.
	observed := time.Now().UTC()
	rows := make([]domain.InstallationRepository, 0, len(repos))
	for _, r := range repos {
		source, err := domain.NormalizeRepoSource(r.Source)
		if err != nil {
			return fmt.Errorf("replace installation repositories: %w", err)
		}
		r.Source = source
		r.ObservedAt = observed
		rows = append(rows, r)
	}

	return inTx(ctx, s.q, func(tx queryer) error {
		if _, err := tx.ExecContext(ctx, `
			DELETE FROM installation_repositories
			 WHERE org_id = ? AND installation_id = ?
		`, orgID, installationID); err != nil {
			return fmt.Errorf("clear installation repositories: %w", err)
		}
		for _, r := range rows {
			if _, err := tx.ExecContext(ctx, `
				INSERT INTO installation_repositories
					(org_id, installation_id, source, owner, repo, external_id, observed_at)
				VALUES (?, ?, ?, ?, ?, ?, ?)
				ON CONFLICT DO NOTHING
			`, orgID, installationID, r.Source, r.Owner, r.Repo, nullStringValue(r.ExternalID), r.ObservedAt); err != nil {
				return fmt.Errorf("insert installation repository %s: %w", r.Slug(), err)
			}
		}
		return nil
	})
}

func (s *installationReposStore) ClearForInstallationSystem(ctx context.Context, orgID, installationID string) error {
	if err := assertLocalOrg(orgID); err != nil {
		return err
	}
	if _, err := s.q.ExecContext(ctx, `
		DELETE FROM installation_repositories
		 WHERE org_id = ? AND installation_id = ?
	`, orgID, installationID); err != nil {
		return fmt.Errorf("clear installation repositories: %w", err)
	}
	return nil
}

func (s *installationReposStore) ListForOrgSystem(ctx context.Context, orgID string) ([]domain.InstallationRepository, error) {
	if err := assertLocalOrg(orgID); err != nil {
		return nil, err
	}
	return scanInstallationRepos(s.q.QueryContext(ctx, `
		SELECT r.org_id, r.installation_id, r.source, r.owner, r.repo, r.external_id, r.observed_at
		  FROM installation_repositories r
		  JOIN org_github_app_installations i
		    ON i.org_id = r.org_id AND i.installation_id = r.installation_id
		 WHERE r.org_id = ? AND i.removed_at IS NULL
		 ORDER BY r.installation_id, LOWER(r.owner), LOWER(r.repo)
	`, orgID))
}

// ListReachWithoutPurposeSystem: granted, tracked by nobody. The NOT EXISTS is
// case-folded on both sides because GitHub identifiers are case-insensitive and
// a case-sensitive comparison here would report a tracked repository as
// untracked purely because someone capitalized the slug differently — a
// fabricated security finding, which is worse than a missed one.
func (s *installationReposStore) ListReachWithoutPurposeSystem(ctx context.Context, orgID string) ([]domain.InstallationRepository, error) {
	if err := assertLocalOrg(orgID); err != nil {
		return nil, err
	}
	return scanInstallationRepos(s.q.QueryContext(ctx, `
		SELECT r.org_id, r.installation_id, r.source, r.owner, r.repo, r.external_id, r.observed_at
		  FROM installation_repositories r
		  JOIN org_github_app_installations i
		    ON i.org_id = r.org_id AND i.installation_id = r.installation_id
		 WHERE r.org_id = ?
		   AND i.removed_at IS NULL
		   AND NOT EXISTS (
		       SELECT 1 FROM team_github_repos g
		        WHERE LOWER(g.owner) = LOWER(r.owner)
		          AND LOWER(g.repo) = LOWER(r.repo))
		 ORDER BY r.installation_id, LOWER(r.owner), LOWER(r.repo)
	`, orgID))
}

// ListScopeDriftSystem: tracked, granted by nobody.
//
// The EXISTS gate is the whole difference between a finding and a fabrication.
// An org with no App, or one whose first reconcile has not landed, has an empty
// mirror — and "not in the mirror" then describes every repository it tracks.
// Reporting those as drift would turn "we have not looked yet" into "your
// tracking is broken", so with no mirror at all the answer is empty.
func (s *installationReposStore) ListScopeDriftSystem(ctx context.Context, orgID string) ([]domain.TeamGitHubRepo, error) {
	if err := assertLocalOrg(orgID); err != nil {
		return nil, err
	}
	// Grouped on the folded slug rather than DISTINCT on the raw pair: two teams
	// tracking one repository under different casings are tracking one
	// repository, and listing it twice would double-count the finding. MIN picks
	// a stable spelling from the group, whose members differ only in case.
	rows, err := s.q.QueryContext(ctx, `
		SELECT MIN(g.owner), MIN(g.repo)
		  FROM team_github_repos g
		 WHERE EXISTS (
		       SELECT 1
		         FROM installation_repositories m
		         JOIN org_github_app_installations i
		           ON i.org_id = m.org_id AND i.installation_id = m.installation_id
		        WHERE m.org_id = ? AND i.removed_at IS NULL)
		   AND NOT EXISTS (
		       SELECT 1
		         FROM installation_repositories m
		         JOIN org_github_app_installations i
		           ON i.org_id = m.org_id AND i.installation_id = m.installation_id
		        WHERE m.org_id = ?
		          AND i.removed_at IS NULL
		          AND LOWER(m.owner) = LOWER(g.owner)
		          AND LOWER(m.repo) = LOWER(g.repo))
		 GROUP BY LOWER(g.owner), LOWER(g.repo)
		 ORDER BY LOWER(g.owner), LOWER(g.repo)
	`, orgID, orgID)
	if err != nil {
		return nil, fmt.Errorf("read scope drift: %w", err)
	}
	defer rows.Close()
	out := []domain.TeamGitHubRepo{}
	for rows.Next() {
		var r domain.TeamGitHubRepo
		if err := rows.Scan(&r.Owner, &r.Repo); err != nil {
			return nil, fmt.Errorf("scan scope drift: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func scanInstallationRepos(rows *sql.Rows, err error) ([]domain.InstallationRepository, error) {
	if err != nil {
		return nil, fmt.Errorf("read installation_repositories: %w", err)
	}
	defer rows.Close()
	out := []domain.InstallationRepository{}
	for rows.Next() {
		var (
			r          domain.InstallationRepository
			externalID sql.NullString
		)
		if err := rows.Scan(&r.OrgID, &r.InstallationID, &r.Source, &r.Owner, &r.Repo, &externalID, &r.ObservedAt); err != nil {
			return nil, fmt.Errorf("scan installation_repositories: %w", err)
		}
		r.ExternalID = externalID.String
		out = append(out, r)
	}
	return out, rows.Err()
}
