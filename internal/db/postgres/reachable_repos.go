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

// reachableOnHost is the predicate placing a reachable_repositories row (alias
// alias) on the GitHub host bound at param. Every row carries the host it was
// observed on — an App row its installation's, which the foreign key holds it
// to — and every read here is confined to the org's current host, so the rows
// an earlier host left behind are never offered, admitted or counted.
func reachableOnHost(alias, param string) string {
	return alias + `.host = ` + param
}

// registryMatchesMirror is the predicate pairing a registry row (alias reg)
// with a reachable entry (alias m) as one repository: by GitHub id when both
// carry one, and by folded name only when either does not. A registry row whose
// id differs from the entry's is a different repository even under the same
// name, because GitHub gives a name to one repository at a time and a row's
// name can go stale after GitHub moves it to another.
func registryMatchesMirror(reg, m string) string {
	return fmt.Sprintf(`(%[1]s.external_id = %[2]s.external_id
		OR ((%[1]s.external_id IS NULL OR %[2]s.external_id IS NULL)
		    AND lower(%[1]s.owner) = lower(%[2]s.owner) AND lower(%[1]s.repo) = lower(%[2]s.repo)))`, reg, m)
}

// reachableRegistryID resolves the live registry row of the entry aliased r
// under registryMatchesMirror's rule, "" when there is none. It is two
// subqueries rather than one OR so each is answered by a unique index
// (repositories_external_identity, then repositories_identity) and yields at
// most one row: the row carrying the entry's GitHub id, else the row under its
// folded name when either side has no id.
const reachableRegistryID = `COALESCE(
	(SELECT reg.id::text FROM repositories reg
	  WHERE reg.org_id = r.org_id AND reg.source = r.source AND reg.host = r.host
	    AND reg.retired_at IS NULL AND reg.external_id = r.external_id),
	(SELECT reg.id::text FROM repositories reg
	  WHERE reg.org_id = r.org_id AND reg.source = r.source AND reg.host = r.host
	    AND reg.retired_at IS NULL AND (reg.external_id IS NULL OR r.external_id IS NULL)
	    AND lower(reg.owner) = lower(r.owner) AND lower(reg.repo) = lower(r.repo)),
	'')`

