package sqlite

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// reachableReposStore is the SQLite (local-mode) impl of
// db.ReachableReposStore. One connection, no RLS, one org — so the admin/app
// split the Postgres impl draws collapses, and every method re-asserts the local
// org sentinel the way the rest of this package does.
type reachableReposStore struct{ q queryer }

func newReachableReposStore(q queryer) db.ReachableReposStore {
	return &reachableReposStore{q: q}
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
// wrong. Both halves are in one transaction, so a reader never observes the
// intermediate empty state and a failure mid-insert rolls back to the previous
// answer rather than to nothing.
//
// The insert conflicts on the case-folded identity index, which is what makes
// two casings of one repository in GitHub's own answer collapse to one row
// instead of raising.
func (s *reachableReposStore) ReplaceForInstallationSystem(ctx context.Context, orgID string, class domain.GitHubCredentialClass, installationID string, repos []domain.ReachableRepository) error {
	if err := assertLocalOrg(orgID); err != nil {
		return err
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
	return inTx(ctx, s.q, func(tx queryer) error {
		// Addressed by installation rather than by (class, installation): the
		// entries being replaced are this installation's, whatever class observed
		// them, and only the App classes carry an installation at all. It also
		// keeps the delete and the insert reading the same key as the identity
		// index, which spans both App classes — a class-scoped delete could leave
		// a row the insert then silently conflicts with.
		if _, err := tx.ExecContext(ctx, `
			DELETE FROM reachable_repositories
			 WHERE org_id = ? AND installation_id = ?
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
	if err := assertLocalOrg(orgID); err != nil {
		return err
	}
	if host == "" {
		return fmt.Errorf("replace reachable repositories: empty host")
	}
	rows, err := normalizeReachable(repos, domain.GitHubCredentialClassPAT, "", host)
	if err != nil {
		return err
	}
	return inTx(ctx, s.q, func(tx queryer) error {
		if _, err := tx.ExecContext(ctx, `
			DELETE FROM reachable_repositories
			 WHERE org_id = ? AND credential_class = ?
		`, orgID, string(domain.GitHubCredentialClassPAT)); err != nil {
			return fmt.Errorf("clear reachable repositories: %w", err)
		}
		// The whole class is replaced, so previous hosts' scope rows go with it —
		// otherwise a repointed GitHubBaseURL would leave the old deployment's
		// refresh vouching for the new one's freshness.
		if _, err := tx.ExecContext(ctx, `
			DELETE FROM reachable_scopes WHERE org_id = ? AND credential_class = ?
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
	if err := assertLocalOrg(orgID); err != nil {
		return err
	}
	if installationID == "" {
		return fmt.Errorf("clear reachable repositories: empty installation id")
	}
	classes, classArgs := appTierClassArgs()
	return inTx(ctx, s.q, func(tx queryer) error {
		if _, err := tx.ExecContext(ctx, `
			DELETE FROM reachable_repositories
			 WHERE org_id = ? AND installation_id = ?
		`, orgID, installationID); err != nil {
			return fmt.Errorf("clear reachable repositories: %w", err)
		}
		// The scope row goes with the entries: leaving it would keep vouching that
		// this installation's reach had been established, for an installation that
		// no longer reaches anything. Narrowed to the App classes because scope
		// holds a host for the PAT tier, and this argument is an installation id.
		if _, err := tx.ExecContext(ctx, `
			DELETE FROM reachable_scopes
			 WHERE org_id = ? AND scope = ? AND credential_class IN (`+classes+`)
		`, append([]any{orgID, installationID}, classArgs...)...); err != nil {
			return fmt.Errorf("clear reachable scope: %w", err)
		}
		return nil
	})
}

func (s *reachableReposStore) ListForOrgSystem(ctx context.Context, orgID string, class domain.GitHubCredentialClass) ([]domain.ReachableRepository, error) {
	if err := assertLocalOrg(orgID); err != nil {
		return nil, err
	}
	if err := db.RequireGrantClass("list grant entries", class); err != nil {
		return nil, err
	}
	return scanReachableRepos(s.q.QueryContext(ctx, `
		SELECT `+reachableColumns+`
		  FROM reachable_repositories r
		  JOIN org_github_app_installations i
		    ON i.org_id = r.org_id AND i.installation_id = r.installation_id
		 WHERE r.org_id = ? AND r.credential_class = ? AND i.removed_at IS NULL
		 ORDER BY r.installation_id, LOWER(r.owner), LOWER(r.repo)
	`, orgID, string(class)))
}

// ListReachWithoutPurposeSystem: granted, tracked by nobody. The NOT EXISTS is
// case-folded on both sides because GitHub identifiers are case-insensitive and
// a case-sensitive comparison here would report a tracked repository as
// untracked purely because someone capitalized the slug differently — a
// fabricated security finding, which is worse than a missed one.
func (s *reachableReposStore) ListReachWithoutPurposeSystem(ctx context.Context, orgID string, class domain.GitHubCredentialClass, opts db.ListOpts) ([]domain.ReachableRepository, int, error) {
	if err := assertLocalOrg(orgID); err != nil {
		return nil, 0, err
	}
	if err := db.RequireGrantClass("list reach without purpose", class); err != nil {
		return nil, 0, err
	}
	from := `
		  FROM reachable_repositories r
		  JOIN org_github_app_installations i
		    ON i.org_id = r.org_id AND i.installation_id = r.installation_id
		 WHERE r.org_id = ?
		   AND r.credential_class = ?
		   AND i.removed_at IS NULL
		   AND NOT EXISTS (
		       SELECT 1 FROM team_github_repos g
		        JOIN repositories reg ON reg.id = g.repository_id
		        WHERE LOWER(reg.owner) = LOWER(r.owner)
		          AND LOWER(reg.repo) = LOWER(r.repo))`
	args := []any{orgID, string(class)}

	var total int
	if err := s.q.QueryRowContext(ctx, `SELECT COUNT(*)`+from, args...).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("count reach without purpose: %w", err)
	}
	if opts.CountOnly {
		return []domain.ReachableRepository{}, total, nil
	}
	query := `SELECT ` + reachableColumns + from + ` ORDER BY r.installation_id, LOWER(r.owner), LOWER(r.repo)`
	if opts.Limit > 0 {
		query += ` LIMIT ?`
		args = append(args, opts.Limit)
		if opts.Offset > 0 {
			query += ` OFFSET ?`
			args = append(args, opts.Offset)
		}
	}
	rows, err := scanReachableRepos(s.q.QueryContext(ctx, query, args...))
	if err != nil {
		return nil, 0, err
	}
	return rows, total, nil
}

// ListScopeDriftSystem: tracked, granted by nobody.
//
// The EXISTS gate is the whole difference between a finding and a fabrication.
// An org with no App, or one whose first refresh has not landed, has an empty
// mirror — and "not in the mirror" then describes every repository it tracks.
// Reporting those as drift would turn "we have not looked yet" into "your
// tracking is broken", so until a scope has been refreshed the answer is empty.
// It reads the scope markers rather than the entry rows so a selective grant
// that contains nothing — refreshed, and found empty — still reports what is
// tracked against it.
//
// The second NOT EXISTS is the three-way rule on the owner account's
// installation: a grant of every repository cannot be drifted out of, and a
// grant of unknown width cannot be said to have been. Only a selective grant, or
// no live installation on the account at all, leaves a tracked repository
// reportable.
func (s *reachableReposStore) ListScopeDriftSystem(ctx context.Context, orgID string, class domain.GitHubCredentialClass, opts db.ListOpts) ([]domain.ScopeDriftRepository, int, error) {
	if err := assertLocalOrg(orgID); err != nil {
		return nil, 0, err
	}
	if err := db.RequireGrantClass("list scope drift", class); err != nil {
		return nil, 0, err
	}
	// Grouped on the folded slug rather than DISTINCT on the raw pair: two teams
	// tracking one repository under different casings are tracking one
	// repository, and listing it twice would double-count the finding. MIN picks
	// a stable spelling from the group, whose members differ only in case.
	//
	// The LEFT JOIN resolves the live installation on the owner account. After
	// the rule below it can only be a selective one, and GitHub allows one
	// installation of an App per account, so MIN over the group is the one id.
	from := `
		  FROM team_github_repos g
		  JOIN repositories reg ON reg.id = g.repository_id
		  LEFT JOIN org_github_app_installations cov
		    ON cov.org_id = ? AND cov.removed_at IS NULL
		   AND LOWER(cov.account_login) = LOWER(reg.owner)
		 WHERE EXISTS (
		       SELECT 1 FROM reachable_scopes sc
		        WHERE sc.org_id = ? AND sc.credential_class = ?)
		   AND NOT EXISTS (
		       SELECT 1
		         FROM reachable_repositories m
		         JOIN org_github_app_installations i
		           ON i.org_id = m.org_id AND i.installation_id = m.installation_id
		        WHERE m.org_id = ?
		          AND m.credential_class = ?
		          AND i.removed_at IS NULL
		          AND LOWER(m.owner) = LOWER(reg.owner)
		          AND LOWER(m.repo) = LOWER(reg.repo))
		   AND NOT EXISTS (
		       SELECT 1 FROM org_github_app_installations w
		        WHERE w.org_id = ? AND w.removed_at IS NULL
		          AND LOWER(w.account_login) = LOWER(reg.owner)
		          AND (w.repository_selection IS NULL OR w.repository_selection = ?))
		 GROUP BY LOWER(reg.owner), LOWER(reg.repo)`
	args := []any{orgID, orgID, string(class), orgID, string(class), orgID, domain.RepositorySelectionAll}

	var total int
	if err := s.q.QueryRowContext(ctx, `SELECT COUNT(*) FROM (SELECT 1`+from+`)`, args...).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("count scope drift: %w", err)
	}
	if opts.CountOnly {
		return []domain.ScopeDriftRepository{}, total, nil
	}
	query := `SELECT MIN(reg.owner), MIN(reg.repo), COALESCE(MIN(cov.installation_id), '')` + from +
		` ORDER BY LOWER(reg.owner), LOWER(reg.repo)`
	if opts.Limit > 0 {
		query += ` LIMIT ?`
		args = append(args, opts.Limit)
		if opts.Offset > 0 {
			query += ` OFFSET ?`
			args = append(args, opts.Offset)
		}
	}
	rows, err := s.q.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, 0, fmt.Errorf("read scope drift: %w", err)
	}
	defer rows.Close()
	out := []domain.ScopeDriftRepository{}
	for rows.Next() {
		var r domain.ScopeDriftRepository
		if err := rows.Scan(&r.Owner, &r.Repo, &r.InstallationID); err != nil {
			return nil, 0, fmt.Errorf("scan scope drift: %w", err)
		}
		out = append(out, r)
	}
	return out, total, rows.Err()
}

func (s *reachableReposStore) ListReachableSystem(ctx context.Context, orgID string, class domain.GitHubCredentialClass, q string, opts db.ListOpts) ([]domain.ReachableRepository, int, error) {
	if err := assertLocalOrg(orgID); err != nil {
		return nil, 0, err
	}
	if !class.Known() {
		return nil, 0, fmt.Errorf("list reachable repositories: unknown credential class %q", class)
	}
	where := ` WHERE r.org_id = ? AND r.credential_class = ?`
	args := []any{orgID, string(class)}
	if term := strings.TrimSpace(q); term != "" {
		pattern := "%" + db.LikeEscape(strings.ToLower(term)) + "%"
		where += ` AND (LOWER(r.owner || '/' || r.repo) LIKE ? ESCAPE '\'
		            OR LOWER(r.description) LIKE ? ESCAPE '\'
		            OR LOWER(r.language) LIKE ? ESCAPE '\')`
		args = append(args, pattern, pattern, pattern)
	}

	var total int
	if err := s.q.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM reachable_repositories r`+where, args...).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("count reachable repositories: %w", err)
	}
	if opts.CountOnly {
		return []domain.ReachableRepository{}, total, nil
	}

	query := `SELECT ` + reachableColumns + ` FROM reachable_repositories r` + where +
		` ORDER BY LOWER(r.owner), LOWER(r.repo)`
	if opts.Limit > 0 {
		query += ` LIMIT ?`
		args = append(args, opts.Limit)
		if opts.Offset > 0 {
			query += ` OFFSET ?`
			args = append(args, opts.Offset)
		}
	}
	rows, err := scanReachableRepos(s.q.QueryContext(ctx, query, args...))
	if err != nil {
		return nil, 0, err
	}
	return rows, total, nil
}

func (s *reachableReposStore) ReachableStateSystem(ctx context.Context, orgID string, class domain.GitHubCredentialClass) (domain.ReachableCacheState, error) {
	if err := assertLocalOrg(orgID); err != nil {
		return domain.ReachableCacheState{}, err
	}
	if !class.Known() {
		return domain.ReachableCacheState{}, fmt.Errorf("read reachable cache state: unknown credential class %q", class)
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
	//
	// The aggregate is scanned as text and parsed rather than into a time.Time:
	// an aggregate column has no declared type for the driver to infer from, so
	// it hands back whatever string the value was stored as. parseDBDatetime is
	// the same reader every other timestamp on this dialect goes through.
	var (
		scopes    int
		refreshed sql.NullString
	)
	if err := s.q.QueryRowContext(ctx, `
		SELECT COUNT(*), MIN(refreshed_at)
		  FROM reachable_scopes
		 WHERE org_id = ? AND credential_class = ?
	`, orgID, string(class)).Scan(&scopes, &refreshed); err != nil {
		return domain.ReachableCacheState{}, fmt.Errorf("read reachable scope state: %w", err)
	}
	var count int
	if err := s.q.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM reachable_repositories
		 WHERE org_id = ? AND credential_class = ?
	`, orgID, string(class)).Scan(&count); err != nil {
		return domain.ReachableCacheState{}, fmt.Errorf("read reachable cache state: %w", err)
	}
	at, err := parseDBDatetime(refreshed.String)
	if err != nil {
		return domain.ReachableCacheState{}, fmt.Errorf("parse reachable refreshed_at %q: %w", refreshed.String, err)
	}
	return domain.ReachableCacheState{Refreshed: scopes > 0, Count: count, ObservedAt: at}, nil
}

func (s *reachableReposStore) ReachableSlugsSystem(ctx context.Context, orgID string, class domain.GitHubCredentialClass, slugs []string) (map[string]struct{}, error) {
	if err := assertLocalOrg(orgID); err != nil {
		return nil, err
	}
	if !class.Known() {
		return nil, fmt.Errorf("read reachable slugs: unknown credential class %q", class)
	}
	out := map[string]struct{}{}
	if len(slugs) == 0 {
		return out, nil
	}
	args := []any{orgID, string(class)}
	placeholders := make([]string, 0, len(slugs))
	for _, slug := range slugs {
		placeholders = append(placeholders, "?")
		args = append(args, strings.ToLower(slug))
	}
	rows, err := s.q.QueryContext(ctx, `
		SELECT LOWER(owner || '/' || repo)
		  FROM reachable_repositories
		 WHERE org_id = ? AND credential_class = ?
		   AND LOWER(owner || '/' || repo) IN (`+strings.Join(placeholders, ",")+`)
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
// arguments. Built from the domain list rather than spelled in SQL so a class
// added there reaches the two statements that ask "is this scope an
// installation's?" without either being edited.
func appTierClassArgs() (string, []any) {
	classes := domain.AppTierCredentialClasses()
	placeholders := make([]string, 0, len(classes))
	args := make([]any, 0, len(classes))
	for _, class := range classes {
		placeholders = append(placeholders, "?")
		args = append(args, string(class))
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
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT DO NOTHING
		`, orgID, string(r.CredentialClass), nullStringValue(r.InstallationID), nullStringValue(r.Host),
			r.Source, r.Owner, r.Repo, nullStringValue(r.ExternalID),
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
		VALUES (?, ?, ?, ?)
		ON CONFLICT(org_id, credential_class, scope) DO UPDATE SET refreshed_at = excluded.refreshed_at
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
