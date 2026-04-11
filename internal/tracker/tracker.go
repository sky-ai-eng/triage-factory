package tracker

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/sky-ai-eng/todo-triage/internal/db"
	"github.com/sky-ai-eng/todo-triage/internal/domain"
	"github.com/sky-ai-eng/todo-triage/internal/eventbus"
	ghclient "github.com/sky-ai-eng/todo-triage/internal/github"
	jiraclient "github.com/sky-ai-eng/todo-triage/internal/jira"
)

const (
	jiraBatchSize = 100 // max issues per JQL key IN (...) query
)

// Tracker manages the discover → refresh → diff → emit cycle for both GitHub and Jira.
type Tracker struct {
	database *sql.DB
	bus      *eventbus.Bus
}

// New creates a Tracker.
func New(database *sql.DB, bus *eventbus.Bus) *Tracker {
	return &Tracker{database: database, bus: bus}
}

// --- GitHub ---

// RefreshGitHub runs the full tracking cycle for GitHub PRs.
func (t *Tracker) RefreshGitHub(client *ghclient.Client, username string, repos []string) (int, error) {
	// Phase 1: Discovery
	discovered, err := t.discoverGitHub(client, username, repos)
	if err != nil {
		log.Printf("[tracker] GitHub discovery error: %v", err)
	}

	// Register newly discovered items and upsert tasks
	for _, d := range discovered {
		task := prSnapshotToTask(d.Snapshot, username)
		if err := db.UpsertTask(t.database, task); err != nil {
			log.Printf("[tracker] error upserting task for PR #%d: %v", d.Snapshot.Number, err)
			continue
		}
		sid := ghSourceID(d.Snapshot.Repo, d.Snapshot.Number)
		taskID := t.resolveTaskID("github", sid)

		item := domain.TrackedItem{
			Source:   "github",
			SourceID: sid,
			TaskID:   taskID,
			NodeID:   d.NodeID,
		}
		if err := db.UpsertTrackedItem(t.database, item); err != nil {
			log.Printf("[tracker] error registering tracked item %s: %v", sid, err)
		}

		// Reactivate if a previously-terminal item is now open (e.g., reopened PR)
		if !d.Snapshot.Merged && d.Snapshot.State != "CLOSED" {
			if reactivated, err := db.ReactivateTrackedItem(t.database, "github", sid); err != nil {
				log.Printf("[tracker] error reactivating %s: %v", sid, err)
			} else if reactivated {
				log.Printf("[tracker] reactivated %s (reopened)", sid)
			}
		}
	}

	// Phase 2: Refresh all tracked items via GraphQL batch
	nodeIDs, err := db.ListNodeIDs(t.database, "github")
	if err != nil {
		return 0, fmt.Errorf("list node IDs: %w", err)
	}
	if len(nodeIDs) == 0 {
		return 0, nil
	}

	refreshed, err := client.RefreshPRs(nodeIDs)
	if err != nil {
		return 0, fmt.Errorf("refresh PRs: %w", err)
	}

	// Phase 3: Diff, upsert tasks, and emit events
	tracked, err := db.ListActiveTrackedItems(t.database, "github")
	if err != nil {
		return 0, fmt.Errorf("list tracked items: %w", err)
	}

	eventsEmitted := 0
	for _, item := range tracked {
		if item.NodeID == "" {
			continue
		}

		newSnap, ok := refreshed[item.NodeID]
		if !ok {
			log.Printf("[tracker] tracked item %s not returned by refresh, skipping", item.SourceID)
			continue
		}

		// Update the task with fresh snapshot data
		task := prSnapshotToTask(newSnap, username)
		if err := db.UpsertTask(t.database, task); err != nil {
			log.Printf("[tracker] error upserting task for PR #%d: %v", newSnap.Number, err)
		}

		// Ensure task_id is linked
		if item.TaskID == "" {
			item.TaskID = t.resolveTaskID("github", item.SourceID)
		}

		// Parse previous snapshot and diff
		var prevSnap domain.PRSnapshot
		if item.Snapshot != "" && item.Snapshot != "{}" {
			if err := json.Unmarshal([]byte(item.Snapshot), &prevSnap); err != nil {
				log.Printf("[tracker] corrupt snapshot for %s, skipping diff: %v", item.SourceID, err)
				// Overwrite with fresh state so next cycle can diff cleanly
				snapJSON, _ := json.Marshal(newSnap)
				if err := db.UpdateTrackedSnapshot(t.database, "github", item.SourceID, string(snapJSON)); err != nil {
					log.Printf("[tracker] failed to rewrite corrupt snapshot for %s: %v", item.SourceID, err)
				}
				continue
			}
		}

		events := DiffPRSnapshots(prevSnap, newSnap, item.SourceID, username)

		// Persist new snapshot
		snapJSON, _ := json.Marshal(newSnap)
		if err := db.UpdateTrackedSnapshot(t.database, "github", item.SourceID, string(snapJSON)); err != nil {
			log.Printf("[tracker] error updating snapshot for %s: %v", item.SourceID, err)
		}

		// Record and publish events
		for _, evt := range events {
			evt.TaskID = item.TaskID
			if id, err := db.RecordEvent(t.database, evt); err != nil {
				log.Printf("[tracker] error recording event: %v", err)
			} else {
				evt.ID = id
			}
			if item.TaskID != "" {
				if err := db.SetTaskEventType(t.database, item.TaskID, evt.EventType); err != nil {
					log.Printf("[tracker] failed to set event type for task %s: %v", item.TaskID, err)
				}
			}
			t.bus.Publish(evt)
			eventsEmitted++
		}

		// Sync task status when PR state changes
		if item.TaskID != "" && prevSnap.State != newSnap.State {
			newTaskStatus := prStateToTaskStatus(newSnap)
			if _, err := t.database.Exec(`UPDATE tasks SET status = ? WHERE id = ? AND status != ?`, newTaskStatus, item.TaskID, newTaskStatus); err != nil {
				log.Printf("[tracker] failed to sync task status for %s: %v", item.TaskID, err)
			}
		}

		if newSnap.Merged || newSnap.State == "CLOSED" {
			if err := db.MarkTerminal(t.database, "github", item.SourceID); err != nil {
				log.Printf("[tracker] failed to mark github/%s terminal: %v", item.SourceID, err)
			}
		}
	}

	log.Printf("[tracker] GitHub refresh: %d discovered, %d tracked, %d refreshed, %d events",
		len(discovered), len(tracked), len(refreshed), eventsEmitted)

	// Emit poll-complete sentinel
	if len(tracked) > 0 {
		t.EmitPollComplete("github", len(tracked), eventsEmitted)
	}

	return eventsEmitted, nil
}

