package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"sort"
	"strings"

	"github.com/sky-ai-eng/triage-factory/internal/auth"
	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	jiraevents "github.com/sky-ai-eng/triage-factory/internal/domain/events"
	"github.com/sky-ai-eng/triage-factory/internal/integrations"
	"github.com/sky-ai-eng/triage-factory/internal/jira"
	"github.com/sky-ai-eng/triage-factory/internal/server/teamscope"
	"github.com/sky-ai-eng/triage-factory/internal/toast"
)

// stockTicket is the per-row payload for the carry-over list. Bucket +
// PrefilledAction let the frontend render two sections ("Your tickets" /
// "Available to claim") and seed the tri-selector with a sensible default
// based on the ticket's current Jira status.
type stockTicket struct {
	IssueKey  string `json:"issue_key"`
	Summary   string `json:"summary"`
	Status    string `json:"status"`
	Project   string `json:"project"`
	IssueType string `json:"issue_type"`
	Priority  string `json:"priority"`
	ParentKey string `json:"parent_key,omitempty"`
	ParentURL string `json:"parent_url,omitempty"`
	URL       string `json:"url"`
	// Bucket is "assigned" (assigned to the user) or "available" (unassigned
	// in a Pickup-rule status). Frontend splits the list on this field.
	Bucket string `json:"bucket"`
	// PrefilledAction is "queue" | "claim" | "done" | "". Empty means the
	// user must choose — we couldn't infer a sensible default from the
	// current Jira status (e.g. an assigned ticket in a status that matches
	// none of the configured Pickup/InProgress/Done rules, or any ticket in
	// the available bucket).
	PrefilledAction string `json:"prefilled_action,omitempty"`
}

