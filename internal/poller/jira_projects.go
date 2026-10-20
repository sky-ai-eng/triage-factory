package poller

import (
	"context"
	"errors"
	"log/slog"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	jiraclient "github.com/sky-ai-eng/triage-factory/internal/jira"
	"github.com/sky-ai-eng/triage-factory/internal/upstream"
)

// syncJiraProjects brings the registry rows of the projects this cycle polls
// up to date with Jira, and reports whether it wrote any. It runs before the
// tracker reads a single issue, so a project key renamed since the last cycle
// is already the registry's key when the cycle's rules are built, and every
// rule read by key — the tracker's terminal check, the router gate, the
// deck — finds the rule under the key the issues now carry.
//
// A resolved row is read by Jira's project id, which answers with the key the
// project has now; a row not yet resolved is read by its key, and the answer
// resolves it (merging it into the row that already carries the id, when one
// does).
//
// One request per armed project per cycle, and only for the projects the
// cycle is about to query anyway. Best-effort like reporename.Apply: a project
// Jira will not describe is left as stored, and the discovery that follows
// reports whatever is actually wrong with it.
func (m *Manager) syncJiraProjects(ctx context.Context, client *jiraclient.Client, orgID string, rules []domain.JiraProjectStatusRules) bool {
	if m.jiraProjects == nil {
		return false
	}
	seen := map[string]bool{}
	wrote := false
	for _, r := range rules {
		if !r.Armed() || seen[r.JiraProjectID] {
			continue
		}
		seen[r.JiraProjectID] = true
		ref := r.ProjectExternalID
		if ref == "" {
			ref = r.ProjectKey
		}
		p, err := client.GetProject(ctx, ref)
		if err != nil {
			jiraLog.Log(ctx, upstream.LogLevel(err, slog.LevelWarn), "read jira project for the registry failed; keeping the stored row",
				"org", orgID, "project", r.ProjectKey, "jira_project_id", r.JiraProjectID, "error", err)
			continue
		}
		if p.ID == "" || p.Key == "" {
			jiraLog.WarnContext(ctx, "jira answered a project read without an id or a key; keeping the stored row",
				"org", orgID, "project", r.ProjectKey, "jira_project_id", r.JiraProjectID)
			continue
		}
		if p.ID == r.ProjectExternalID && p.Key == r.ProjectKey && p.Name == r.ProjectName {
			continue
		}
		row, err := m.jiraProjects.RefreshSystem(ctx, orgID, r.JiraProjectID, p.ID, p.Key, p.Name)
		switch {
		case errors.Is(err, db.ErrNoSuchJiraProject):
			// A merge earlier this cycle, or a settings save since the rules
			// were read, removed the row; the next cycle reads it fresh.
			continue
		case err != nil:
			jiraLog.ErrorContext(ctx, "refresh jira project registry row failed",
				"org", orgID, "project", r.ProjectKey, "jira_project_id", r.JiraProjectID, "error", err)
			continue
		}
		wrote = true
		switch {
		case row.ID != r.JiraProjectID:
			jiraLog.InfoContext(ctx, "jira project registered twice; merged into the row carrying its id",
				"org", orgID, "from", r.JiraProjectID, "into", row.ID, "project", row.Key)
		case r.ProjectExternalID != "" && row.Key != r.ProjectKey:
			jiraLog.InfoContext(ctx, "jira project key changed; registry row renamed",
				"org", orgID, "jira_project_id", row.ID, "from", r.ProjectKey, "to", row.Key)
		}
	}
	return wrote
}
