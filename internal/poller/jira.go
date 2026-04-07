package poller

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/sky-ai-eng/todo-tinder/internal/db"
	"github.com/sky-ai-eng/todo-tinder/internal/domain"
	"github.com/sky-ai-eng/todo-tinder/internal/eventbus"
)

// JiraPoller fetches tasks from the Jira API on an interval.
type JiraPoller struct {
	baseURL        string
	pat            string
	projects       []string
	pickupStatuses []string
	database       *sql.DB
	client         *http.Client
	interval       time.Duration
	stop           chan struct{}
	bus            *eventbus.Bus
}

// jiraItemState is the snapshot stored in poller_state for diffing.
type jiraItemState struct {
	Status   string `json:"status"`
	Assignee string `json:"assignee"`
	Priority string `json:"priority"`
}

func NewJiraPoller(baseURL, pat string, projects, pickupStatuses []string, database *sql.DB, interval time.Duration, bus *eventbus.Bus) *JiraPoller {
	return &JiraPoller{
		baseURL:        strings.TrimRight(baseURL, "/"),
		pat:            pat,
		projects:       projects,
		pickupStatuses: pickupStatuses,
		database:       database,
		client:         &http.Client{Timeout: 15 * time.Second},
		interval:       interval,
		stop:           make(chan struct{}),
		bus:            bus,
	}
}

func (p *JiraPoller) Start() {
	go func() {
		p.poll()
		ticker := time.NewTicker(p.interval)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				p.poll()
			case <-p.stop:
				return
			}
		}
	}()
}

func (p *JiraPoller) Stop() {
	close(p.stop)
}

func (p *JiraPoller) poll() {
	log.Println("[jira] polling for tasks...")

	var allTasks []domain.Task

	// 1. Unassigned pickup tasks -> queued
	pickupTasks, err := p.fetchPickupTasks()
	if err != nil {
		log.Printf("[jira] error fetching pickup tasks: %v", err)
	} else {
		allTasks = append(allTasks, pickupTasks...)
	}

	// 2. Assigned to me, in progress -> claimed
	inProgressTasks, err := p.fetchAssignedByStatus([]string{"In Progress", "In Review"}, "claimed")
	if err != nil {
		log.Printf("[jira] error fetching in-progress tasks: %v", err)
	} else {
		allTasks = append(allTasks, inProgressTasks...)
	}

	// 3. Assigned to me, done in last 30 days -> done
	doneTasks, err := p.fetchAssignedDone()
	if err != nil {
		log.Printf("[jira] error fetching done tasks: %v", err)
	} else {
		allTasks = append(allTasks, doneTasks...)
	}

	// Deduplicate by source_id
	seen := map[string]bool{}
	var tasks []domain.Task
	for _, t := range allTasks {
		if !seen[t.SourceID] {
			seen[t.SourceID] = true
			tasks = append(tasks, t)
		}
	}

	inserted := 0
	eventsEmitted := 0
	for _, t := range tasks {
		// Detect events via state diffing
		events := p.diffAndEmit(t)
		eventsEmitted += len(events)

		if len(events) > 0 {
			t.EventType = events[len(events)-1].EventType
		}

		if err := db.UpsertTask(p.database, t); err != nil {
			log.Printf("[jira] error upserting task %s: %v", t.SourceID, err)
			continue
		}
		inserted++

		// Record events and publish
		for _, evt := range events {
			evt.TaskID = t.ID
			if id, err := db.RecordEvent(p.database, evt); err != nil {
				log.Printf("[jira] error recording event: %v", err)
			} else {
				evt.ID = id
			}
			db.SetTaskEventType(p.database, t.ID, evt.EventType)
			p.bus.Publish(evt)
		}

		// Save state snapshot
		p.saveState(t)
	}

	log.Printf("[jira] poll complete: %d tasks processed, %d events emitted (%d pickup, %d in-progress, %d done)",
		inserted, eventsEmitted, len(pickupTasks), len(inProgressTasks), len(doneTasks))

	// Emit batch-complete sentinel so subscribers (scorer) know the cycle is done
	if inserted > 0 {
		p.bus.Publish(domain.Event{
			EventType: domain.EventSystemPollCompleted,
			SourceID:  "jira",
			Metadata:  mustJSON(map[string]any{"tasks": inserted, "events": eventsEmitted}),
			CreatedAt: time.Now(),
		})
	}
}