// handleJiraStockGet returns two carry-over buckets:
//
//   - assigned: non-terminal Jira tickets assigned to the user, with a
//     prefilled action derived from the current status (Pickup → queue,
//     InProgress → claim, Done → done; unmapped statuses → no prefill).
//   - available: unassigned tickets currently in a Pickup-rule status —
//     new work the user could grab.
//
// Tickets without snapshots yet, tickets with active tasks, and parents
// with open subtasks (SKY-173) are skipped. Returns {status: "polling"}
// while the Jira poller hasn't completed its first cycle since the last
// config change — snapshots are seeded on first poll.
func (s *Server) handleJiraStockGet(w http.ResponseWriter, r *http.Request) {
	orgID, ok := s.requireOrg(w, r)
	if !ok {
		return
	}
	userID := ClaimsFrom(r.Context()).Subject
	// Everything this handler reads from per-tenant state — creds,
	// org settings, default team, Jira rules, the user's Jira
	// identity — runs through tx-bound stores under the user's
	// claims so RLS (org_settings_select / team_settings_select /
	// jira_rules_select / users_select) gates every read.
	// SKY-270: identity facts live on the users row, not the
	// keychain. Account ID drives "is this assigned to me" (stable,
	// predicate-grade); display name drives the optimistic post-
	// claim snapshot update so the synthesized event metadata reads
	// correctly. Both come from the same auth.ValidateJira response
	// at PAT setup; missing either means the bootstrap hasn't run
	// yet or Jira isn't connected.
	var (
		creds                            auth.Credentials
		orgSet                           domain.OrgSettings
		teamID                           string
		jiraRules                        []domain.JiraProjectStatusRules
		localAccountID, localDisplayName string
	)
	// The optional ?team_id= read filter narrows the deck to one team's
	// Jira projects (the per-page selector). The deck is single-team by
	// construction; teamscope.ResolveRead defaults to the sticky/first team
	// when no filter is set rather than blocking a multi-team caller.
	// teamscope.SingleParam drops a malformed id to "" — same validation the
	// prompts / event-handlers read paths use, so a junk value can't reach
	// a future ::uuid cast.
	filterTeam := teamscope.SingleParam(r)
	if err := s.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		creds, _ = integrations.Load(r.Context(), tx.Secrets, orgID)
		var e error
		orgSet, e = tx.Orgs.GetSettings(r.Context(), orgID)
		if e != nil {
			return fmt.Errorf("load org settings: %w", e)
		}
		teamID, e = teamscope.ResolveRead(r.Context(), tx.Teams, tx.Users, orgID, userID, filterTeam)
		if e != nil {
			return e
		}
		jiraRules, e = tx.JiraStatusRules.ListForTeam(r.Context(), teamID)
		if e != nil {
			return fmt.Errorf("list jira rules: %w", e)
		}
		// Jira identity is host-scoped (SKY-397): look it up for the org's
		// Jira host (org_settings already loaded above).
		localAccountID, localDisplayName, e = tx.Users.GetJiraIdentity(r.Context(), userID, orgSet.JiraBaseURL)
		return e
	}); err != nil {
		internalError(w, "stock", err)
		return
	}

	// Require full Jira configuration (PAT + URL + at least one project) plus
	// a stored identity so we can match the assignee field. Gate on the
	// account ID alone, not the display name: StableID() is always populated
	// when connected (it falls back to the Server/DC key), and assignee
	// matching keys on account ID — display name is only the legacy
	// no-accountID fallback and the post-claim snapshot label, both of which
	// degrade gracefully when it's blank (valid on some Server/DC installs).
	// Partial config would silently stall on "polling" forever because the
	// poller never has anything to do.
	if creds.JiraPAT == "" || creds.JiraURL == "" || len(jiraRules) == 0 || localAccountID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "Jira not configured"})
		return
	}

	if !s.jiraPollReady() {
		writeJSON(w, http.StatusOK, map[string]any{"status": "polling"})
		return
	}

	var entities []domain.Entity
	var taskedEntityIDs map[string]struct{}
	if err := s.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		var e error
		// Team-scoped discovery read: in multi-mode this returns only
		// Jira entities whose project is attached to one of the viewer's
		// teams (via the jira_project_status_rules RLS semi-join), so an
		// org-wide poller can't surface another team's untriaged backlog
		// on this user's swipe deck. Local mode (SQLite) returns the full
		// active set — N=1, nothing to scope away.
		entities, e = tx.Entities.ListActiveJiraTeamScoped(r.Context(), orgID, teamID)
		if e != nil {
			return e
		}
		// Batch-fetch the set of Jira entity IDs that already have an active task
		// so we don't run N queries inside the loop. If this fails we can't tell
		// which entities are safe to show, so fail the request outright.
		taskedEntityIDs, e = tx.Tasks.EntityIDsWithActiveTasks(r.Context(), orgID, "jira")
		return e
	}); err != nil {
		internalError(w, "stock", err)
		return
	}

	type scored struct {
		ticket    stockTicket
		createdAt string // ISO-8601 from snap.CreatedAt; empty for old snapshots
		fallback  string // entity.CreatedAt as RFC3339 — sort key when snap.CreatedAt is empty
	}

	var assigned, available []scored
	for _, e := range entities {
		if _, hasTask := taskedEntityIDs[e.ID]; hasTask {
			continue
		}
		if e.SnapshotJSON == "" || e.SnapshotJSON == "{}" {
			continue
		}
		var snap domain.JiraSnapshot
		if err := json.Unmarshal([]byte(e.SnapshotJSON), &snap); err != nil {
			log.Printf("[stock] skipping entity %s (%s): invalid snapshot: %v", e.ID, e.SourceID, err)
			continue
		}
		// Subtask gate (SKY-173) applies to both buckets — a parent ticket
		// with open subtasks is a container, not a work unit. Its subtasks
		// (if assigned or available) surface on their own; if the
		// decomposition later collapses, became_atomic routes the parent
		// through the normal path.
		if snap.OpenSubtaskCount > 0 {
			continue
		}

		var parentURL string
		jiraBase := orgSet.JiraBaseURL
		if jiraBase == "" {
			jiraBase = creds.JiraURL
		}
		if snap.ParentKey != "" && jiraBase != "" {
			parentURL = strings.TrimRight(jiraBase, "/") + "/browse/" + snap.ParentKey
		}

		projectKey := projectFromKey(snap.Key)
		baseTicket := stockTicket{
			IssueKey:  snap.Key,
			Summary:   snap.Summary,
			Status:    snap.Status,
			Project:   projectKey,
			IssueType: snap.IssueType,
			Priority:  snap.Priority,
			ParentKey: snap.ParentKey,
			ParentURL: parentURL,
			URL:       snap.URL,
		}

		// Per-project rule lookup. nil means the ticket's project_key
		// has no row — degrade like "no rules configured": skip the
		// available bucket entirely (we can't classify pickup status)
		// and leave prefilled actions empty for assigned tickets.
		projectRule := domain.RuleForProject(jiraRules, projectKey)

		// SKY-270: "is this assigned to me?" uses the Atlassian account ID
		// — the stable identifier — rather than display name. Display name
		// is a fallback for older snapshots that predate the account-id
		// field (empty AssigneeAccountID); they degrade to today's behavior
		// of comparing the display name string.
		isSelf := (snap.AssigneeAccountID != "" && snap.AssigneeAccountID == localAccountID) ||
			(snap.AssigneeAccountID == "" && snap.Assignee == localDisplayName)
		isUnassigned := snap.Assignee == ""

		switch {
		case isSelf:
			baseTicket.Bucket = "assigned"
			baseTicket.PrefilledAction = prefillForAssigned(projectRule, snap.Status)
			assigned = append(assigned, scored{baseTicket, snap.CreatedAt, e.CreatedAt.Format("2006-01-02T15:04:05Z07:00")})

		case isUnassigned && projectRule != nil && projectRule.PickupContains(snap.Status):
			baseTicket.Bucket = "available"
			baseTicket.PrefilledAction = "" // user decides
			available = append(available, scored{baseTicket, snap.CreatedAt, e.CreatedAt.Format("2006-01-02T15:04:05Z07:00")})

		default:
			// Assigned to someone else, unassigned but not in Pickup
			// (in-progress orphan, stale Done), or project has no
			// configured rules — no action in carry-over.
			if projectRule == nil {
				log.Printf("[stock] skipping %s: project %q has no configured status rules", snap.Key, projectKey)
			}
			continue
		}
	}

	// Newest-first within each bucket. Primary key is snap.CreatedAt (Jira's
	// own creation timestamp); when a snapshot predates this field (zero
	// value) we fall back to the entity's TF-side created_at so ordering
	// degrades gracefully instead of jumping to top/bottom.
	// Times are parsed to time.Time before comparison so timezone/offset
	// format differences (e.g. "+0000" vs "-07:00") don't corrupt ordering.
	byNewest := func(list []scored) {
		sort.SliceStable(list, func(i, j int) bool {
			iKey := list[i].createdAt
			if iKey == "" {
				iKey = list[i].fallback
			}
			jKey := list[j].createdAt
			if jKey == "" {
				jKey = list[j].fallback
			}
			it, iOK := domain.ParseExternalTime(iKey)
			jt, jOK := domain.ParseExternalTime(jKey)
			if iOK && jOK {
				return it.After(jt)
			}
			return iKey > jKey
		})
	}
	byNewest(assigned)
	byNewest(available)

	assignedOut := make([]stockTicket, len(assigned))
	for i, s := range assigned {
		assignedOut[i] = s.ticket
	}
	availableOut := make([]stockTicket, len(available))
	for i, s := range available {
		availableOut[i] = s.ticket
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"status":    "ready",
		"assigned":  assignedOut,
		"available": availableOut,
	})
}

