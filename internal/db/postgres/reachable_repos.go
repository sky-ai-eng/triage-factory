package postgres

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// reachableReposStore is the Postgres impl of db.ReachableReposStore. Admin pool
// only: the RLS policies on reachable_repositories deny tf_app every write,
// exactly like the installation rows the App half hangs off, because there is no
// user gesture that adds or removes a reachable entry.
type reachableReposStore struct{ admin queryer }

func newReachableReposStore(admin queryer) db.ReachableReposStore {
	return &reachableReposStore{admin: admin}
}

var _ db.ReachableReposStore = (*reachableReposStore)(nil)

// reachableColumns is the projection every row-returning read here shares, so a
// column added to the table lands in one place rather than in five SELECTs that
// then disagree about what a reachable row is.
const reachableColumns = `r.org_id, r.credential_class, COALESCE(r.installation_id, ''), COALESCE(r.host, ''),
	r.source, r.owner, r.repo, r.external_id, r.description, r.language, r.html_url, r.pushed_at,
	r.private, r.observed_at`

// ReplaceForInstallationSystem swaps one installation's entries for repos inside
// a single transaction: delete the installation's rows, insert the new answer.
//
// Delete-then-insert rather than a mark-and-sweep is the shape the data wants —
// the cache has no durable identity, so there is nothing on a row worth
// preserving across passes and no epoch column whose tie-breaking could go
// wrong. Under MVCC a concurrent reader sees the previous answer until this
// commits, so the intermediate empty state is unobservable, and a failure
// mid-insert rolls back to the previous answer rather than to nothing.
//
// The insert conflicts on the case-folded identity index, which is what makes
// two casings of one repository in GitHub's own answer collapse to one row
// instead of aborting the transaction.
func (s *reachableReposStore) ReplaceForInstallationSystem(ctx context.Context, orgID string, class domain.GitHubCredentialClass, installationID string, repos []domain.ReachableRepository) error {
	if !isValidUUID(orgID) {
		return fmt.Errorf("replace reachable repositories: invalid org id %q", orgID)
	}
	if !class.AppTier() {
		return fmt.Errorf("replace reachable repositories: %q is not an installation-scoped credential class", class)
	}
	if installationID == "" {
		return fmt.Errorf("replace reachable repositories: empty installation id")
	}
	rows, err := normalizeReachable(repos, class, installationID, "")
	if err != nil {
		return err
	}
	return inTx(ctx, s.admin, func(tx queryer) error {
		// Addressed by installation rather than by (class, installation): the
		// entries being replaced are this installation's, whatever class observed
		// them, and only the App classes carry an installation at all. It also
		// keeps the delete and the insert reading the same key as the identity
		// index, which spans both App classes — a class-scoped delete could leave
		// a row the insert then silently conflicts with.
		if _, err := tx.ExecContext(ctx, `
			DELETE FROM reachable_repositories
			 WHERE org_id = $1 AND installation_id = $2
		`, orgID, installationID); err != nil {
			return fmt.Errorf("clear reachable repositories: %w", err)
		}
		if err := insertReachable(ctx, tx, orgID, rows); err != nil {
			return err
		}
		return markScopeRefreshed(ctx, tx, orgID, class, installationID, observedAt(rows))
	})
}