// maxSearchQueryLen is GitHub's limit for the q= search parameter.
const maxSearchQueryLen = 256

// discoverGitHub runs search queries to find new PRs.
func (t *Tracker) discoverGitHub(client *ghclient.Client, username string, repos []string) ([]ghclient.DiscoveredPR, error) {
	since := time.Now().AddDate(0, 0, -30).Format("2006-01-02")
	bases := []string{
		// Active / actionable
		fmt.Sprintf("is:pr is:open review-requested:%s", username),
		fmt.Sprintf("is:pr is:open author:%s", username),
		fmt.Sprintf("is:pr is:open mentions:%s", username),
		// Active PRs you've reviewed (may still need attention)
		fmt.Sprintf("is:pr is:open reviewed-by:%s", username),
		// Backfill for dashboard — merged/closed in last 30 days
		fmt.Sprintf("is:pr is:merged author:%s merged:>=%s", username, since),
		fmt.Sprintf("is:pr is:merged reviewed-by:%s merged:>=%s", username, since),
		fmt.Sprintf("is:pr is:closed is:unmerged author:%s closed:>=%s", username, since),
		fmt.Sprintf("is:pr is:closed is:unmerged reviewed-by:%s closed:>=%s", username, since),
	}

	var queries []string
	for _, base := range bases {
		queries = append(queries, scopedQueries(base, repos)...)
	}

	seen := map[string]bool{}
	var all []ghclient.DiscoveredPR

	for _, q := range queries {
		prs, err := client.DiscoverPRs(q, 50)
		if err != nil {
			log.Printf("[tracker] discovery query failed: %v (query: %s)", err, q)
			continue
		}
		for _, pr := range prs {
			sid := ghSourceID(pr.Snapshot.Repo, pr.Snapshot.Number)
			if !seen[sid] {
				seen[sid] = true
				all = append(all, pr)
			}
		}
	}

	return all, nil
}

// --- Jira ---