// prefillForAssigned returns the carry-over action that matches the ticket's
// current Jira status, or "" if none of the configured status rules apply.
// Done.Contains takes precedence over InProgress (a ticket in a Done-rule
// status should always be offered for closure, even if the user has
// overlapping rule membership). Pickup is checked last so the "new work"
// case is the default for simply-assigned-to-you tickets.
//
// A nil project rule (the ticket's project_key has no configured rules)
// returns "" — user picks the action manually, matching the "no rules
// configured" degrade-cleanly contract.
func prefillForAssigned(rule *domain.JiraProjectStatusRules, status string) string {
	if rule == nil {
		return ""
	}
	switch {
	case rule.DoneContains(status):
		return "done"
	case rule.InProgressContains(status):
		return "claim"
	case rule.PickupContains(status):
		return "queue"
	default:
		return ""
	}
}

type stockAction struct {
	IssueKey string `json:"issue_key"`
	Action   string `json:"action"` // "queue" | "claim" | "done"
}

type stockFailure struct {
	IssueKey string `json:"issue_key"`
	Action   string `json:"action"`
	Error    string `json:"error"`
}

// handleJiraStockPost applies carry-over actions. Eligibility varies by
// bucket:
//
//   - Assigned (snap.Assignee == self): queue/claim/done are all valid.
//     queue emits jira:issue:assigned (no Jira mutation). claim emits
//     jira:issue:assigned + assigns-to-self + transitions to InProgress.
//     done transitions to Done + closes the entity; a no-op guard skips
//     the transition when already in a Done-member status.
//
//   - Available (unassigned, Pickup status): queue emits jira:issue:available
//     (no Jira mutation — user is parking it in the queue to decide later).
//     claim behaves like the assigned-claim path (assign + transition +
//     claimed task). done is rejected — closing an unassigned ticket from
//     here is not a supported cleanup action.
//
// Transition failures are surfaced per-row; other actions still apply.
func (s *Server) handleJiraStockPost(w http.ResponseWriter, r *http.Request) {
	orgID, ok := s.requireOrg(w, r)
	if !ok {
		return
	}
	userID := ClaimsFrom(r.Context()).Subject
	var req struct {
		Actions []stockAction `json:"actions"`
		// TeamID is the acting team the write picker supplied — the team
		// claimed/queued stock tasks are stamped under. Required in the UI
		// when the caller belongs to ≥2 teams; empty (sole-team fallback)
		// otherwise. The same value must scope the Jira rules the actions
		// validate against, so claim writes land on the picked team.
		TeamID string `json:"team_id"`
	}
	if !decodeJSON(w, r, &req, "") {
		return
	}

	// All per-tenant state goes through tx-bound stores so RLS gates
	// fire under the user's claims — mirrors handleJiraStockGet.
	var (
		creds                            auth.Credentials
		teamID                           string
		jiraRules                        []domain.JiraProjectStatusRules
		localAccountID, localDisplayName string
	)
	if err := s.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		creds, _ = integrations.Load(r.Context(), tx.Secrets, orgID)
		var e error
		teamID, e = teamscope.ResolveActing(r.Context(), tx.Teams, tx.Users, orgID, userID, req.TeamID)
		if e != nil {
			return e
		}
		jiraRules, e = tx.JiraStatusRules.ListForTeam(r.Context(), teamID)
		if e != nil {
			return fmt.Errorf("list jira rules: %w", e)
		}
		// Jira identity is host-scoped (SKY-397): creds.JiraURL mirrors
		// org_settings.jira_base_url (both set from the same field), so it
		// is the canonical host this identity was captured under.
		localAccountID, localDisplayName, e = tx.Users.GetJiraIdentity(r.Context(), userID, creds.JiraURL)
		return e
	}); err != nil {
		if teamscope.WriteIfSelectionError(w, err) {
			return
		}
		internalError(w, "stock", err)
		return
	}
	// Account ID alone gates the action (see handleJiraStockGet): display
	// name is optional and degrades gracefully when blank.
	if creds.JiraPAT == "" || creds.JiraURL == "" || len(jiraRules) == 0 || localAccountID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "Jira not configured"})
		return
	}

	// Batch-fetch, for O(1) per-action eligibility checks: (a) the Jira
	// entities that already have an active task, and (b) the team-scoped Jira
	// set the viewer is allowed to act on — the *same* ListActiveJiraTeamScoped
	// read the GET deck uses, so POST eligibility can't diverge from what the
	// deck surfaced. Fail the request if either read fails.
	var (
		taskedEntityIDs map[string]struct{}
		scopedJiraIDs   map[string]struct{}
	)
	if err := s.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		var e error
		taskedEntityIDs, e = tx.Tasks.EntityIDsWithActiveTasks(r.Context(), orgID, "jira")
		if e != nil {
			return e
		}
		scoped, e := tx.Entities.ListActiveJiraTeamScoped(r.Context(), orgID, teamID)
		if e != nil {
			return e
		}
		scopedJiraIDs = make(map[string]struct{}, len(scoped))
		for _, ent := range scoped {
			scopedJiraIDs[ent.ID] = struct{}{}
		}
		return nil
	}); err != nil {
		internalError(w, "stock", err)
		return
	}

	// SKY-463: claim (assign + transition) and done (transition) are
	// user-initiated Jira writes — they must act as the acting user, not the
	// org service account (AssignToSelf assigns to the token's user). Resolve
	// the acting user's Jira client only when the batch actually contains a
	// write-bearing action: a queue action synthesizes an event + task with no
	// Jira mutation, so a queue-only request must not depend on secret-store
	// availability (a transient token-lookup failure shouldn't 500 a pure
	// queue). On ErrNoJiraUserCredential the client stays nil and the claim/done
	// rows fail per-row with a connect prompt; queue rows still apply.
	var jiraUserClient *jira.Client
	needsUserClient := false
	for _, a := range req.Actions {
		if a.Action == "claim" || a.Action == "done" {
			needsUserClient = true
			break
		}
	}
	if needsUserClient {
		if c, jerr := s.jiraResolver.ForUser(r.Context(), orgID, userID); jerr == nil {
			jiraUserClient = c
		} else if !errors.Is(jerr, jira.ErrNoJiraUserCredential) {
			internalError(w, "stock", jerr)
			return
		}
	}

	applied := 0
	queued := 0 // number of queue actions applied — gates the scorer trigger
	claimed := 0
	closed := 0
	failed := make([]stockFailure, 0)

	for _, a := range req.Actions {
		if a.Action != "queue" && a.Action != "claim" && a.Action != "done" {
			failed = append(failed, stockFailure{a.IssueKey, a.Action, "unknown action"})
			continue
		}

		issueKey := strings.TrimSpace(a.IssueKey)
		if issueKey == "" {
			failed = append(failed, stockFailure{a.IssueKey, a.Action, "missing issue_key"})
			continue
		}

		var entity *domain.Entity
		if err := s.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
			var e error
			entity, e = tx.Entities.GetBySource(r.Context(), orgID, "jira", issueKey)
			return e
		}); err != nil {
			failed = append(failed, stockFailure{issueKey, a.Action, "failed to load entity"})
			continue
		}
		if entity == nil {
			failed = append(failed, stockFailure{issueKey, a.Action, "entity not found"})
			continue
		}

		// Team-scope gate: mirror the GET deck (ListActiveJiraTeamScoped). A
		// ticket whose project isn't tracked by the viewer's team(s) isn't on
		// their deck and must not be actionable here — even if it's assigned to
		// them — which closes the bypass where a user POSTs an off-team issue
		// key. In local mode the scoped set is the full active Jira set, so this
		// gate is a no-op there.
		if _, inScope := scopedJiraIDs[entity.ID]; !inScope {
			failed = append(failed, stockFailure{a.IssueKey, a.Action, "ticket is outside your team's tracked projects"})
			continue
		}

		// Enforce the same eligibility rules as the GET list. Prevents acting
		// on tickets that shouldn't be in carry-over at all — stale frontend
		// state, tampered requests, or tickets that changed since GET.
		if entity.SnapshotJSON == "" || entity.SnapshotJSON == "{}" {
			failed = append(failed, stockFailure{issueKey, a.Action, "no snapshot yet"})
			continue
		}
		var snap domain.JiraSnapshot
		if err := json.Unmarshal([]byte(entity.SnapshotJSON), &snap); err != nil {
			failed = append(failed, stockFailure{a.IssueKey, a.Action, "invalid snapshot"})
			continue
		}

		// Per-project rule lookup. Tickets whose project_key has no
		// configured rules fall through every branch below — there's
		// no terminal check, no available bucket — so the only action
		// they support is "queue" on an assigned ticket (the
		// synthesized event doesn't depend on status rules).
		projectRule := domain.RuleForProject(jiraRules, projectFromKey(snap.Key))

		isSelf := (snap.AssigneeAccountID != "" && snap.AssigneeAccountID == localAccountID) ||
			(snap.AssigneeAccountID == "" && snap.Assignee == localDisplayName)
		isUnassigned := snap.Assignee == ""
		isAvailable := isUnassigned && projectRule != nil && projectRule.PickupContains(snap.Status)

		if !isSelf && !isAvailable {
			failed = append(failed, stockFailure{a.IssueKey, a.Action, "ticket is not assigned to you and not in the available pickup queue"})
			continue
		}

		// Defensive subtask gate (SKY-173 principle): queue/claim on a parent
		// with open subtasks would create the exact non-atomic task the main
		// flow works hard to suppress. The GET handler already filters these
		// out so legitimate UI flows never submit them, but subtasks could be
		// added between GET and POST, or the request could come from a stale
		// frontend. "done" is still allowed on the assigned branch — closing
		// a parent with dangling subtasks is a valid cleanup action.
		if snap.OpenSubtaskCount > 0 && a.Action != "done" {
			failed = append(failed, stockFailure{a.IssueKey, a.Action, "ticket has open subtasks — delegate those atomic subtasks directly rather than the parent"})
			continue
		}

		// Available-bucket branches never make sense for "done" — closing an
		// unassigned ticket from carry-over isn't a supported cleanup (the
		// "done" flow is for orphan cleanup on your own assigned tickets that
		// are already in a Done-rule status).
		if isAvailable && a.Action == "done" {
			failed = append(failed, stockFailure{a.IssueKey, a.Action, "ticket is not assigned to you; done is only for cleaning up your own already-complete tickets"})
			continue
		}

		// Assigned-bucket: tickets in Done.Members are allowed through for
		// the "done" action (no-op guard skips the Jira transition when the
		// status is already a Done member); queue/claim on an already-done
		// ticket is pointless, so reject those outright.
		if isSelf && projectRule != nil && projectRule.DoneContains(snap.Status) && a.Action != "done" {
			failed = append(failed, stockFailure{a.IssueKey, a.Action, "ticket is already in a done status — only the done action is valid"})
			continue
		}

		if _, hasTask := taskedEntityIDs[entity.ID]; hasTask {
			failed = append(failed, stockFailure{a.IssueKey, a.Action, "ticket already has an active task"})
			continue
		}

		switch a.Action {
		case "queue":
			// Available tickets synthesize jira:issue:available (they're
			// unassigned — a synthesized jira:issue:assigned would be a
			// lie). Assigned tickets use jira:issue:assigned as before.
			var eventType string
			var eventID string
			if err := s.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
				var e error
				if isAvailable {
					eventType = domain.EventJiraIssueAvailable
					eventID, e = recordCarryOverAvailableEvent(r.Context(), tx.Events, orgID, entity.ID, snap)
				} else {
					eventType = domain.EventJiraIssueAssigned
					eventID, e = recordCarryOverAssignedEvent(r.Context(), tx.Events, orgID, entity.ID, snap)
				}
				if e != nil {
					return e
				}
				_, _, e = tx.Tasks.FindOrCreate(r.Context(), orgID, teamID, entity.ID, eventType, "", eventID, 0.5)
				return e
			}); err != nil {
				failed = append(failed, stockFailure{a.IssueKey, a.Action, err.Error()})
				continue
			}
			queued++

		case "claim":
			// SKY-463: claim performs Jira writes as the acting user; refuse
			// (don't act as the bot) when they have no connected Jira.
			if jiraUserClient == nil {
				failed = append(failed, stockFailure{a.IssueKey, a.Action, "connect your Jira to act on tickets"})
				continue
			}
			if projectRule == nil || projectRule.InProgressCanonical == "" {
				failed = append(failed, stockFailure{a.IssueKey, a.Action, "in_progress canonical status not configured for this project"})
				continue
			}

			// Do Jira mutations first: if Jira fails we bail before touching
			// the task table, so there's no claimed-task orphan pointing at a
			// Jira issue that never got assigned or transitioned. Claim-guard
			// pattern skips the API calls when state is already correct —
			// containment against InProgress.Members so a ticket in any
			// in-progress variant isn't transitioned back to canonical. For
			// available tickets the state check is a no-op (they're
			// unassigned by definition), but GetClaimState is cheap and keeps
			// one code path for both branches.
			state := jiraUserClient.GetClaimState(r.Context(), a.IssueKey)
			if state == nil || !state.AssignedToSelf {
				if err := jiraUserClient.AssignToSelf(r.Context(), a.IssueKey); err != nil {
					failed = append(failed, stockFailure{a.IssueKey, a.Action, "assign: " + err.Error()})
					continue
				}
			}
			if state == nil || !projectRule.InProgressContains(state.StatusName) {
				if err := jiraUserClient.TransitionTo(r.Context(), a.IssueKey, projectRule.InProgressCanonical); err != nil {
					failed = append(failed, stockFailure{a.IssueKey, a.Action, "transition: " + err.Error()})
					continue
				}
				snap.Status = projectRule.InProgressCanonical
			} else {
				snap.Status = state.StatusName
			}

			// Refresh the snap with the known post-mutation state so the
			// synthesized event metadata matches the ticket's actual Jira
			// state at the moment of claim. Both the display name (for UI)
			// and the account ID (for predicate matching) flip to self.
			snap.Assignee = localDisplayName
			snap.AssigneeAccountID = localAccountID

			// Both assigned and available claim paths end with a
			// jira:issue:assigned event — after the AssignToSelf call, the
			// user is the assignee in Jira too, so the event metadata is
			// accurate for either starting state.
			var task *domain.Task
			var claimOK bool
			if err := s.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
				eventID, e := recordCarryOverAssignedEvent(r.Context(), tx.Events, orgID, entity.ID, snap)
				if e != nil {
					return fmt.Errorf("record event: %w", e)
				}
				task, _, e = tx.Tasks.FindOrCreate(r.Context(), orgID, teamID, entity.ID, domain.EventJiraIssueAssigned, "", eventID, 0.5)
				if e != nil {
					return e
				}
				// SKY-261 B+: claim is on the responsibility axis now,
				// not status. status='claimed' was dropped along with
				// status='delegated' once the claim cols took over the
				// "who's responsible" answer. Stamp the user claim
				// optimistically — if the task pre-existed in some
				// non-queued state or was already claimed by someone
				// else, surface that as a failed action rather than
				// stealing.
				claimOK, e = tx.Tasks.ClaimQueuedForUser(r.Context(), orgID, task.ID, userID)
				return e
			}); err != nil {
				failed = append(failed, stockFailure{a.IssueKey, a.Action, err.Error()})
				continue
			}
			if !claimOK {
				failed = append(failed, stockFailure{a.IssueKey, a.Action, "task already claimed or no longer queued"})
				continue
			}
			claimed++

		case "done":
			// SKY-463: done transitions the ticket as the acting user; refuse
			// (don't act as the bot) when they have no connected Jira.
			if jiraUserClient == nil {
				failed = append(failed, stockFailure{a.IssueKey, a.Action, "connect your Jira to act on tickets"})
				continue
			}
			if projectRule == nil || projectRule.DoneCanonical == "" {
				failed = append(failed, stockFailure{a.IssueKey, a.Action, "done canonical status not configured for this project"})
				continue
			}
			// Skip the transition when the ticket is already in any Done
			// member (not just the canonical) — a ticket in "Verified" when
			// Done.Members=[Resolved,Verified] is already done from TF's
			// perspective; transitioning to Resolved would be a no-op at best
			// and a workflow violation at worst.
			state := jiraUserClient.GetClaimState(r.Context(), a.IssueKey)
			if state == nil || !projectRule.DoneContains(state.StatusName) {
				if err := jiraUserClient.TransitionTo(r.Context(), a.IssueKey, projectRule.DoneCanonical); err != nil {
					failed = append(failed, stockFailure{a.IssueKey, a.Action, "transition: " + err.Error()})
					continue
				}
			}
			if err := s.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
				return tx.Entities.MarkClosed(r.Context(), orgID, entity.ID)
			}); err != nil {
				failed = append(failed, stockFailure{a.IssueKey, a.Action, err.Error()})
				continue
			}
			closed++
		}

		applied++
	}

	// Carry-over creates tasks without going through the poller, so no
	// system:poll:completed fires to wake the scorer via its event-bus
	// subscription. Poke it directly, but only when we actually produced
	// queued tasks — done doesn't create a task at all, and claim now
	// (post-SKY-261 B+) creates a status='queued' task with the user
	// claim col stamped. UnscoredTasks would pick up those rows on its
	// next natural cycle, so leaving the trigger off this branch is fine
	// — scoring a user-claimed task is harmless dormant work rather than
	// the wrong-status skip the old comment described.
	if queued > 0 && s.scorerTrigger != nil {
		s.scorerTrigger(orgID)
	}

	// Success toast with the per-action breakdown when at least one ticket
	// applied cleanly. The frontend also shows a partial-failure warning toast
	// if there are any failures; this one only fires on at-least-one-success.
	if applied > 0 {
		toast.Success(s.ws, orgID, fmt.Sprintf(
			"Carry-over applied: %d queued, %d claimed, %d closed", queued, claimed, closed,
		))
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"applied": applied,
		"failed":  failed,
	})
}