// ReplaceForPATSystem swaps the org's whole pat-class set for repos observed
// against host. Whole-class rather than per-host: an org authenticates against
// one GitHub deployment at a time, so rows left by a previous GitHubBaseURL are
// the old deployment's repositories rather than a second scope to keep.
func (s *reachableReposStore) ReplaceForPATSystem(ctx context.Context, orgID, host string, repos []domain.ReachableRepository) error {
	if !isValidUUID(orgID) {
		return fmt.Errorf("replace reachable repositories: invalid org id %q", orgID)
	}
	if host == "" {
		return fmt.Errorf("replace reachable repositories: empty host")
	}
	rows, err := normalizeReachable(repos, domain.GitHubCredentialClassPAT, "", host)
	if err != nil {
		return err
	}
	return inTx(ctx, s.admin, func(tx queryer) error {
		if _, err := tx.ExecContext(ctx, `
			DELETE FROM reachable_repositories
			 WHERE org_id = $1 AND credential_class = $2
		`, orgID, string(domain.GitHubCredentialClassPAT)); err != nil {
			return fmt.Errorf("clear reachable repositories: %w", err)
		}
		// The whole class is replaced, so previous hosts' scope rows go with it —
		// otherwise a repointed GitHubBaseURL would leave the old deployment's
		// refresh vouching for the new one's freshness.
		if _, err := tx.ExecContext(ctx, `
			DELETE FROM reachable_scopes WHERE org_id = $1 AND credential_class = $2
		`, orgID, string(domain.GitHubCredentialClassPAT)); err != nil {
			return fmt.Errorf("clear reachable scopes: %w", err)
		}
		if err := insertReachable(ctx, tx, orgID, rows); err != nil {
			return err
		}
		return markScopeRefreshed(ctx, tx, orgID, domain.GitHubCredentialClassPAT, host, observedAt(rows))
	})
}

// ClearForInstallationSystem fails closed on a malformed argument rather than
// reporting success for a delete that matched nothing. It is a write path called
// only when the reach is known to be gone, so "no rows matched" and "the caller
// named the wrong installation" must not be the same answer — the second would
// leave the mirror answering for a grant that no longer exists.
func (s *reachableReposStore) ClearForInstallationSystem(ctx context.Context, orgID, installationID string) error {
	if !isValidUUID(orgID) {
		return fmt.Errorf("clear reachable repositories: invalid org id %q", orgID)
	}
	if installationID == "" {
		return fmt.Errorf("clear reachable repositories: empty installation id")
	}
	classes, classArgs := appTierClassArgs(2)
	return inTx(ctx, s.admin, func(tx queryer) error {
		if _, err := tx.ExecContext(ctx, `
			DELETE FROM reachable_repositories
			 WHERE org_id = $1 AND installation_id = $2
		`, orgID, installationID); err != nil {
			return fmt.Errorf("clear reachable repositories: %w", err)
		}
		// The scope row goes with the entries: leaving it would keep vouching that
		// this installation's reach had been established, for an installation that
		// no longer reaches anything. Narrowed to the App classes because scope
		// holds a host for the PAT tier, and this argument is an installation id.
		if _, err := tx.ExecContext(ctx, `
			DELETE FROM reachable_scopes
			 WHERE org_id = $1 AND scope = $2 AND credential_class IN (`+classes+`)
		`, append([]any{orgID, installationID}, classArgs...)...); err != nil {
			return fmt.Errorf("clear reachable scope: %w", err)
		}
		return nil
	})
}