// reachableColumns is the projection every row-returning read here shares, so a
// column added to the table lands in one place rather than in five SELECTs that
// then disagree about what a reachable row is.
const reachableColumns = `r.org_id, r.credential_class, COALESCE(r.installation_id, ''), r.host,
	r.source, r.owner, r.repo, r.external_id, ` + reachableRegistryID + `,
	r.description, r.language, r.html_url, r.pushed_at, r.private, r.observed_at`

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
func (s *reachableReposStore) ReplaceForInstallationSystem(ctx context.Context, orgID string, class domain.GitHubCredentialClass, githubHost, installationID string, repos []domain.ReachableRepository) error {
	if !isValidUUID(orgID) {
		return fmt.Errorf("replace reachable repositories: invalid org id %q", orgID)
	}
	if !class.AppTier() {
		return fmt.Errorf("replace reachable repositories: %q is not an installation-scoped credential class", class)
	}
	if installationID == "" {
		return fmt.Errorf("replace reachable repositories: empty installation id")
	}
	host := db.InstallationHostKey(githubHost)
	if host == "" {
		return fmt.Errorf("replace reachable repositories: empty host")
	}
	rows, err := normalizeReachable(repos, class, installationID, host)
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
			 WHERE org_id = $1 AND host = $2 AND installation_id = $3
		`, orgID, host, installationID); err != nil {
			return fmt.Errorf("clear reachable repositories: %w", err)
		}
		if err := insertReachable(ctx, tx, orgID, rows); err != nil {
			return err
		}
		return markScopeRefreshed(ctx, tx, orgID, class, host, installationID, observedAt(rows))
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
		return markScopeRefreshed(ctx, tx, orgID, domain.GitHubCredentialClassPAT, host, host, observedAt(rows))
	})
}

// ListReachWithoutPurposeSystem: granted, tracked by nobody. The NOT EXISTS
// pairs an entry with a tracked row through registryMatchesMirror: by GitHub id,
// so a repository renamed on GitHub before TF applied the rename still counts as
// tracked and a repository GitHub gave a tracked name to does not; and by
// case-folded name only for a side with no id, because GitHub identifiers are
// case-insensitive and a case-sensitive comparison would report a tracked
// repository as untracked purely because someone capitalized the slug
// differently — a fabricated security finding, which is worse than a missed one.
//
// Tracking crosses every team in the org, so the semi-join goes through teams
// on the admin pool: an org-level fact about the org's own App cannot be
// rendered from one viewer's team memberships.
func (s *reachableReposStore) ListReachWithoutPurposeSystem(ctx context.Context, orgID, host string, class domain.GitHubCredentialClass, opts db.ListOpts) ([]domain.ReachableRepository, int, error) {
	if err := db.RequireGrantClass("list reach without purpose", class); err != nil {
		return nil, 0, err
	}
	if err := db.RequireRepoHost(host); err != nil {
		return nil, 0, err
	}
	if !isValidUUID(orgID) {
		return []domain.ReachableRepository{}, 0, nil
	}
	from := `
		  FROM reachable_repositories r
		  JOIN org_github_app_installations i
		    ON i.org_id = r.org_id AND i.github_host = r.host AND i.installation_id = r.installation_id
		 WHERE r.org_id = $1
		   AND r.credential_class = $2
		   AND i.removed_at IS NULL
		   AND i.github_host = $3
		   AND NOT EXISTS (
		       SELECT 1
		         FROM team_github_repos g
		         JOIN teams t ON t.id = g.team_id
		         JOIN repositories reg ON reg.id = g.repository_id
		        WHERE t.org_id = r.org_id
		          AND reg.host = i.github_host AND reg.retired_at IS NULL
		          AND ` + registryMatchesMirror("reg", "r") + `)`
	args := []any{orgID, string(class), host}

	var total int
	if err := s.admin.QueryRowContext(ctx, `SELECT COUNT(*)`+from, args...).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("count reach without purpose: %w", err)
	}
	if opts.CountOnly {
		return []domain.ReachableRepository{}, total, nil
	}
	query := `SELECT ` + reachableColumns + from + ` ORDER BY r.installation_id, lower(r.owner), lower(r.repo)`
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
// The first NOT EXISTS asks the mirror for the repository through
// registryMatchesMirror, so a repository GitHub renamed before TF applied the
// rename is still found in its grant by id, and an entry under a tracked name
// with a different id does not stand in for the tracked one.
//
// The second NOT EXISTS is the three-way rule on the owner account's
// installation: a grant of every repository cannot be drifted out of, and a
// grant of unknown width cannot be said to have been. Only a selective grant, or
// no live installation on the account at all, leaves a tracked repository
// reportable.
//
// The owner account is matched to its installation by login, here and in the
// LEFT JOIN that names the installation. The registry records no account id
// for a repository's owner, and a drifted repository is by definition in no
// grant the mirror holds, so no id TF stores leads from it to an account.
func (s *reachableReposStore) ListScopeDriftSystem(ctx context.Context, orgID, host string, class domain.GitHubCredentialClass, opts db.ListOpts) ([]domain.ScopeDriftRepository, int, error) {
	if err := db.RequireGrantClass("list scope drift", class); err != nil {
		return nil, 0, err
	}
	if err := db.RequireRepoHost(host); err != nil {
		return nil, 0, err
	}
	if !isValidUUID(orgID) {
		return []domain.ScopeDriftRepository{}, 0, nil
	}
	// One finding per registry row, however many teams track it: the tracking
	// is an EXISTS rather than a join, so teams cannot multiply the rows, and
	// the GROUP BY collapses the installations the LEFT JOIN finds for one owner
	// login. After the rule below that can only be a selective one, and GitHub
	// allows one installation of an App per account, so MIN over the group is
	// the one id.
	from := `
		  FROM repositories reg
		  LEFT JOIN org_github_app_installations cov
		    ON cov.org_id = $1 AND cov.removed_at IS NULL AND cov.github_host = $4
		   AND lower(cov.account_login) = lower(reg.owner)
		 WHERE reg.org_id = $1
		   AND reg.host = $4 AND reg.retired_at IS NULL
		   AND EXISTS (
		       SELECT 1 FROM team_github_repos g
		         JOIN teams t ON t.id = g.team_id
		        WHERE g.repository_id = reg.id AND t.org_id = $1)
		   AND EXISTS (
		       SELECT 1 FROM reachable_scopes sc
		        WHERE sc.org_id = $1 AND sc.credential_class = $2 AND sc.host = $4)
		   AND NOT EXISTS (
		       SELECT 1
		         FROM reachable_repositories m
		         JOIN org_github_app_installations i
		           ON i.org_id = m.org_id AND i.github_host = m.host AND i.installation_id = m.installation_id
		        WHERE m.org_id = $1
		          AND m.credential_class = $2
		          AND i.removed_at IS NULL
		          AND i.github_host = $4
		          AND ` + registryMatchesMirror("reg", "m") + `)
		   AND NOT EXISTS (
		       SELECT 1 FROM org_github_app_installations w
		        WHERE w.org_id = $1 AND w.removed_at IS NULL AND w.github_host = $4
		          AND lower(w.account_login) = lower(reg.owner)
		          AND (w.repository_selection IS NULL OR w.repository_selection = $3))
		 GROUP BY reg.id, reg.owner, reg.repo, reg.external_id`
	args := []any{orgID, string(class), domain.RepositorySelectionAll, host}

	var total int
	if err := s.admin.QueryRowContext(ctx, `SELECT COUNT(*) FROM (SELECT 1`+from+`) AS drift`, args...).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("count scope drift: %w", err)
	}
	if opts.CountOnly {
		return []domain.ScopeDriftRepository{}, total, nil
	}
	query := `SELECT reg.id::text, reg.owner, reg.repo, COALESCE(reg.external_id, ''),
		COALESCE(MIN(cov.installation_id), '')` + from +
		` ORDER BY lower(reg.owner), lower(reg.repo), reg.id`
	if opts.Limit > 0 {
		args = append(args, opts.Limit)
		query += fmt.Sprintf(" LIMIT $%d", len(args))
		if opts.Offset > 0 {
			args = append(args, opts.Offset)
			query += fmt.Sprintf(" OFFSET $%d", len(args))
		}
	}
	rows, err := s.admin.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, 0, fmt.Errorf("read scope drift: %w", err)
	}
	defer rows.Close()
	out := []domain.ScopeDriftRepository{}
	for rows.Next() {
		var r domain.ScopeDriftRepository
		if err := rows.Scan(&r.RepositoryID, &r.Owner, &r.Repo, &r.ExternalID, &r.InstallationID); err != nil {
			return nil, 0, fmt.Errorf("scan scope drift: %w", err)
		}
		out = append(out, r)
	}
	return out, total, rows.Err()
}

func (s *reachableReposStore) ListReachableSystem(ctx context.Context, orgID, host string, class domain.GitHubCredentialClass, q string, opts db.ListOpts) ([]domain.ReachableRepository, int, error) {
	if !class.Known() {
		return nil, 0, fmt.Errorf("list reachable repositories: unknown credential class %q", class)
	}
	if err := db.RequireRepoHost(host); err != nil {
		return nil, 0, err
	}
	if !isValidUUID(orgID) {
		return []domain.ReachableRepository{}, 0, nil
	}
	where := ` WHERE r.org_id = $1 AND r.credential_class = $2 AND ` + reachableOnHost("r", "$3")
	args := []any{orgID, string(class), host}
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

func (s *reachableReposStore) ReachableStateSystem(ctx context.Context, orgID, host string, class domain.GitHubCredentialClass) (domain.ReachableCacheState, error) {
	if !class.Known() {
		return domain.ReachableCacheState{}, fmt.Errorf("read reachable cache state: unknown credential class %q", class)
	}
	if err := db.RequireRepoHost(host); err != nil {
		return domain.ReachableCacheState{}, err
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
		SELECT COUNT(*), MIN(sc.refreshed_at)
		  FROM reachable_scopes sc
		 WHERE sc.org_id = $1 AND sc.credential_class = $2 AND sc.host = $3
	`, orgID, string(class), host).Scan(&scopes, &refreshed); err != nil {
		return domain.ReachableCacheState{}, fmt.Errorf("read reachable scope state: %w", err)
	}
	var count int
	if err := s.admin.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM reachable_repositories r
		 WHERE r.org_id = $1 AND r.credential_class = $2 AND `+reachableOnHost("r", "$3")+`
	`, orgID, string(class), host).Scan(&count); err != nil {
		return domain.ReachableCacheState{}, fmt.Errorf("read reachable cache state: %w", err)
	}
	return domain.ReachableCacheState{Refreshed: scopes > 0, Count: count, ObservedAt: refreshed.Time}, nil
}

func (s *reachableReposStore) ReachableSlugsSystem(ctx context.Context, orgID, host string, class domain.GitHubCredentialClass, slugs []string) (map[string]struct{}, error) {
	if !class.Known() {
		return nil, fmt.Errorf("read reachable slugs: unknown credential class %q", class)
	}
	if err := db.RequireRepoHost(host); err != nil {
		return nil, err
	}
	out := map[string]struct{}{}
	if !isValidUUID(orgID) || len(slugs) == 0 {
		return out, nil
	}
	// An IN list of numbered placeholders rather than a text[] literal: the
	// slugs originate in a request body, and there is no array-literal helper in
	// this package that quotes them — one bound parameter per slug cannot be
	// mis-escaped at all. The list is bounded by the caller's selection.
	args := []any{orgID, string(class), host}
	placeholders := make([]string, 0, len(slugs))
	for _, slug := range slugs {
		args = append(args, strings.ToLower(slug))
		placeholders = append(placeholders, fmt.Sprintf("$%d", len(args)))
	}
	rows, err := s.admin.QueryContext(ctx, `
		SELECT lower(r.owner || '/' || r.repo)
		  FROM reachable_repositories r
		 WHERE r.org_id = $1 AND r.credential_class = $2 AND `+reachableOnHost("r", "$3")+`
		   AND lower(r.owner || '/' || r.repo) IN (`+strings.Join(placeholders, ",")+`)
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