// diffAndEmit compares current state against stored snapshot and returns typed events.
func (p *JiraPoller) diffAndEmit(t domain.Task) []domain.Event {
	prevJSON, err := db.GetPollerState(p.database, "jira", t.SourceID)
	if err != nil {
		log.Printf("[jira] error loading poller state for %s: %v", t.SourceID, err)
	}

	current := jiraItemState{
		Status:   t.SourceStatus,
		Assignee: t.Author,
		Priority: t.Severity,
	}

	var events []domain.Event
	now := time.Now()

	if prevJSON == "" {
		// First time seeing this item
		eventType := p.initialEventType(t)
		if eventType != "" {
			events = append(events, domain.Event{
				EventType: eventType,
				TaskID:    t.ID,
				SourceID:  t.SourceID,
				Metadata:  mustJSON(map[string]string{"reason": "first_seen"}),
				CreatedAt: now,
			})
		}
		return events
	}

	var prev jiraItemState
	if err := json.Unmarshal([]byte(prevJSON), &prev); err != nil {
		log.Printf("[jira] error parsing previous state for %s: %v", t.SourceID, err)
		return events
	}

	// Detect status change
	if prev.Status != current.Status && current.Status != "" {
		eventType := domain.EventJiraIssueStatusChanged
		if t.Status == "done" {
			eventType = domain.EventJiraIssueCompleted
		}
		events = append(events, domain.Event{
			EventType: eventType,
			TaskID:    t.ID,
			SourceID:  t.SourceID,
			Metadata:  mustJSON(map[string]string{"prev_status": prev.Status, "new_status": current.Status}),
			CreatedAt: now,
		})
	}

	// Detect assignment change
	if prev.Assignee != current.Assignee && current.Assignee != "" {
		events = append(events, domain.Event{
			EventType: domain.EventJiraIssueAssigned,
			TaskID:    t.ID,
			SourceID:  t.SourceID,
			Metadata:  mustJSON(map[string]string{"prev_assignee": prev.Assignee, "new_assignee": current.Assignee}),
			CreatedAt: now,
		})
	}

	// Detect priority change
	if prev.Priority != current.Priority && current.Priority != "" {
		events = append(events, domain.Event{
			EventType: domain.EventJiraIssuePriorityChanged,
			TaskID:    t.ID,
			SourceID:  t.SourceID,
			Metadata:  mustJSON(map[string]string{"prev_priority": prev.Priority, "new_priority": current.Priority}),
			CreatedAt: now,
		})
	}

	return events
}

func (p *JiraPoller) initialEventType(t domain.Task) string {
	switch t.Status {
	case "done":
		return domain.EventJiraIssueCompleted
	case "claimed":
		return domain.EventJiraIssueAssigned
	default:
		return domain.EventJiraIssueAvailable
	}
}

func (p *JiraPoller) saveState(t domain.Task) {
	state := jiraItemState{
		Status:   t.SourceStatus,
		Assignee: t.Author,
		Priority: t.Severity,
	}
	data, _ := json.Marshal(state)
	if err := db.SetPollerState(p.database, "jira", t.SourceID, string(data)); err != nil {
		log.Printf("[jira] error saving poller state for %s: %v", t.SourceID, err)
	}
}

// fetchPickupTasks gets unassigned tickets in configured projects.
func (p *JiraPoller) fetchPickupTasks() ([]domain.Task, error) {
	if len(p.projects) == 0 || len(p.pickupStatuses) == 0 {
		return nil, nil
	}

	projectList := strings.Join(p.projects, ", ")
	quoted := make([]string, len(p.pickupStatuses))
	for i, s := range p.pickupStatuses {
		quoted[i] = fmt.Sprintf("%q", s)
	}
	statusList := strings.Join(quoted, ", ")

	jql := fmt.Sprintf(`project IN (%s) AND status IN (%s) AND assignee IS EMPTY`, projectList, statusList)
	return p.search(jql)
}

