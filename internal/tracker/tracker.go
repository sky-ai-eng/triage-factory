package tracker

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/sky-ai-eng/todo-tinder/internal/db"
	"github.com/sky-ai-eng/todo-tinder/internal/domain"
	"github.com/sky-ai-eng/todo-tinder/internal/eventbus"
	ghclient "github.com/sky-ai-eng/todo-tinder/internal/github"
	jiraclient "github.com/sky-ai-eng/todo-tinder/internal/jira"
)

const (
	jiraBatchSize       = 100 // max issues per JQL key IN (...) query
	terminalPruneDays   = 30  // remove terminal items after this many days
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
// 1. Discover new PRs via search queries
// 2. Refresh all tracked PRs via batched GraphQL
// 3. Diff snapshots and emit events
func (t *Tracker) RefreshGitHub(client *ghclient.Client, username string, repos []string) (int, error) {
	// Phase 1: Discovery — find new PRs to track
	discovered, err := t.discoverGitHub(client, username, repos)
	if err != nil {
		log.Printf("[tracker] GitHub discovery error: %v", err)
		// Continue to refresh — discovery failure shouldn't block tracked item updates
	}

	// Register newly discovered items
	for _, d := range discovered {
		snap, _ := json.Marshal(d.Snapshot)
		item := domain.TrackedItem{
			ID:       fmt.Sprintf("github:pr:%s#%d", d.Snapshot.Repo, d.Snapshot.Number),
			Source:   "github",
			SourceID: fmt.Sprintf("%d", d.Snapshot.Number),
			Repo:     d.Snapshot.Repo,
			NodeID:   d.NodeID,
			Snapshot: string(snap),
		}
		if err := db.UpsertTrackedItem(t.database, item); err != nil {
			log.Printf("[tracker] error registering tracked item %s: %v", item.ID, err)
		}
	}

	// Phase 2: Refresh all tracked items via GraphQL batch
	nodeIDs, err := db.ListNodeIDs(t.database, "github")
	if err != nil {
		return 0, fmt.Errorf("list node IDs: %w", err)
	}
	if len(nodeIDs) == 0 {
		return len(discovered), nil
	}

	refreshed, err := client.RefreshPRs(nodeIDs)
	if err != nil {
		return 0, fmt.Errorf("refresh PRs: %w", err)
	}

	// Phase 3: Diff and emit
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
			// Item no longer accessible — might be deleted or permissions changed
			log.Printf("[tracker] tracked item %s not returned by refresh, skipping", item.ID)
			continue
		}

		// Parse previous snapshot
		var prevSnap domain.PRSnapshot
		if item.Snapshot != "" && item.Snapshot != "{}" {
			json.Unmarshal([]byte(item.Snapshot), &prevSnap)
		}

		// Diff
		events := DiffPRSnapshots(prevSnap, newSnap, item.SourceID)

		// Persist new snapshot
		snapJSON, _ := json.Marshal(newSnap)
		if err := db.UpdateTrackedSnapshot(t.database, "github", item.SourceID, string(snapJSON)); err != nil {
			log.Printf("[tracker] error updating snapshot for %s: %v", item.ID, err)
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
				db.SetTaskEventType(t.database, item.TaskID, evt.EventType)
			}
			t.bus.Publish(evt)
			eventsEmitted++
		}

		// Mark terminal if merged or closed
		if newSnap.Merged || newSnap.State == "CLOSED" {
			db.MarkTerminal(t.database, "github", item.SourceID)
		}
	}

	log.Printf("[tracker] GitHub refresh: %d discovered, %d tracked, %d refreshed, %d events",
		len(discovered), len(tracked), len(refreshed), eventsEmitted)

	return eventsEmitted, nil
}

// discoverGitHub runs search queries to find new PRs.
func (t *Tracker) discoverGitHub(client *ghclient.Client, username string, repos []string) ([]ghclient.DiscoveredPR, error) {
	// Build repo scope for queries
	repoScope := ""
	if len(repos) > 0 {
		parts := make([]string, len(repos))
		for i, r := range repos {
			parts[i] = "repo:" + r
		}
		repoScope = " " + strings.Join(parts, " ")
	}

	queries := []string{
		fmt.Sprintf("is:pr is:open review-requested:%s%s", username, repoScope),
		fmt.Sprintf("is:pr is:open author:%s%s", username, repoScope),
		fmt.Sprintf("is:pr is:open mentions:%s%s", username, repoScope),
	}

	seen := map[int]bool{}
	var all []ghclient.DiscoveredPR

	for _, q := range queries {
		prs, err := client.DiscoverPRs(q, 50)
		if err != nil {
			log.Printf("[tracker] discovery query failed: %v (query: %s)", err, q)
			continue
		}
		for _, pr := range prs {
			if !seen[pr.Snapshot.Number] {
				seen[pr.Snapshot.Number] = true
				all = append(all, pr)
			}
		}
	}

	return all, nil
}

// --- Jira ---