func (s *reachableReposStore) ReachableByExternalIDsSystem(ctx context.Context, orgID, host string, class domain.GitHubCredentialClass, externalIDs []string) (map[string]domain.ReachableRepository, error) {
	if !class.Known() {
		return nil, fmt.Errorf("read reachable repositories by id: unknown credential class %q", class)
	}
	if err := db.RequireRepoHost(host); err != nil {
		return nil, err
	}
	out := map[string]domain.ReachableRepository{}
	if !isValidUUID(orgID) || len(externalIDs) == 0 {
		return out, nil
	}
	// One bound parameter per id, for the reason ReachableSlugsSystem gives:
	// the ids originate in a request body.
	args := []any{orgID, string(class), host}
	placeholders := make([]string, 0, len(externalIDs))
	for _, id := range externalIDs {
		args = append(args, id)
		placeholders = append(placeholders, fmt.Sprintf("$%d", len(args)))
	}
	rows, err := scanReachableRepos(s.admin.QueryContext(ctx, `
		SELECT `+reachableColumns+`
		  FROM reachable_repositories r
		 WHERE r.org_id = $1 AND r.credential_class = $2 AND `+reachableOnHost("r", "$3")+`
		   AND r.external_id IN (`+strings.Join(placeholders, ",")+`)
		 ORDER BY r.observed_at DESC, lower(r.owner), lower(r.repo)
	`, args...))
	if err != nil {
		return nil, err
	}
	for _, r := range rows {
		if _, seen := out[r.ExternalID]; !seen {
			out[r.ExternalID] = r
		}
	}
	return out, nil
}

