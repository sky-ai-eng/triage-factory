package tracker

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/eventbus"
	ghclient "github.com/sky-ai-eng/triage-factory/internal/github"
	jiraclient "github.com/sky-ai-eng/triage-factory/internal/jira"
)

const (
	jiraBatchSize = 100 // max issues per JQL key IN (...) query
)

// Tracker manages the discover → refresh → diff → emit cycle for both
// GitHub and Jira. In the entity-first model, the tracker:
//   - creates/updates entities (not tasks — that's routing's job)
//   - diffs entity snapshots to produce per-action events
//   - publishes events to the bus (recording is routing's job)
//   - does NOT create or update tasks
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
	// Phase 1: Discovery — find new PRs and register as entities.
	discovered, err := t.discoverGitHub(client, username, repos)
	if err != nil {
		log.Printf("[tracker] GitHub discovery error: %v", err)
	}

	for _, d := range discovered {
		// Ensure the NodeID is stored in the snapshot so entity-based refresh
		// can extract it without a separate column.
		snap := d.Snapshot
		snap.NodeID = d.NodeID

		sid := ghSourceID(snap.Repo, snap.Number)
		entity, created, err := db.FindOrCreateEntity(t.database,
			"github", sid, "pr", snap.Title, snap.URL)
		if err != nil {
			log.Printf("[tracker] error creating entity for %s: %v", sid, err)
			continue
		}

		if created {
			// Seed the discovery snapshot.
			snapJSON, _ := json.Marshal(snap)
			if err := db.UpdateEntitySnapshot(t.database, entity.ID, string(snapJSON)); err != nil {
				log.Printf("[tracker] failed to seed snapshot for %s: %v", sid, err)
			}
			// If the PR is already terminal, mark the entity closed immediately
			// so it doesn't sit in the active refresh set forever (Phase 3
			// won't emit a merged/closed event because prev==curr).
			if snap.Merged || snap.State == "CLOSED" || snap.State == "MERGED" {
				if err := db.MarkEntityClosed(t.database, entity.ID); err != nil {
					log.Printf("[tracker] failed to mark entity %s closed on discovery: %v", sid, err)
				}
			}
		} else {
			// Update title if changed.
			if entity.Title != snap.Title {
				_ = db.UpdateEntityTitle(t.database, entity.ID, snap.Title)
			}
			// Reactivate if a previously-closed entity reappears as open
			// (e.g., reopened PR).
			if !snap.Merged && snap.State != "CLOSED" && snap.State != "MERGED" && entity.State == "closed" {
				if reactivated, err := db.ReactivateEntity(t.database, entity.ID); err != nil {
					log.Printf("[tracker] error reactivating %s: %v", sid, err)
				} else if reactivated {
					log.Printf("[tracker] reactivated entity %s (reopened)", sid)
				}
			}
		}
	}

	// Phase 2: Refresh active entities.
	entities, err := db.ListActiveEntities(t.database, "github")
	if err != nil {
		return 0, fmt.Errorf("list active github entities: %w", err)
	}

	// Classify by snapshot state (open vs terminal) for query cost tiering.
	type entityWithSnap struct {
		entity domain.Entity
		snap   domain.PRSnapshot
		nodeID string
	}
	var openItems, terminalItems []entityWithSnap

	for _, e := range entities {
		var snap domain.PRSnapshot
		if e.SnapshotJSON != "" && e.SnapshotJSON != "{}" {
			_ = json.Unmarshal([]byte(e.SnapshotJSON), &snap)
		}
		if snap.NodeID == "" {
			continue // can't refresh without a node ID
		}
		item := entityWithSnap{entity: e, snap: snap, nodeID: snap.NodeID}
		if snap.Merged || snap.State == "CLOSED" || snap.State == "MERGED" {
			terminalItems = append(terminalItems, item)
		} else {
			openItems = append(openItems, item)
		}
	}

	if len(openItems) == 0 && len(terminalItems) == 0 {
		return 0, nil
	}

	// Fetch fresh state — open PRs get the full fragment (includes CheckRuns).
	refreshed := make(map[string]domain.PRSnapshot)
	if len(openItems) > 0 {
		nodeIDs := make([]string, len(openItems))
		for i, item := range openItems {
			nodeIDs[i] = item.nodeID
		}
		open, err := client.RefreshPRs(nodeIDs, true)
		if err != nil {
			return 0, fmt.Errorf("refresh open PRs: %w", err)
		}
		for k, v := range open {
			refreshed[k] = v
		}
	}
	if len(terminalItems) > 0 {
		nodeIDs := make([]string, len(terminalItems))
		for i, item := range terminalItems {
			nodeIDs[i] = item.nodeID
		}
		terminal, err := client.RefreshPRs(nodeIDs, false)
		if err != nil {
			return 0, fmt.Errorf("refresh terminal PRs: %w", err)
		}
		for k, v := range terminal {
			refreshed[k] = v
		}
	}

	// Phase 3: Diff + emit events.
	allItems := append(openItems, terminalItems...)
	eventsEmitted := 0

	for _, item := range allItems {
		newSnap, ok := refreshed[item.nodeID]
		if !ok {
			continue
		}
		// Preserve NodeID through the refresh (RefreshPRs returns map[nodeID]→snap
		// but doesn't set snap.NodeID).
		newSnap.NodeID = item.nodeID

		// Diff against previous snapshot.
		events := DiffPRSnapshots(item.snap, newSnap, item.entity.ID, username)

		// Update entity snapshot + title.
		snapJSON, _ := json.Marshal(newSnap)
		if err := db.UpdateEntitySnapshot(t.database, item.entity.ID, string(snapJSON)); err != nil {
			log.Printf("[tracker] error updating snapshot for %s: %v", item.entity.SourceID, err)
		}
		if item.entity.Title != newSnap.Title {
			_ = db.UpdateEntityTitle(t.database, item.entity.ID, newSnap.Title)
		}

		// Publish events to bus. Recording + routing happens downstream.
		for _, evt := range events {
			t.bus.Publish(evt)
			eventsEmitted++
		}
	}

	log.Printf("[tracker] GitHub refresh: %d discovered, %d entities, %d refreshed, %d events",
		len(discovered), len(entities), len(refreshed), eventsEmitted)

	if len(entities) > 0 {
		t.EmitPollComplete("github", len(entities), eventsEmitted)
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
func (t *Tracker) RefreshJira(client *jiraclient.Client, baseURL string, projects, pickupStatuses []string, username string) (int, error) {
	// Phase 1: Discovery
	discovered, err := t.discoverJira(client, baseURL, projects, pickupStatuses)
	if err != nil {
		log.Printf("[tracker] Jira discovery error: %v", err)
	}

	for _, snap := range discovered {
		entity, created, err := db.FindOrCreateEntity(t.database,
			"jira", snap.Key, "issue", snap.Summary, snap.URL)
		if err != nil {
			log.Printf("[tracker] error creating entity for %s: %v", snap.Key, err)
			continue
		}
		if created {
			snapJSON, _ := json.Marshal(snap)
			if err := db.UpdateEntitySnapshot(t.database, entity.ID, string(snapJSON)); err != nil {
				log.Printf("[tracker] failed to seed snapshot for %s: %v", snap.Key, err)
			}
			if isJiraTerminal(snap.Status) {
				if err := db.MarkEntityClosed(t.database, entity.ID); err != nil {
					log.Printf("[tracker] failed to mark entity %s closed on discovery: %v", snap.Key, err)
				}
			}
		} else {
			if entity.Title != snap.Summary {
				_ = db.UpdateEntityTitle(t.database, entity.ID, snap.Summary)
			}
			// Reactivate if a previously-closed issue reappears as open.
			if !isJiraTerminal(snap.Status) && entity.State == "closed" {
				if reactivated, err := db.ReactivateEntity(t.database, entity.ID); err != nil {
					log.Printf("[tracker] error reactivating %s: %v", snap.Key, err)
				} else if reactivated {
					log.Printf("[tracker] reactivated entity %s (reopened)", snap.Key)
				}
			}
		}
	}

	// Phase 2: Refresh
	entities, err := db.ListActiveEntities(t.database, "jira")
	if err != nil {
		return 0, fmt.Errorf("list active jira entities: %w", err)
	}
	if len(entities) == 0 {
		return 0, nil
	}

	keys := make([]string, len(entities))
	for i, e := range entities {
		keys[i] = e.SourceID
	}

	refreshed, err := t.batchFetchJira(client, baseURL, keys)
	if err != nil {
		return 0, fmt.Errorf("batch fetch jira: %w", err)
	}

	// Phase 3: Diff + emit events.
	eventsEmitted := 0
	for _, e := range entities {
		newSnap, ok := refreshed[e.SourceID]
		if !ok {
			continue
		}

		var prevSnap domain.JiraSnapshot
		if e.SnapshotJSON != "" && e.SnapshotJSON != "{}" {
			if err := json.Unmarshal([]byte(e.SnapshotJSON), &prevSnap); err != nil {
				log.Printf("[tracker] corrupt jira snapshot for %s, reseeding: %v", e.SourceID, err)
				snapJSON, _ := json.Marshal(newSnap)
				_ = db.UpdateEntitySnapshot(t.database, e.ID, string(snapJSON))
				continue
			}
		}

		events := DiffJiraSnapshots(prevSnap, newSnap, e.ID, username)

		snapJSON, _ := json.Marshal(newSnap)
		if err := db.UpdateEntitySnapshot(t.database, e.ID, string(snapJSON)); err != nil {
			log.Printf("[tracker] error updating jira snapshot for %s: %v", e.SourceID, err)
		}
		if e.Title != newSnap.Summary {
			_ = db.UpdateEntityTitle(t.database, e.ID, newSnap.Summary)
		}

		for _, evt := range events {
			t.bus.Publish(evt)
			eventsEmitted++
		}
	}

	log.Printf("[tracker] Jira refresh: %d discovered, %d entities, %d refreshed, %d events",
		len(discovered), len(entities), len(refreshed), eventsEmitted)

	// Always fire the sentinel — it means "a poll cycle completed," not "a
	// poll produced work." Carry-over readiness depends on this firing even
	// on an empty first poll (e.g. projects configured but nothing assigned
	// yet), otherwise the setup step shimmers forever.
	t.EmitPollComplete("jira", len(entities), eventsEmitted)

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

// EmitPollComplete publishes the system poll-completed sentinel.
func (t *Tracker) EmitPollComplete(source string, entityCount, eventCount int) {
	t.bus.Publish(domain.Event{
		EventType:    domain.EventSystemPollCompleted,
		MetadataJSON: mustJSON(map[string]any{"source": source, "entities": entityCount, "events": eventCount}),
		CreatedAt:    time.Now(),
	})
}