func (s *reachableReposStore) ListForOrgSystem(ctx context.Context, orgID string) ([]domain.ReachableRepository, error) {
	if !isValidUUID(orgID) {
		return []domain.ReachableRepository{}, nil
	}
	return scanReachableRepos(s.admin.QueryContext(ctx, `
		SELECT `+reachableColumns+`
		  FROM reachable_repositories r
		  JOIN org_github_app_installations i
		    ON i.org_id = r.org_id AND i.installation_id = r.installation_id
		 WHERE r.org_id = $1 AND r.credential_class = $2 AND i.removed_at IS NULL
		 ORDER BY r.installation_id, lower(r.owner), lower(r.repo)
	`, orgID, string(domain.GitHubCredentialClassBYOApp)))
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
func (s *reachableReposStore) ListReachWithoutPurposeSystem(ctx context.Context, orgID string) ([]domain.ReachableRepository, error) {
	if !isValidUUID(orgID) {
		return []domain.ReachableRepository{}, nil
	}
	return scanReachableRepos(s.admin.QueryContext(ctx, `
		SELECT `+reachableColumns+`
		  FROM reachable_repositories r
		  JOIN org_github_app_installations i
		    ON i.org_id = r.org_id AND i.installation_id = r.installation_id
		 WHERE r.org_id = $1
		   AND r.credential_class = $2
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
	`, orgID, string(domain.GitHubCredentialClassBYOApp)))
}

// ListScopeDriftSystem: tracked, granted by nobody.
//
// The EXISTS gate is the whole difference between a finding and a fabrication.
// An org with no App, or one whose first refresh has not landed, has an empty
// mirror — and "not in the mirror" then describes every repository it tracks.
// Reporting those as drift would turn "we have not looked yet" into "your
// tracking is broken", so with no mirror at all the answer is empty.
func (s *reachableReposStore) ListScopeDriftSystem(ctx context.Context, orgID string) ([]domain.TeamGitHubRepo, error) {
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
		         FROM reachable_repositories m
		         JOIN org_github_app_installations i
		           ON i.org_id = m.org_id AND i.installation_id = m.installation_id
		        WHERE m.org_id = $1 AND m.credential_class = $2 AND i.removed_at IS NULL)
		   AND NOT EXISTS (
		       SELECT 1
		         FROM reachable_repositories m
		         JOIN org_github_app_installations i
		           ON i.org_id = m.org_id AND i.installation_id = m.installation_id
		        WHERE m.org_id = $1
		          AND m.credential_class = $2
		          AND i.removed_at IS NULL
		          AND lower(m.owner) = lower(reg.owner)
		          AND lower(m.repo) = lower(reg.repo))
		 GROUP BY lower(reg.owner), lower(reg.repo)
		 ORDER BY lower(reg.owner), lower(reg.repo)
	`, orgID, string(domain.GitHubCredentialClassBYOApp))
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

func (s *reachableReposStore) ListReachableSystem(ctx context.Context, orgID string, class domain.GitHubCredentialClass, q string, opts db.ListOpts) ([]domain.ReachableRepository, int, error) {
	if !class.Known() {
		return nil, 0, fmt.Errorf("list reachable repositories: unknown credential class %q", class)
	}
	if !isValidUUID(orgID) {
		return []domain.ReachableRepository{}, 0, nil
	}
	where := ` WHERE r.org_id = $1 AND r.credential_class = $2`
	args := []any{orgID, string(class)}
	if term := strings.TrimSpace(q); term != "" {
		args = append(args, "%"+db.LikeEscape(strings.ToLower(term))+"%")
		p := fmt.Sprintf("$%d", len(args))
		where += fmt.Sprintf(` AND (lower(r.owner || '/' || r.repo) LIKE %[1]s ESCAPE '\'
		            OR lower(r.description) LIKE %[1]s ESCAPE '\'
		            OR lower(r.language) LIKE %[1]s ESCAPE '\')`, p)
	}

	var total int
	if err := s.admin.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM reachable_repositories r`+where, args...).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("count reachable repositories: %w", err)
	}
	if opts.CountOnly {
		return []domain.ReachableRepository{}, total, nil
	}

	query := `SELECT ` + reachableColumns + ` FROM reachable_repositories r` + where +
		` ORDER BY lower(r.owner), lower(r.repo)`
	if opts.Limit > 0 {
		args = append(args, opts.Limit)
		query += fmt.Sprintf(" LIMIT $%d", len(args))
		if opts.Offset > 0 {
			args = append(args, opts.Offset)
			query += fmt.Sprintf(" OFFSET $%d", len(args))
		}
	}
	rows, err := scanReachableRepos(s.admin.QueryContext(ctx, query, args...))
	if err != nil {
		return nil, 0, err
	}
	return rows, total, nil
}

func (s *reachableReposStore) ReachableStateSystem(ctx context.Context, orgID string, class domain.GitHubCredentialClass) (domain.ReachableCacheState, error) {
	if !class.Known() {
		return domain.ReachableCacheState{}, fmt.Errorf("read reachable cache state: unknown credential class %q", class)
	}
	if !isValidUUID(orgID) {
		return domain.ReachableCacheState{}, nil
	}
	// Two reads, because the two questions are genuinely different. Whether the
	// org has EVER been refreshed, and how stale the stalest scope is, come from
	// the scope rows — a scope that reaches nothing writes no repository rows, so
	// asking the repository table would read a legitimately empty grant as an
	// un-run refresh. How much it holds comes from the repository rows.
	//
	// MIN(refreshed_at), so the answer describes the STALEST scope: an org with
	// two installations — one refreshed a minute ago, one a week ago — is a week
	// stale for the repositories on the second account.
	var (
		scopes    int
		refreshed sql.NullTime
	)
	if err := s.admin.QueryRowContext(ctx, `
		SELECT COUNT(*), MIN(refreshed_at)
		  FROM reachable_scopes
		 WHERE org_id = $1 AND credential_class = $2
	`, orgID, string(class)).Scan(&scopes, &refreshed); err != nil {
		return domain.ReachableCacheState{}, fmt.Errorf("read reachable scope state: %w", err)
	}
	var count int
	if err := s.admin.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM reachable_repositories
		 WHERE org_id = $1 AND credential_class = $2
	`, orgID, string(class)).Scan(&count); err != nil {
		return domain.ReachableCacheState{}, fmt.Errorf("read reachable cache state: %w", err)
	}
	return domain.ReachableCacheState{Refreshed: scopes > 0, Count: count, ObservedAt: refreshed.Time}, nil
}

