package postgres

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// installationReposStore is the Postgres impl of db.InstallationReposStore.
// Admin pool only: the RLS policies on installation_repositories deny tf_app
// every write, exactly like the installation rows the mirror hangs off, because
// there is no user gesture that adds or removes a grant entry.
type installationReposStore struct{ admin queryer }

func newInstallationReposStore(admin queryer) db.InstallationReposStore {
	return &installationReposStore{admin: admin}
}

var _ db.InstallationReposStore = (*installationReposStore)(nil)

// ReplaceForInstallationSystem swaps one installation's mirror for repos inside
// a single transaction: delete the installation's rows, insert the new answer.
//
// Delete-then-insert rather than a mark-and-sweep is the shape the data wants —
// the mirror is a cache of an external fact with no durable identity, so there
// is nothing on a row worth preserving across passes and no epoch column whose
// tie-breaking could go wrong. Under MVCC a concurrent reader sees the previous
// answer until this commits, so the intermediate empty state is unobservable,
// and a failure mid-insert rolls back to the previous answer rather than to
// nothing.
//
// The insert conflicts on the case-folded identity index, which is what makes
// two casings of one repository in GitHub's own answer collapse to one row
// instead of aborting the transaction.
func (s *installationReposStore) ReplaceForInstallationSystem(ctx context.Context, orgID, installationID string, repos []domain.InstallationRepository) error {
	if !isValidUUID(orgID) {
		return fmt.Errorf("replace installation repositories: invalid org id %q", orgID)
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

	return inTx(ctx, s.admin, func(tx queryer) error {
		if _, err := tx.ExecContext(ctx, `
			DELETE FROM installation_repositories
			 WHERE org_id = $1 AND installation_id = $2
		`, orgID, installationID); err != nil {
			return fmt.Errorf("clear installation repositories: %w", err)
		}
		for _, r := range rows {
			if _, err := tx.ExecContext(ctx, `
				INSERT INTO installation_repositories
					(org_id, installation_id, source, owner, repo, external_id, observed_at)
				VALUES ($1, $2, $3, $4, $5, $6, $7)
				ON CONFLICT DO NOTHING
			`, orgID, installationID, r.Source, r.Owner, r.Repo, nullString(r.ExternalID), r.ObservedAt); err != nil {
				return fmt.Errorf("insert installation repository %s: %w", r.Slug(), err)
			}
		}
		return nil
	})
}

// ClearForInstallationSystem fails closed on a malformed argument rather than
// reporting success for a delete that matched nothing. It is a write path
// called only when the grant is known to be gone, so "no rows matched" and "the
// caller named the wrong installation" must not be the same answer — the second
// would leave the mirror answering for a grant that no longer exists.
func (s *installationReposStore) ClearForInstallationSystem(ctx context.Context, orgID, installationID string) error {
	if !isValidUUID(orgID) {
		return fmt.Errorf("clear installation repositories: invalid org id %q", orgID)
	}
	if installationID == "" {
		return fmt.Errorf("clear installation repositories: empty installation id")
	}
	if _, err := s.admin.ExecContext(ctx, `
		DELETE FROM installation_repositories
		 WHERE org_id = $1 AND installation_id = $2
	`, orgID, installationID); err != nil {
		return fmt.Errorf("clear installation repositories: %w", err)
	}
	return nil
}

func (s *installationReposStore) ListForOrgSystem(ctx context.Context, orgID string) ([]domain.InstallationRepository, error) {
	if !isValidUUID(orgID) {
		return []domain.InstallationRepository{}, nil
	}
	return scanInstallationRepos(s.admin.QueryContext(ctx, `
		SELECT r.org_id, r.installation_id, r.source, r.owner, r.repo, r.external_id, r.observed_at
		  FROM installation_repositories r
		  JOIN org_github_app_installations i
		    ON i.org_id = r.org_id AND i.installation_id = r.installation_id
		 WHERE r.org_id = $1 AND i.removed_at IS NULL
		 ORDER BY r.installation_id, lower(r.owner), lower(r.repo)
	`, orgID))
}

// ListReachWithoutPurposeSystem: granted, tracked by nobody. The NOT EXISTS is
// case-folded on both sides because GitHub identifiers are case-insensitive and
// a case-sensitive comparison here would report a tracked repository as
// untracked purely because someone capitalized the slug differently — a
// fabricated security finding, which is worse than a missed one.
//
// Tracking crosses every team in the org, so the semi-join goes through teams
// on the admin pool: an org-level fact about the org's own App cannot be
// rendered from one viewer's team memberships.
func (s *installationReposStore) ListReachWithoutPurposeSystem(ctx context.Context, orgID string) ([]domain.InstallationRepository, error) {
	if !isValidUUID(orgID) {
		return []domain.InstallationRepository{}, nil
	}
	return scanInstallationRepos(s.admin.QueryContext(ctx, `
		SELECT r.org_id, r.installation_id, r.source, r.owner, r.repo, r.external_id, r.observed_at
		  FROM installation_repositories r
		  JOIN org_github_app_installations i
		    ON i.org_id = r.org_id AND i.installation_id = r.installation_id
		 WHERE r.org_id = $1
		   AND i.removed_at IS NULL
		   AND NOT EXISTS (
		       SELECT 1
		         FROM team_github_repos g
		         JOIN teams t ON t.id = g.team_id
		         JOIN repositories reg ON reg.id = g.repository_id
		        WHERE t.org_id = r.org_id
		          AND lower(reg.owner) = lower(r.owner)
		          AND lower(reg.repo) = lower(r.repo))
		 ORDER BY r.installation_id, lower(r.owner), lower(r.repo)
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
	if !isValidUUID(orgID) {
		return []domain.TeamGitHubRepo{}, nil
	}
	// Grouped on the folded slug rather than DISTINCT on the raw pair: two teams
	// tracking one repository under different casings are tracking one
	// repository, and listing it twice would double-count the finding. MIN picks
	// a stable spelling from the group, whose members differ only in case.
	rows, err := s.admin.QueryContext(ctx, `
		SELECT MIN(reg.owner), MIN(reg.repo)
		  FROM team_github_repos g
		  JOIN teams t ON t.id = g.team_id
		  JOIN repositories reg ON reg.id = g.repository_id
		 WHERE t.org_id = $1
		   AND EXISTS (
		       SELECT 1
		         FROM installation_repositories m
		         JOIN org_github_app_installations i
		           ON i.org_id = m.org_id AND i.installation_id = m.installation_id
		        WHERE m.org_id = $1 AND i.removed_at IS NULL)
		   AND NOT EXISTS (
		       SELECT 1
		         FROM installation_repositories m
		         JOIN org_github_app_installations i
		           ON i.org_id = m.org_id AND i.installation_id = m.installation_id
		        WHERE m.org_id = $1
		          AND i.removed_at IS NULL
		          AND lower(m.owner) = lower(reg.owner)
		          AND lower(m.repo) = lower(reg.repo))
		 GROUP BY lower(reg.owner), lower(reg.repo)
		 ORDER BY lower(reg.owner), lower(reg.repo)
	`, orgID)
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