// fetchAssignedByStatus gets tickets assigned to the user in given statuses.
func (p *JiraPoller) fetchAssignedByStatus(statuses []string, taskStatus string) ([]domain.Task, error) {
	if len(p.projects) == 0 {
		return nil, nil
	}

	projectList := strings.Join(p.projects, ", ")
	quoted := make([]string, len(statuses))
	for i, s := range statuses {
		quoted[i] = fmt.Sprintf("%q", s)
	}
	statusList := strings.Join(quoted, ", ")

	jql := fmt.Sprintf(`project IN (%s) AND assignee = currentUser() AND status IN (%s)`, projectList, statusList)
	tasks, err := p.search(jql)
	if err != nil {
		return nil, err
	}
	for i := range tasks {
		tasks[i].Status = taskStatus
		tasks[i].RelevanceReason = "assigned"
	}
	return tasks, nil
}

// fetchAssignedDone gets tickets assigned to the user that were completed in the last 30 days.
func (p *JiraPoller) fetchAssignedDone() ([]domain.Task, error) {
	if len(p.projects) == 0 {
		return nil, nil
	}

	projectList := strings.Join(p.projects, ", ")
	jql := fmt.Sprintf(`project IN (%s) AND assignee = currentUser() AND status = Done AND resolved >= -30d`, projectList)
	tasks, err := p.search(jql)
	if err != nil {
		return nil, err
	}
	for i := range tasks {
		tasks[i].Status = "done"
		tasks[i].RelevanceReason = "assigned"
	}
	return tasks, nil
}

func (p *JiraPoller) search(jql string) ([]domain.Task, error) {
	apiURL := fmt.Sprintf("%s/rest/api/2/search?jql=%s&maxResults=50&fields=summary,description,status,assignee,priority,labels,created",
		p.baseURL, url.QueryEscape(jql))

	body, err := p.get(apiURL)
	if err != nil {
		return nil, err
	}

	var result jiraSearchResult
	if err := json.Unmarshal(body, &result); err != nil {
		return nil, fmt.Errorf("parse search results: %w", err)
	}

	tasks := make([]domain.Task, 0, len(result.Issues))
	for _, issue := range result.Issues {
		tasks = append(tasks, issue.toTask(p.baseURL))
	}
	return tasks, nil
}

func (p *JiraPoller) get(url string) ([]byte, error) {
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+p.pat)
	req.Header.Set("Content-Type", "application/json")

	resp, err := p.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("request %s: %w", url, err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read response body: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET %s returned %d: %s", url, resp.StatusCode, string(body))
	}
	return body, nil
}

// --- Jira API types ---

type jiraSearchResult struct {
	Issues []jiraIssue `json:"issues"`
}

type jiraIssue struct {
	Key    string     `json:"key"`
	Fields jiraFields `json:"fields"`
}

type jiraFields struct {
	Summary     string        `json:"summary"`
	Description string        `json:"description"`
	Status      jiraStatus    `json:"status"`
	Assignee    *jiraUser     `json:"assignee"`
	Priority    *jiraPriority `json:"priority"`
	Labels      []string      `json:"labels"`
	Created     string        `json:"created"`
}

type jiraStatus struct {
	Name string `json:"name"`
}

type jiraUser struct {
	DisplayName string `json:"displayName"`
	Key         string `json:"key"`
}

type jiraPriority struct {
	Name string `json:"name"`
	ID   string `json:"id"`
}

func (issue jiraIssue) toTask(baseURL string) domain.Task {
	var severity string
	if issue.Fields.Priority != nil {
		severity = issue.Fields.Priority.Name
	}

	var author string
	if issue.Fields.Assignee != nil {
		author = issue.Fields.Assignee.DisplayName
	}

	createdAt := time.Now()
	if t, err := time.Parse("2006-01-02T15:04:05.000-0700", issue.Fields.Created); err == nil {
		createdAt = t
	}

	return domain.Task{
		ID:           uuid.New().String(),
		Source:       "jira",
		SourceID:     issue.Key,
		SourceURL:    fmt.Sprintf("%s/browse/%s", baseURL, issue.Key),
		Title:        issue.Fields.Summary,
		Description:  issue.Fields.Description,
		Author:       author,
		Labels:       issue.Fields.Labels,
		Severity:     severity,
		SourceStatus: issue.Fields.Status.Name,
		CreatedAt:    createdAt,
		FetchedAt:    time.Now(),
		Status:       "queued",
	}
}
