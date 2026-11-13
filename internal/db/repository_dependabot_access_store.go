package db

import (
	"context"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// RepositoryDependabotAccessStore owns repository_dependabot_access: per
// repository, whether the org's GitHub credential can read its Dependabot
// alerts, as the poll's probe last found it, and the ETag of the poll's last
// listing of those alerts. A repository the poll has not probed has no row.
//
// Every method is a system method on the admin pool in Postgres, org_id bound
// by argument: the poll runs with no claims, and the one request-path reader
// is the org-admin settings count, which the handler's admin gate authorizes.
// SQLite collapses to the one connection.
type RepositoryDependabotAccessStore interface {
	// RecordSystem stores access as the answer for the repository whose
	// registry row is repositoryID, checked now, replacing any earlier answer.
	// Returns the row as stored. access must be one of the three answers, and
	// the repository must be a registry row of orgID.
	RecordSystem(ctx context.Context, orgID, repositoryID string, access domain.DependabotAlertsAccess) (domain.RepositoryDependabotAccess, error)

	// GetSystem reads one repository's answer, or (nil, nil) when none is
	// recorded.
	GetSystem(ctx context.Context, orgID, repositoryID string) (*domain.RepositoryDependabotAccess, error)

	// ListSystem returns the answer recorded for every repository of the org
	// on host that holds its name (retired rows left out), ordered by
	// repository id.
	ListSystem(ctx context.Context, orgID, host string) ([]domain.RepositoryDependabotAccess, error)

	// RecordAlertListingSystem stores etag as the condition of the next
	// listing of the repository's open alerts ("" for none). The answer and
	// when it was checked are left alone. Returns the row as stored, or
	// sql.ErrNoRows when the repository has no answer recorded: the poll lists
	// only a repository whose answer is readable.
	RecordAlertListingSystem(ctx context.Context, orgID, repositoryID, etag string) (domain.RepositoryDependabotAccess, error)

	// ClearUntrackedAlertListingsSystem clears the stored ETag of every
	// repository of the org that no team tracks on host, the org's GitHub
	// host, retired ones included, so the listing that follows a team
	// tracking it again is answered in full and diffed against the packages
	// TF holds. A 304 would say nothing changed since a listing whose packages
	// TF may since have closed without listing it: the poll retires the
	// packages on another host when the org moves, and a retired repository's
	// entities close. Returns how many rows it changed. Exempt from the
	// returned-row rule: a bulk write the poll makes every cycle, whose count
	// is for its log line.
	ClearUntrackedAlertListingsSystem(ctx context.Context, orgID, host string) (int, error)

	// CountTrackedSystem counts the repositories on host that a team of the
	// org tracks, by their answer. A tracked repository with no answer, or
	// with one checked before checkedSince, counts as unchecked. Retired rows
	// and archived teams are left out, as in RepositoryStore.ListTrackedSystem.
	CountTrackedSystem(ctx context.Context, orgID, host string, checkedSince time.Time) (domain.DependabotAccessCounts, error)
}
