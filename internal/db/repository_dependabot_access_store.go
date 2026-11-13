package db

import (
	"context"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// RepositoryDependabotAccessStore owns repository_dependabot_access: per
// repository, whether the org's GitHub credential can read its Dependabot
// alerts, as the poll's probe last found it. A repository the poll has not
// probed has no row.
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

	// CountTrackedSystem counts the repositories on host that a team of the
	// org tracks, by their answer; a tracked repository with no answer counts
	// as unchecked. Retired rows and archived teams are left out, as in
	// RepositoryStore.ListTrackedSystem.
	CountTrackedSystem(ctx context.Context, orgID, host string) (domain.DependabotAccessCounts, error)
}
