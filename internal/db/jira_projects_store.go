package db

import (
	"context"
	"database/sql"
	"errors"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// ErrNoSuchJiraProject is returned when a registry row id names no row in the
// org.
var ErrNoSuchJiraProject = errors.New("no such jira project")

// ErrJiraProjectIdentityConflict is returned when a refresh names a Jira
// project id other than the one the row already carries. A row's id is the
// project's identity, so a different answer is a different project, never a
// refresh of this one.
var ErrJiraProjectIdentityConflict = errors.New("jira project answered with another project id")

// JiraProjectStore owns the jira_projects registry: one row per Jira project on
// one Jira site, the target every stored reference to a Jira project points at
// (domain.JiraProject). A team's tracked set references a row by its id
// (JiraStatusRulesStore), and so does every Jira issue entity
// (entities.jira_project_id), so a project key rename is a write to the row's
// key and nothing else.
//
// The registry is therefore every project TF records an issue in, not the
// tracked set: an issue an agent touched in a project no team tracks
// registers its project too. What a team tracks is its rules.
//
// Rows are durable. Untracking a project deletes its rules and leaves the row,
// and nothing here deletes one except the merge RefreshSystem performs.
//
// # Pool split (Postgres)
//
//   - FindOrCreate runs on the app pool, inside the team settings write that
//     adds the project. jira_projects_all gates it to the caller's org; the
//     handler gates it to a team admin adding the project to that team.
//   - FindOrCreateSystem, RefreshSystem and GetSystem run on the admin pool,
//     for the poller and the tracker. A merge rewrites the tracked sets of
//     teams the caller may not belong to.
//
// SQLite collapses the split to one connection.
type JiraProjectStore interface {
	// GetSystem returns the registry row id names in the org, or nil when it
	// names none. Admin pool.
	GetSystem(ctx context.Context, orgID, id string) (*domain.JiraProject, error)

	// FindOrCreate returns the registry row for the project Jira identifies as
	// externalID on site, creating it when none exists, with key and name
	// written as its display values. The row is found by externalID, so a row
	// created under the project's old key answers under the new one. When no
	// row carries externalID but an unresolved row on site holds key, that row
	// is the project and is resolved in place. Every argument is required.
	FindOrCreate(ctx context.Context, orgID, site, externalID, key, name string) (domain.JiraProject, error)

	// FindOrCreateSystem is FindOrCreate on the admin pool: the tracker
	// registers the project of every issue it reads before writing the
	// issue's snapshot, which is what moves the issue's entity onto the
	// project's row (EntityStore.UpdateSnapshotCASSystem). name may be empty,
	// which keeps a found row's name. A found row whose key and name already
	// match is returned without a write.
	FindOrCreateSystem(ctx context.Context, orgID, site, externalID, key, name string) (domain.JiraProject, error)

	// RefreshSystem writes what Jira answered for the registry row id: its
	// project id, key and name. The row is the one returned, unless it was
	// unresolved and another row on its site already carries externalID: the
	// two are one project, so this row is merged into that one — its rules
	// move to that row (a team holding both keeps the row it is merged into),
	// the team settings that list it list that row instead, the issue
	// entities that reference it reference that row, and the row is deleted.
	// The surviving row is returned either way.
	//
	// ErrNoSuchJiraProject when id names no row in the org;
	// ErrJiraProjectIdentityConflict when the row is resolved to a different
	// project id. Admin pool.
	RefreshSystem(ctx context.Context, orgID, id, externalID, key, name string) (domain.JiraProject, error)
}

// JiraProjectColumns is the projection ScanJiraProject reads, in its order — the
// list every jira_projects read and RETURNING clause shares in both dialects.
const JiraProjectColumns = `id, org_id, site, COALESCE(external_id, ''), project_key, project_name`

// ScanJiraProject reads one row projected by JiraProjectColumns.
func ScanJiraProject(row *sql.Row) (domain.JiraProject, error) {
	var p domain.JiraProject
	err := row.Scan(&p.ID, &p.OrgID, &p.Site, &p.ExternalID, &p.Key, &p.Name)
	return p, err
}