func (s *reachableReposStore) ListTrackedInGrantsSystem(ctx context.Context, orgID, host string, installationIDs []string) ([]string, error) {
	if err := db.RequireRepoHost(host); err != nil {
		return nil, err
	}
	out := []string{}
	if !isValidUUID(orgID) || len(installationIDs) == 0 {
		return out, nil
	}
	args := []any{orgID, host}
	placeholders := make([]string, 0, len(installationIDs))
	for _, id := range installationIDs {
		args = append(args, id)
		placeholders = append(placeholders, fmt.Sprintf("$%d", len(args)))
	}
	rows, err := s.admin.QueryContext(ctx, `
		SELECT reg.owner || '/' || reg.repo
		  FROM repositories reg
		 WHERE reg.org_id = $1 AND reg.host = $2 AND reg.retired_at IS NULL
		   AND EXISTS (
		       SELECT 1 FROM team_github_repos g
		         JOIN teams t ON t.id = g.team_id
		        WHERE g.repository_id = reg.id AND t.org_id = $1)
		   AND EXISTS (
		       SELECT 1 FROM reachable_repositories m
		        WHERE m.org_id = $1 AND m.host = $2
		          AND m.installation_id IN (`+strings.Join(placeholders, ",")+`)
		          AND `+registryMatchesMirror("reg", "m")+`)
		 ORDER BY lower(reg.owner), lower(reg.repo), reg.id
	`, args...)
	if err != nil {
		return nil, fmt.Errorf("read tracked repositories in grants: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, fmt.Errorf("scan tracked repository in grants: %w", err)
		}
		out = append(out, name)
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

// insertReachable writes one replace's rows. A PAT row has no installation, and
// its installation_id goes in as NULL rather than as an empty string that would
// satisfy the table's CHECK while meaning nothing.
func insertReachable(ctx context.Context, tx queryer, orgID string, rows []domain.ReachableRepository) error {
	for _, r := range rows {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO reachable_repositories
				(org_id, credential_class, installation_id, host, source, owner, repo,
				 external_id, description, language, html_url, pushed_at, private, observed_at)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14)
			ON CONFLICT DO NOTHING
		`, orgID, string(r.CredentialClass), nullString(r.InstallationID), r.Host,
			r.Source, r.Owner, r.Repo, nullString(r.ExternalID),
			r.Description, r.Language, r.HTMLURL, r.PushedAt, r.Private, r.ObservedAt); err != nil {
			return fmt.Errorf("insert reachable repository %s: %w", r.Slug(), err)
		}
	}
	return nil
}

// markScopeRefreshed records that this scope's reach on host was just
// established. It is what makes "we have looked and found nothing"
// distinguishable from "we have not looked", which no count of repository rows
// can express.
func markScopeRefreshed(ctx context.Context, tx queryer, orgID string, class domain.GitHubCredentialClass, host, scope string, at time.Time) error {
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO reachable_scopes (org_id, credential_class, host, scope, refreshed_at)
		VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT (org_id, credential_class, host, scope) DO UPDATE SET refreshed_at = excluded.refreshed_at
	`, orgID, string(class), host, scope, at); err != nil {
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
			&externalID, &r.RepositoryID, &r.Description, &r.Language, &r.HTMLURL, &r.PushedAt, &r.Private, &r.ObservedAt); err != nil {
			return nil, fmt.Errorf("scan reachable_repositories: %w", err)
		}
		r.CredentialClass = domain.GitHubCredentialClass(class)
		r.ExternalID = externalID.String
		out = append(out, r)
	}
	return out, rows.Err()
}