// recordCarryOverAssignedEvent writes a synthesized jira:issue:assigned event
// for a carry-over ticket and returns the event ID. Tasks require a non-null
// primary_event_id FK to events.id, but carry-over has no upstream event —
// the tracker seeded the snapshot silently on first poll per the "no events
// on initial load" rule. Semantically this matches what would have fired if
// the ticket had been assigned after we started watching. Routes through the
// app-pool EventStore.Record (not bus.Publish) so downstream handlers don't
// double-create a task. In multi-mode this caller will be WithTx-wrapped by
// SKY-253 D9 so JWT claims are set for RLS; local-mode passes through
// assertLocalOrg cleanly without a wrapping tx.
//
// SKY-270: account ID flows through from the (caller-mutated) snap so the
// metadata carries the stable identifier the matcher needs. The display
// name in metadata is informational; matching keys on account ID.
func recordCarryOverAssignedEvent(ctx context.Context, events_ db.EventStore, orgID, entityID string, snap domain.JiraSnapshot) (string, error) {
	meta := jiraevents.JiraIssueAssignedMetadata{
		Assignee:          snap.Assignee,
		AssigneeAccountID: snap.AssigneeAccountID,
		IssueKey:          snap.Key,
		Project:           projectFromKey(snap.Key),
		IssueType:         snap.IssueType,
		Priority:          snap.Priority,
		Status:            snap.Status,
		Summary:           snap.Summary,
	}
	metaJSON, err := json.Marshal(meta)
	if err != nil {
		return "", err
	}
	eid := entityID
	return events_.Record(ctx, orgID, domain.Event{
		EntityID:     &eid,
		EventType:    domain.EventJiraIssueAssigned,
		MetadataJSON: string(metaJSON),
	})
}