// RefreshJira runs the full tracking cycle for Jira issues.
// 1. Discover new issues via JQL searches
// 2. Refresh all tracked issues via batched JQL key IN (...)
// 3. Diff snapshots and emit events
func (t *Tracker) RefreshJira(client *jiraclient.Client, baseURL string, projects, pickupStatuses []string) (int, error) {
	// Phase 1: Discovery
	discovered, err := t.discoverJira(client, baseURL, projects, pickupStatuses)
	if err != nil {
		log.Printf("[tracker] Jira discovery error: %v", err)
	}

	// Register newly discovered items
	for _, snap := range discovered {
		snapJSON, _ := json.Marshal(snap)
		item := domain.TrackedItem{
			ID:       "jira:" + snap.Key,
			Source:   "jira",
			SourceID: snap.Key,
			Snapshot: string(snapJSON),
		}
		if err := db.UpsertTrackedItem(t.database, item); err != nil {
			log.Printf("[tracker] error registering tracked item %s: %v", item.ID, err)
		}
	}

	// Phase 2: Refresh all tracked items via batched JQL
	tracked, err := db.ListActiveTrackedItems(t.database, "jira")
	if err != nil {
		return 0, fmt.Errorf("list tracked jira items: %w", err)
	}
	if len(tracked) == 0 {
		return len(discovered), nil
	}

	// Collect keys and batch-fetch
	keys := make([]string, len(tracked))
	for i, item := range tracked {
		keys[i] = item.SourceID
	}

	refreshed, err := t.batchFetchJira(client, baseURL, keys)
	if err != nil {
		return 0, fmt.Errorf("batch fetch jira: %w", err)
	}

	// Phase 3: Diff and emit
	eventsEmitted := 0
	for _, item := range tracked {
		newSnap, ok := refreshed[item.SourceID]
		if !ok {
			log.Printf("[tracker] tracked Jira item %s not returned by refresh, skipping", item.ID)
			continue
		}

		var prevSnap domain.JiraSnapshot
		if item.Snapshot != "" && item.Snapshot != "{}" {
			json.Unmarshal([]byte(item.Snapshot), &prevSnap)
		}

		events := DiffJiraSnapshots(prevSnap, newSnap, item.SourceID)

		snapJSON, _ := json.Marshal(newSnap)
		if err := db.UpdateTrackedSnapshot(t.database, "jira", item.SourceID, string(snapJSON)); err != nil {
			log.Printf("[tracker] error updating snapshot for %s: %v", item.ID, err)
		}

		for _, evt := range events {
			evt.TaskID = item.TaskID
			if id, err := db.RecordEvent(t.database, evt); err != nil {
				log.Printf("[tracker] error recording event: %v", err)
			} else {
				evt.ID = id
			}
			if item.TaskID != "" {
				db.SetTaskEventType(t.database, item.TaskID, evt.EventType)
			}
			t.bus.Publish(evt)
			eventsEmitted++
		}

		if isJiraTerminal(newSnap.Status) {
			db.MarkTerminal(t.database, "jira", item.SourceID)
		}
	}

	log.Printf("[tracker] Jira refresh: %d discovered, %d tracked, %d refreshed, %d events",
		len(discovered), len(tracked), len(refreshed), eventsEmitted)

	return eventsEmitted, nil
}

// discoverJira runs JQL queries to find new issues.
func (t *Tracker) discoverJira(client *jiraclient.Client, baseURL string, projects, pickupStatuses []string) ([]domain.JiraSnapshot, error) {
	if len(projects) == 0 {
		return nil, nil
	}

	projectList := strings.Join(projects, ", ")
	var queries []string

	// Unassigned pickup
	if len(pickupStatuses) > 0 {
		quoted := make([]string, len(pickupStatuses))
		for i, s := range pickupStatuses {
			quoted[i] = fmt.Sprintf("%q", s)
		}
		queries = append(queries, fmt.Sprintf(
			`project IN (%s) AND status IN (%s) AND assignee IS EMPTY`, projectList, strings.Join(quoted, ", ")))
	}

	// Assigned to me
	queries = append(queries, fmt.Sprintf(
		`project IN (%s) AND assignee = currentUser() AND status NOT IN (Done, Closed, Resolved)`, projectList))

	seen := map[string]bool{}
	var all []domain.JiraSnapshot

	fields := []string{"summary", "status", "assignee", "priority", "labels", "issuetype", "parent"}

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

// batchFetchJira fetches current state for tracked Jira issues via key IN (...) queries.
// Batches in groups of jiraBatchSize to stay under JQL length limits.
func (t *Tracker) batchFetchJira(client *jiraclient.Client, baseURL string, keys []string) (map[string]domain.JiraSnapshot, error) {
	results := make(map[string]domain.JiraSnapshot, len(keys))
	fields := []string{"summary", "status", "assignee", "priority", "labels", "issuetype", "parent"}

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
	snap.Labels = issue.Fields.Labels
	return snap
}

// Prune removes terminal items older than the configured threshold.
func (t *Tracker) Prune() {
	removed, err := db.PruneTerminalItems(t.database, terminalPruneDays)
	if err != nil {
		log.Printf("[tracker] prune error: %v", err)
		return
	}
	if removed > 0 {
		log.Printf("[tracker] pruned %d terminal items", removed)
	}
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