// RefreshJira runs the full tracking cycle for Jira issues.
func (t *Tracker) RefreshJira(client *jiraclient.Client, baseURL string, projects, pickupStatuses []string) (int, error) {
	// Phase 1: Discovery
	discovered, err := t.discoverJira(client, baseURL, projects, pickupStatuses)
	if err != nil {
		log.Printf("[tracker] Jira discovery error: %v", err)
	}

	// Register newly discovered items and upsert tasks
	for _, snap := range discovered {
		task := jiraSnapshotToTask(snap, baseURL)
		if err := db.UpsertTask(t.database, task); err != nil {
			log.Printf("[tracker] error upserting task for %s: %v", snap.Key, err)
			continue
		}
		taskID := t.resolveTaskID("jira", snap.Key)

		item := domain.TrackedItem{
			Source:   "jira",
			SourceID: snap.Key,
			TaskID:   taskID,
		}
		if err := db.UpsertTrackedItem(t.database, item); err != nil {
			log.Printf("[tracker] error registering tracked item %s: %v", snap.Key, err)
		}

		// Reactivate if a previously-terminal item is now active (e.g., reopened issue)
		if !isJiraTerminal(snap.Status) {
			if reactivated, err := db.ReactivateTrackedItem(t.database, "jira", snap.Key); err != nil {
				log.Printf("[tracker] error reactivating %s: %v", snap.Key, err)
			} else if reactivated {
				log.Printf("[tracker] reactivated %s (reopened)", snap.Key)
			}
		}
	}

	// Phase 2: Refresh
	tracked, err := db.ListActiveTrackedItems(t.database, "jira")
	if err != nil {
		return 0, fmt.Errorf("list tracked jira items: %w", err)
	}
	if len(tracked) == 0 {
		return 0, nil
	}

	keys := make([]string, len(tracked))
	for i, item := range tracked {
		keys[i] = item.SourceID
	}

	refreshed, err := t.batchFetchJira(client, baseURL, keys)
	if err != nil {
		return 0, fmt.Errorf("batch fetch jira: %w", err)
	}

	// Phase 3: Diff, upsert tasks, and emit events
	eventsEmitted := 0
	for _, item := range tracked {
		newSnap, ok := refreshed[item.SourceID]
		if !ok {
			log.Printf("[tracker] tracked Jira item %s not returned by refresh, skipping", item.SourceID)
			continue
		}

		// Update task with fresh data
		task := jiraSnapshotToTask(newSnap, baseURL)
		if err := db.UpsertTask(t.database, task); err != nil {
			log.Printf("[tracker] error upserting task for %s: %v", newSnap.Key, err)
		}

		if item.TaskID == "" {
			item.TaskID = t.resolveTaskID("jira", item.SourceID)
		}

		var prevSnap domain.JiraSnapshot
		if item.Snapshot != "" && item.Snapshot != "{}" {
			if err := json.Unmarshal([]byte(item.Snapshot), &prevSnap); err != nil {
				log.Printf("[tracker] corrupt snapshot for %s, skipping diff: %v", item.SourceID, err)
				snapJSON, _ := json.Marshal(newSnap)
				if err := db.UpdateTrackedSnapshot(t.database, "jira", item.SourceID, string(snapJSON)); err != nil {
					log.Printf("[tracker] failed to rewrite corrupt snapshot for %s: %v", item.SourceID, err)
				}
				continue
			}
		}

		events := DiffJiraSnapshots(prevSnap, newSnap, item.SourceID)

		// Sync task status when the source status changes
		if item.TaskID != "" && (prevSnap.Status != newSnap.Status || prevSnap.Assignee != newSnap.Assignee) {
			newTaskStatus := jiraStatusToTaskStatus(newSnap)
			if _, err := t.database.Exec(`UPDATE tasks SET status = ? WHERE id = ? AND status != ?`, newTaskStatus, item.TaskID, newTaskStatus); err != nil {
				log.Printf("[tracker] failed to sync task status for %s: %v", item.TaskID, err)
			}
		}

		snapJSON, _ := json.Marshal(newSnap)
		if err := db.UpdateTrackedSnapshot(t.database, "jira", item.SourceID, string(snapJSON)); err != nil {
			log.Printf("[tracker] error updating snapshot for %s: %v", item.SourceID, err)
		}

		for _, evt := range events {
			evt.TaskID = item.TaskID
			if id, err := db.RecordEvent(t.database, evt); err != nil {
				log.Printf("[tracker] error recording event: %v", err)
			} else {
				evt.ID = id
			}
			if item.TaskID != "" {
				if err := db.SetTaskEventType(t.database, item.TaskID, evt.EventType); err != nil {
					log.Printf("[tracker] failed to set event type for task %s: %v", item.TaskID, err)
				}
			}
			t.bus.Publish(evt)
			eventsEmitted++
		}

		if isJiraTerminal(newSnap.Status) {
			if err := db.MarkTerminal(t.database, "jira", item.SourceID); err != nil {
				log.Printf("[tracker] failed to mark jira/%s terminal: %v", item.SourceID, err)
			}
		}
	}

	log.Printf("[tracker] Jira refresh: %d discovered, %d tracked, %d refreshed, %d events",
		len(discovered), len(tracked), len(refreshed), eventsEmitted)

	if len(tracked) > 0 {
		t.EmitPollComplete("jira", len(tracked), eventsEmitted)
	}

	return eventsEmitted, nil
}