// recordCarryOverAvailableEvent is the available-bucket analogue of
// recordCarryOverAssignedEvent: synthesizes a jira:issue:available event so
// the carry-over "queue" action on an unassigned ticket has a real event
// row to hang the task off. Mirrors the tracker's own emission path for
// first-discovered available tickets (diff.go). No actor identity carried
// on this event — the ticket is unassigned by definition.
func recordCarryOverAvailableEvent(ctx context.Context, events_ db.EventStore, orgID, entityID string, snap domain.JiraSnapshot) (string, error) {
	meta := jiraevents.JiraIssueAvailableMetadata{
		IssueKey:  snap.Key,
		Project:   projectFromKey(snap.Key),
		IssueType: snap.IssueType,
		Priority:  snap.Priority,
		Status:    snap.Status,
		Summary:   snap.Summary,
	}
	metaJSON, err := json.Marshal(meta)
	if err != nil {
		return "", err
	}
	eid := entityID
	return events_.Record(ctx, orgID, domain.Event{
		EntityID:     &eid,
		EventType:    domain.EventJiraIssueAvailable,
		MetadataJSON: string(metaJSON),
	})
}

// projectFromKey pulls "SKY" out of "SKY-123". Mirrors tracker.extractProject.
func projectFromKey(key string) string {
	if i := strings.IndexByte(key, '-'); i > 0 {
		return key[:i]
	}
	return key
}