func (s *reachableReposStore) ReachableSlugsSystem(ctx context.Context, orgID string, class domain.GitHubCredentialClass, slugs []string) (map[string]struct{}, error) {
	if !class.Known() {
		return nil, fmt.Errorf("read reachable slugs: unknown credential class %q", class)
	}
	out := map[string]struct{}{}
	if !isValidUUID(orgID) || len(slugs) == 0 {
		return out, nil
	}
	// An IN list of numbered placeholders rather than a text[] literal: the
	// slugs originate in a request body, and there is no array-literal helper in
	// this package that quotes them — one bound parameter per slug cannot be
	// mis-escaped at all. The list is bounded by the caller's selection.
	args := []any{orgID, string(class)}
	placeholders := make([]string, 0, len(slugs))
	for _, slug := range slugs {
		args = append(args, strings.ToLower(slug))
		placeholders = append(placeholders, fmt.Sprintf("$%d", len(args)))
	}
	rows, err := s.admin.QueryContext(ctx, `
		SELECT lower(owner || '/' || repo)
		  FROM reachable_repositories
		 WHERE org_id = $1 AND credential_class = $2
		   AND lower(owner || '/' || repo) IN (`+strings.Join(placeholders, ",")+`)
	`, args...)
	if err != nil {
		return nil, fmt.Errorf("read reachable slugs: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var slug string
		if err := rows.Scan(&slug); err != nil {
			return nil, fmt.Errorf("scan reachable slug: %w", err)
		}
		out[slug] = struct{}{}
	}
	return out, rows.Err()
}

// appTierClassArgs renders the App-shaped credential classes as a bound IN list:
// the placeholders to interpolate, and the values to append to the query's
// arguments. bound is how many parameters the statement already binds, since
// this dialect numbers them. Built from the domain list rather than spelled in
// SQL so a class added there reaches the two statements that ask "is this scope
// an installation's?" without either being edited.
func appTierClassArgs(bound int) (string, []any) {
	classes := domain.AppTierCredentialClasses()
	placeholders := make([]string, 0, len(classes))
	args := make([]any, 0, len(classes))
	for _, class := range classes {
		args = append(args, string(class))
		placeholders = append(placeholders, fmt.Sprintf("$%d", bound+len(args)))
	}
	return strings.Join(placeholders, ","), args
}

// normalizeReachable stamps the scope every row in one replace shares and
// validates the provider source, BEFORE the transaction opens: a bad source is a
// caller bug, and discovering it halfway through the replace would leave the work
// to a rollback that only exists to undo a mistake we could refuse outright.
//
// The scope is taken from the method rather than from the rows, so a caller
// cannot hand one replace two scopes' worth of entries and have half of them
// silently escape the delete that is supposed to precede them.
func normalizeReachable(repos []domain.ReachableRepository, class domain.GitHubCredentialClass, installationID, host string) ([]domain.ReachableRepository, error) {
	observed := refreshInstant()
	rows := make([]domain.ReachableRepository, 0, len(repos))
	for _, r := range repos {
		source, err := domain.NormalizeRepoSource(r.Source)
		if err != nil {
			return nil, fmt.Errorf("replace reachable repositories: %w", err)
		}
		r.Source = source
		r.CredentialClass = class
		r.InstallationID = installationID
		r.Host = host
		r.ObservedAt = observed
		rows = append(rows, r)
	}
	return rows, nil
}

// refreshInstant is the one timestamp a whole replace carries — its rows and its
// scope marker alike, so "the entries as of T" and "refreshed at T" cannot
// disagree by the width of the transaction.
func refreshInstant() time.Time { return time.Now().UTC() }

// observedAt recovers that instant from the normalized rows. An empty scope has
// no rows to recover it from, and a scope that reaches nothing is exactly the
// case the marker exists for — so it falls back to now, which for a replace that
// just ran is the same instant to within the transaction.
func observedAt(rows []domain.ReachableRepository) time.Time {
	if len(rows) > 0 {
		return rows[0].ObservedAt
	}
	return refreshInstant()
}

// insertReachable writes one replace's rows. The scope columns are the
// alternatives the table's CHECK enforces, so the unused one goes in as NULL
// rather than as an empty string that would satisfy the constraint while meaning
// nothing.
func insertReachable(ctx context.Context, tx queryer, orgID string, rows []domain.ReachableRepository) error {
	for _, r := range rows {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO reachable_repositories
				(org_id, credential_class, installation_id, host, source, owner, repo,
				 external_id, description, language, html_url, pushed_at, private, observed_at)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14)
			ON CONFLICT DO NOTHING
		`, orgID, string(r.CredentialClass), nullString(r.InstallationID), nullString(r.Host),
			r.Source, r.Owner, r.Repo, nullString(r.ExternalID),
			r.Description, r.Language, r.HTMLURL, r.PushedAt, r.Private, r.ObservedAt); err != nil {
			return fmt.Errorf("insert reachable repository %s: %w", r.Slug(), err)
		}
	}
	return nil
}

// markScopeRefreshed records that this scope's reach was just established. It is
// what makes "we have looked and found nothing" distinguishable from "we have
// not looked", which no count of repository rows can express.
func markScopeRefreshed(ctx context.Context, tx queryer, orgID string, class domain.GitHubCredentialClass, scope string, at time.Time) error {
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO reachable_scopes (org_id, credential_class, scope, refreshed_at)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT (org_id, credential_class, scope) DO UPDATE SET refreshed_at = excluded.refreshed_at
	`, orgID, string(class), scope, at); err != nil {
		return fmt.Errorf("mark reachable scope refreshed: %w", err)
	}
	return nil
}

func scanReachableRepos(rows *sql.Rows, err error) ([]domain.ReachableRepository, error) {
	if err != nil {
		return nil, fmt.Errorf("read reachable_repositories: %w", err)
	}
	defer rows.Close()
	out := []domain.ReachableRepository{}
	for rows.Next() {
		var (
			r          domain.ReachableRepository
			class      string
			externalID sql.NullString
		)
		if err := rows.Scan(&r.OrgID, &class, &r.InstallationID, &r.Host, &r.Source, &r.Owner, &r.Repo,
			&externalID, &r.Description, &r.Language, &r.HTMLURL, &r.PushedAt, &r.Private, &r.ObservedAt); err != nil {
			return nil, fmt.Errorf("scan reachable_repositories: %w", err)
		}
		r.CredentialClass = domain.GitHubCredentialClass(class)
		r.ExternalID = externalID.String
		out = append(out, r)
	}
	return out, rows.Err()
}