// discoverJira runs JQL queries to find new issues.
func (t *Tracker) discoverJira(client *jiraclient.Client, baseURL string, projects, pickupStatuses []string) ([]domain.JiraSnapshot, error) {
	if len(projects) == 0 {
		return nil, nil
	}

	projectList := strings.Join(projects, ", ")
	var queries []string

	if len(pickupStatuses) > 0 {
		quoted := make([]string, len(pickupStatuses))
		for i, s := range pickupStatuses {
			quoted[i] = fmt.Sprintf("%q", s)
		}
		queries = append(queries, fmt.Sprintf(
			`project IN (%s) AND status IN (%s) AND assignee IS EMPTY`, projectList, strings.Join(quoted, ", ")))
	}

	queries = append(queries, fmt.Sprintf(
		`project IN (%s) AND assignee = currentUser() AND status NOT IN (Done, Closed, Resolved)`, projectList))

	seen := map[string]bool{}
	var all []domain.JiraSnapshot

	fields := []string{"summary", "status", "assignee", "priority", "labels", "issuetype", "parent", "comment"}

	for _, jql := range queries {
		issues, err := client.SearchIssues(jql, fields, 100)
		if err != nil {
			log.Printf("[tracker] Jira discovery query failed: %v", err)
			continue
		}
		for _, issue := range issues {
			if !seen[issue.Key] {
				seen[issue.Key] = true
				all = append(all, issueToSnapshot(issue, baseURL))
			}
		}
	}

	return all, nil
}

// batchFetchJira fetches current state for tracked Jira issues.
func (t *Tracker) batchFetchJira(client *jiraclient.Client, baseURL string, keys []string) (map[string]domain.JiraSnapshot, error) {
	results := make(map[string]domain.JiraSnapshot, len(keys))
	fields := []string{"summary", "status", "assignee", "priority", "labels", "issuetype", "parent", "comment"}

	for i := 0; i < len(keys); i += jiraBatchSize {
		end := i + jiraBatchSize
		if end > len(keys) {
			end = len(keys)
		}
		batch := keys[i:end]

		jql := fmt.Sprintf("key IN (%s)", strings.Join(batch, ", "))
		issues, err := client.SearchIssues(jql, fields, jiraBatchSize)
		if err != nil {
			return nil, fmt.Errorf("batch fetch keys %d-%d: %w", i, end, err)
		}

		for _, issue := range issues {
			results[issue.Key] = issueToSnapshot(issue, baseURL)
		}
	}

	return results, nil
}

// --- Snapshot → Task converters ---

// prSnapshotToTask builds a Task from a PR snapshot.
func prSnapshotToTask(snap domain.PRSnapshot, username string) domain.Task {
	status := "queued"
	if snap.Merged || snap.State == "CLOSED" {
		status = "done"
	}

	// Determine relevance reason
	reason := "authored"
	if snap.Author != username {
		reason = "mentioned"
		for _, rr := range snap.ReviewRequests {
			if rr == username {
				reason = "review_requested"
				break
			}
		}
		if reason == "mentioned" {
			for _, r := range snap.Reviews {
				if r.Author == username {
					reason = "reviewed"
					break
				}
			}
		}
	}

	ciStatus := ""
	switch snap.CIState {
	case "SUCCESS":
		ciStatus = "success"
	case "FAILURE", "ERROR":
		ciStatus = "failure"
	case "PENDING", "EXPECTED":
		ciStatus = "pending"
	}

	return domain.Task{
		ID:              uuid.New().String(),
		Source:          "github",
		SourceID:        ghSourceID(snap.Repo, snap.Number),
		SourceURL:       snap.URL,
		Title:           snap.Title,
		Repo:            snap.Repo,
		Author:          snap.Author,
		Labels:          snap.Labels,
		DiffAdditions:   snap.Additions,
		DiffDeletions:   snap.Deletions,
		FilesChanged:    snap.ChangedFiles,
		CIStatus:        ciStatus,
		RelevanceReason: reason,
		CreatedAt:       parseTimeOrNow(snap.CreatedAt),
		FetchedAt:       time.Now(),
		Status:          status,
	}
}

// jiraSnapshotToTask builds a Task from a Jira snapshot.
func jiraSnapshotToTask(snap domain.JiraSnapshot, baseURL string) domain.Task {
	status := "queued"
	reason := "available"
	if snap.Assignee != "" {
		status = "claimed"
		reason = "assigned"
	}
	if isJiraTerminal(snap.Status) {
		status = "done"
	}

	return domain.Task{
		ID:              uuid.New().String(),
		Source:          "jira",
		SourceID:        snap.Key,
		SourceURL:       snap.URL,
		Title:           snap.Summary,
		Author:          snap.Assignee,
		Labels:          snap.Labels,
		Severity:        snap.Priority,
		SourceStatus:    snap.Status,
		RelevanceReason: reason,
		CreatedAt:       time.Now(),
		FetchedAt:       time.Now(),
		Status:          status,
	}
}

// issueToSnapshot converts a Jira API Issue to our snapshot type.
func issueToSnapshot(issue jiraclient.Issue, baseURL string) domain.JiraSnapshot {
	snap := domain.JiraSnapshot{
		Key:     issue.Key,
		Summary: issue.Fields.Summary,
		URL:     fmt.Sprintf("%s/browse/%s", baseURL, issue.Key),
	}
	if issue.Fields.Status != nil {
		snap.Status = issue.Fields.Status.Name
	}
	if issue.Fields.Assignee != nil {
		snap.Assignee = issue.Fields.Assignee.DisplayName
	}
	if issue.Fields.Priority != nil {
		snap.Priority = issue.Fields.Priority.Name
	}
	if issue.Fields.IssueType != nil {
		snap.IssueType = issue.Fields.IssueType.Name
	}
	if issue.Fields.Parent != nil {
		snap.ParentKey = issue.Fields.Parent.Key
	}
	if issue.Fields.Comment != nil {
		snap.CommentCount = issue.Fields.Comment.Total
	}
	snap.Labels = issue.Fields.Labels
	return snap
}

// --- Helpers ---

// parseTimeOrNow parses an RFC3339 timestamp, falling back to time.Now().
func parseTimeOrNow(s string) time.Time {
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t
	}
	return time.Now()
}

// prStateToTaskStatus maps a PR's GraphQL state to a task status.
func prStateToTaskStatus(snap domain.PRSnapshot) string {
	if snap.Merged || snap.State == "CLOSED" {
		return "done"
	}
	return "queued"
}

// jiraStatusToTaskStatus maps a Jira issue's current state to a task status.
func jiraStatusToTaskStatus(snap domain.JiraSnapshot) string {
	if isJiraTerminal(snap.Status) {
		return "done"
	}
	if snap.Assignee != "" {
		return "claimed"
	}
	return "queued"
}

// resolveTaskID looks up the task ID for a source+sourceID pair.
func (t *Tracker) resolveTaskID(source, sourceID string) string {
	var id string
	// sql.ErrNoRows is expected here for un-tracked items; any other error
	// leaves id empty which the caller handles the same way.
	_ = t.database.QueryRow(`SELECT id FROM tasks WHERE source = ? AND source_id = ?`, source, sourceID).Scan(&id)
	return id
}

// EmitPollComplete publishes the system poll-completed sentinel.
func (t *Tracker) EmitPollComplete(source string, taskCount, eventCount int) {
	t.bus.Publish(domain.Event{
		EventType: domain.EventSystemPollCompleted,
		SourceID:  source,
		Metadata:  mustJSON(map[string]any{"tasks": taskCount, "events": eventCount}),
		CreatedAt: time.Now(),
	})
}
