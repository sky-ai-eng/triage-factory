package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"

	"github.com/sky-ai-eng/triage-factory/internal/auth"
	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	jiraevents "github.com/sky-ai-eng/triage-factory/internal/domain/events"
	"github.com/sky-ai-eng/triage-factory/internal/integrations"
	"github.com/sky-ai-eng/triage-factory/internal/jira"
	"github.com/sky-ai-eng/triage-factory/internal/server/httpx"
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

// stockDeck is the GET response, and it is the whole response in every state.
// Status distinguishes "the Jira poller hasn't finished its first cycle since
// the last config change" from "here is the deck"; the two partitions are
// always present, empty while polling, so a client parses one shape.
type stockDeck struct {
	Status    string        `json:"status"`
	Assigned  []stockTicket `json:"assigned"`
	Available []stockTicket `json:"available"`
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
// with open subtasks are skipped. Status is "polling" with both buckets empty
// while the Jira poller hasn't completed its first cycle since the last
// config change — snapshots are seeded on first poll.
//
// GET /api/jira/stock
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
	// Identity facts live on the users row, not the
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
	// when no filter is set rather than blocking a multi-team caller. A
	// malformed id is rejected outright — silently falling through to the
	// default team would retarget the deck instead of narrowing it.
	filterTeam, ok := teamscope.SingleParamStrict(w, r)
	if !ok {
		return
	}
	if err := s.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		var e error
		// A credential-store failure is a backend fault, not "no credentials":
		// swallowing it here used to answer "Jira not configured", telling the
		// user to re-enter a credential they have.
		creds, e = integrations.Load(r.Context(), tx.Secrets, orgID)
		if e != nil {
			return fmt.Errorf("load integration credentials: %w", e)
		}
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
		// Jira identity is host-scoped: look it up for the org's
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
	// "Configured" reads the auth-method marker (via JiraSystemConfig), not key
	// presence, so DC and Cloud orgs both gate correctly and consistently with
	// the resolver.
	if _, ok := integrations.JiraSystemConfig(creds); !ok || len(jiraRules) == 0 || localAccountID == "" {
		writeNotConfigured(w, "Jira not configured")
		return
	}

	if !s.jiraPollReady(r.Context(), orgID) {
		// Same keys as the ready answer, empty. A response whose shape
		// depends on its status makes every client write two parsers and
		// hope it picked the right one; the deck's own poll loop just
		// re-reads until status flips.
		writeJSON(w, http.StatusOK, stockDeck{
			Status:    "polling",
			Assigned:  []stockTicket{},
			Available: []stockTicket{},
		})
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
			stockLog.Warn("skipping entity, invalid snapshot", "entity", e.ID, "source_id", e.SourceID, "error", err)
			continue
		}
		// Subtask gate applies to both buckets — a parent ticket
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

		// "Is this assigned to me?" uses the Atlassian account ID
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
			baseTicket.PrefilledAction = prefillForAssigned(projectRule, snap.StatusRef())
			assigned = append(assigned, scored{baseTicket, snap.CreatedAt, e.CreatedAt.Format("2006-01-02T15:04:05Z07:00")})

		case isUnassigned && projectRule != nil && projectRule.PickupContains(snap.StatusRef()):
			baseTicket.Bucket = "available"
			baseTicket.PrefilledAction = "" // user decides
			available = append(available, scored{baseTicket, snap.CreatedAt, e.CreatedAt.Format("2006-01-02T15:04:05Z07:00")})

		default:
			// Assigned to someone else, unassigned but not in Pickup
			// (in-progress orphan, stale Done), or project has no
			// configured rules — no action in carry-over.
			if projectRule == nil {
				stockLog.Warn("skipping entity, project has no configured status rules", "key", snap.Key, "project", projectKey)
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

	writeJSON(w, http.StatusOK, stockDeck{
		Status:    "ready",
		Assigned:  assignedOut,
		Available: availableOut,
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
func prefillForAssigned(rule *domain.JiraProjectStatusRules, status domain.JiraStatusRef) string {
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

// The three carry-over action routes. Each is one operation on a list of
// issue keys, because the arms have nothing in common past the eligibility
// gate: queue mints a task off a synthesized event with no Jira write at all,
// claim makes two external Jira writes before it touches a row, and done
// transitions the ticket and closes the entity without minting anything. A
// single route with a per-row action discriminator meant one request could ask
// for all three and a caller could not tell which half of it had happened.

// maxStockBatch caps one carry-over submission. The deck is dealt whole from
// the team's tracked projects, so a legitimate batch is bounded by what a
// person selected; the cap is here so a hand-written body can't turn one
// request into thousands of sequential Jira round-trips.
const maxStockBatch = 500

// stockActionRequest is the body every carry-over action route takes.
type stockActionRequest struct {
	IssueKeys []string `json:"issue_keys"`
	// TeamID is the acting team the write picker supplied — the team
	// claimed/queued stock tasks are stamped under. Required in the UI
	// when the caller belongs to ≥2 teams; empty (sole-team fallback)
	// otherwise. The same value scopes the Jira rules the actions
	// validate against, so claim writes land on the picked team.
	TeamID string `json:"team_id,omitempty"`
}

// stockItemResult is one row of a batch's per-item accounting. Every
// submitted key appears exactly once, ok either way, so applied + failed =
// submitted holds by construction and no id is silently skipped. Errors
// carries the envelope's item shape, so a per-row failure is as
// machine-readable as a request-level one.
type stockItemResult struct {
	IssueKey string            `json:"issue_key"`
	OK       bool              `json:"ok"`
	Errors   []httpx.ErrorItem `json:"errors,omitempty"`
}

// stockFailed builds a one-item per-row failure.
func stockFailed(issueKey, reason, msg string) stockItemResult {
	return stockItemResult{IssueKey: issueKey, Errors: []httpx.ErrorItem{{Reason: reason, Message: msg}}}
}

// handleJiraStockQueue puts the named tickets on the team's queue: a
// synthesized event plus the task hanging off it, and no Jira write at all —
// the user is parking work to triage later, not picking it up.
//
// POST /api/jira/stock/queue
func (s *Server) handleJiraStockQueue(w http.ResponseWriter, r *http.Request) {
	s.applyStockAction(w, r, stockActionQueue)
}

// handleJiraStockClaim takes the named tickets: assigns each to the acting
// user in Jira, transitions it to the project's in-progress status, then mints
// the task and stamps the user claim.
//
// POST /api/jira/stock/claim
func (s *Server) handleJiraStockClaim(w http.ResponseWriter, r *http.Request) {
	s.applyStockAction(w, r, stockActionClaim)
}

// handleJiraStockDone closes out the named tickets: transitions each to the
// project's done status and closes the entity. No task is minted — this is
// cleanup for work that finished outside TF.
//
// POST /api/jira/stock/done
func (s *Server) handleJiraStockDone(w http.ResponseWriter, r *http.Request) {
	s.applyStockAction(w, r, stockActionDone)
}

// The carry-over actions. These are route identities now rather than a body
// field, but the strings still name the prefilled_action the deck serves.
const (
	stockActionQueue = "queue"
	stockActionClaim = "claim"
	stockActionDone  = "done"
)

// applyStockAction is the shared body of the three routes: resolve the acting
// team's Jira configuration once, then walk the batch applying one action.
//
// Eligibility is enforced server-side against the same team-scoped read the
// GET deck uses, so a stale tab or a hand-written body can't act on a ticket
// the deck would never have surfaced. Per-row failures are collected rather
// than failing the batch — a Jira transition that bounces on one ticket
// shouldn't discard the twenty that applied.
func (s *Server) applyStockAction(w http.ResponseWriter, r *http.Request, action string) {
	orgID, ok := s.requireOrg(w, r)
	if !ok {
		return
	}
	userID := ClaimsFrom(r.Context()).Subject

	var req stockActionRequest
	if !httpx.DecodeJSONStrict(w, r, &req) {
		return
	}
	issueKeys, ok := validateStockBatch(w, req.IssueKeys)
	if !ok {
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
		var e error
		// See handleJiraStockGet: a failed credential read is a 500, never
		// "not configured".
		creds, e = integrations.Load(r.Context(), tx.Secrets, orgID)
		if e != nil {
			return fmt.Errorf("load integration credentials: %w", e)
		}
		teamID, e = teamscope.ResolveActing(r.Context(), tx.Teams, tx.Users, orgID, userID, req.TeamID)
		if e != nil {
			return e
		}
		jiraRules, e = tx.JiraStatusRules.ListForTeam(r.Context(), teamID)
		if e != nil {
			return fmt.Errorf("list jira rules: %w", e)
		}
		// Jira identity is host-scoped: creds.JiraURL mirrors
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
	// "Configured" reads the auth-method marker (via JiraSystemConfig), not key
	// presence, so DC and Cloud orgs both gate correctly and consistently with
	// the resolver.
	if _, ok := integrations.JiraSystemConfig(creds); !ok || len(jiraRules) == 0 || localAccountID == "" {
		writeNotConfigured(w, "Jira not configured")
		return
	}

	// Claim (assign + transition) and done (transition) are user-initiated
	// Jira writes — they must act as the acting user, not the org service
	// account (AssignToSelf assigns to the token's user). A user with no
	// connected Jira can't perform either, and because the route is one
	// action that's a whole-request refusal rather than the same per-row
	// error repeated for every key. Queue makes no Jira write, so it must
	// not depend on secret-store availability at all: a transient token
	// lookup shouldn't 500 a pure queue.
	var jiraUserClient *jira.Client
	if action == stockActionClaim || action == stockActionDone {
		c, jerr := s.jiraResolver.ForUser(r.Context(), orgID, userID)
		if errors.Is(jerr, jira.ErrNoJiraUserCredential) {
			httpx.WriteErrors(w, http.StatusConflict, httpx.ErrorItem{
				Reason:  httpx.ReasonNotConfigured,
				Message: "connect your Jira to act on tickets",
			})
			return
		}
		if jerr != nil {
			internalError(w, "stock", jerr)
			return
		}
		jiraUserClient = c
	}

	// Batch-fetch, for O(1) per-item eligibility checks: (a) the Jira
	// entities that already have an active task, and (b) the team-scoped Jira
	// set the viewer is allowed to act on — the *same* ListActiveJiraTeamScoped
	// read the GET deck uses, so eligibility here can't diverge from what the
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

	batch := &stockBatch{
		orgID:            orgID,
		userID:           userID,
		teamID:           teamID,
		action:           action,
		jiraRules:        jiraRules,
		localAccountID:   localAccountID,
		localDisplayName: localDisplayName,
		jiraUserClient:   jiraUserClient,
		taskedEntityIDs:  taskedEntityIDs,
		scopedJiraIDs:    scopedJiraIDs,
	}

	results := make([]stockItemResult, 0, len(issueKeys))
	applied := 0
	for _, issueKey := range issueKeys {
		result := s.applyStockItem(r, batch, issueKey)
		if result.OK {
			applied++
		}
		results = append(results, result)
	}

	// Carry-over creates tasks without going through the poller, so no
	// system:poll:completed fires to wake the scorer via its event-bus
	// subscription. Poke it directly, but only for queued tasks — done
	// creates no task at all, and claim creates a status='queued' task with
	// the user claim col stamped, which UnscoredTasks picks up on its next
	// natural cycle anyway (scoring a user-claimed task is harmless dormant
	// work rather than a wrong-status skip).
	if action == stockActionQueue && applied > 0 && s.scorerTrigger != nil {
		s.scorerTrigger(orgID)
	}

	// Success toast when at least one ticket applied cleanly. The frontend
	// also shows a partial-failure warning toast if there are any failures;
	// this one only fires on at-least-one-success.
	if applied > 0 {
		toast.Success(s.ws, orgID, fmt.Sprintf("Carry-over applied: %d %s", applied, stockActionPastTense(action)))
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"applied": applied,
		"failed":  len(results) - applied,
		"results": results,
	})
}

// stockActionPastTense renders an action for the success toast.
func stockActionPastTense(action string) string {
	switch action {
	case stockActionClaim:
		return "claimed"
	case stockActionDone:
		return "closed"
	default:
		return "queued"
	}
}

// validateStockBatch enforces the request-level half of the batch policy: an
// empty list, an over-cap list, a blank or whitespace-padded key and a
// repeated key all fail the whole call, so the per-item accounting invariant
// (applied + failed = submitted) holds for every batch that actually runs, on
// the keys the caller actually sent. Dropping any of them mid-loop instead
// would make a five-key submission answer "applied: 3" with no mention of the
// other two.
func validateStockBatch(w http.ResponseWriter, raw []string) ([]string, bool) {
	var v httpx.Validation
	if len(raw) == 0 {
		v.Invalid("issue_keys", "issue_keys must contain at least one issue key")
	}
	if len(raw) > maxStockBatch {
		v.OutOfRange("issue_keys", fmt.Sprintf("issue_keys accepts at most %d keys per request", maxStockBatch))
	}
	seen := make(map[string]struct{}, len(raw))
	keys := make([]string, 0, len(raw))
	for _, key := range raw {
		switch {
		case strings.TrimSpace(key) == "":
			v.Invalid("issue_keys", "issue_keys contains a blank issue key")
			continue
		case key != strings.TrimSpace(key):
			// Rejected rather than trimmed. Every result row echoes the key
			// as submitted, which is how a client matches results back to
			// its request — silently rewriting one here would answer for a
			// key the caller never sent.
			v.Invalid("issue_keys", "issue_keys contains "+strings.TrimSpace(key)+" with surrounding whitespace")
			continue
		}
		if _, dup := seen[key]; dup {
			v.Invalid("issue_keys", "issue_keys contains "+key+" more than once")
			continue
		}
		seen[key] = struct{}{}
		keys = append(keys, key)
	}
	if v.Flush(w, http.StatusBadRequest) {
		return nil, false
	}
	return keys, true
}

// stockBatch is the per-request state every item in a carry-over batch reads:
// the acting identity and team, the resolved Jira rules and client, and the
// two eligibility index maps. Built once so a 500-key batch doesn't re-resolve
// any of it per row.
type stockBatch struct {
	orgID            string
	userID           string
	teamID           string
	action           string
	jiraRules        []domain.JiraProjectStatusRules
	localAccountID   string
	localDisplayName string
	// jiraUserClient is the acting user's own client, nil on the queue route
	// (which makes no Jira write).
	jiraUserClient  *jira.Client
	taskedEntityIDs map[string]struct{}
	scopedJiraIDs   map[string]struct{}
}

// applyStockItem runs one issue key through the eligibility gate and then the
// batch's action. Every return is a result row — nothing about one key ever
// aborts the batch — and every row is keyed by the SUBMITTED key rather than
// by whatever the entity or its snapshot calls the ticket, so a client can
// always match results back to what it sent.
func (s *Server) applyStockItem(r *http.Request, b *stockBatch, issueKey string) stockItemResult {
	result := func() stockItemResult {
		entity, snap, rule, bucket, failure := s.resolveStockTicket(r, b, issueKey)
		if failure != nil {
			return *failure
		}
		switch b.action {
		case stockActionQueue:
			return s.stockQueueTicket(r, b, entity, snap, bucket)
		case stockActionClaim:
			return s.stockClaimTicket(r, b, entity, snap, rule)
		default:
			return s.stockDoneTicket(r, b, entity, rule)
		}
	}()
	result.IssueKey = issueKey
	return result
}

// stockBucket is which half of the deck a ticket sits in, as re-derived
// server-side at action time rather than trusted from the client.
type stockBucket int

const (
	// bucketAssigned: the ticket is assigned to the acting user.
	bucketAssigned stockBucket = iota
	// bucketAvailable: the ticket is unassigned and in a Pickup-rule status.
	bucketAvailable
)

// resolveStockTicket loads a submitted issue key and enforces every
// eligibility rule the GET deck applies, plus the per-action ones. It returns
// the entity, its snapshot, the project's status rules (nil when the project
// has none configured) and the bucket the ticket is in — or a populated
// failure row, in which case the rest is unusable.
func (s *Server) resolveStockTicket(r *http.Request, b *stockBatch, issueKey string) (*domain.Entity, domain.JiraSnapshot, *domain.JiraProjectStatusRules, stockBucket, *stockItemResult) {
	var snap domain.JiraSnapshot
	fail := func(reason, msg string) (*domain.Entity, domain.JiraSnapshot, *domain.JiraProjectStatusRules, stockBucket, *stockItemResult) {
		f := stockFailed(issueKey, reason, msg)
		return nil, snap, nil, bucketAssigned, &f
	}

	var entity *domain.Entity
	if err := s.tx.WithTx(r.Context(), b.orgID, b.userID, func(tx db.TxStores) error {
		var e error
		entity, e = tx.Entities.GetBySource(r.Context(), b.orgID, "jira", issueKey)
		return e
	}); err != nil {
		stockLog.Error("stock: entity lookup failed", "issue", issueKey, "error", err)
		return fail(httpx.ReasonInternal, "failed to load ticket"+httpx.LocalDetail(err))
	}
	if entity == nil {
		return fail(httpx.ReasonNotFound, "ticket not found")
	}

	// Team-scope gate: mirror the GET deck (ListActiveJiraTeamScoped). A
	// ticket whose project isn't tracked by the viewer's team(s) isn't on
	// their deck and must not be actionable here — even if it's assigned to
	// them — which closes the bypass where a user submits an off-team issue
	// key. In local mode the scoped set is the full active Jira set, so this
	// gate is a no-op there.
	if _, inScope := b.scopedJiraIDs[entity.ID]; !inScope {
		return fail(httpx.ReasonNotFound, "ticket is outside your team's tracked projects")
	}

	// Enforce the same eligibility rules as the GET list. Prevents acting
	// on tickets that shouldn't be in carry-over at all — stale frontend
	// state, tampered requests, or tickets that changed since GET.
	if entity.SnapshotJSON == "" || entity.SnapshotJSON == "{}" {
		return fail(httpx.ReasonConflict, "no snapshot yet")
	}
	if err := json.Unmarshal([]byte(entity.SnapshotJSON), &snap); err != nil {
		stockLog.Error("stock: invalid snapshot", "issue", issueKey, "entity", entity.ID, "error", err)
		return fail(httpx.ReasonInternal, "invalid snapshot")
	}

	// Per-project rule lookup. Tickets whose project_key has no configured
	// rules fall through every status branch below — there's no terminal
	// check and no available bucket — so the only action they support is
	// queue on an assigned ticket (the synthesized event doesn't depend on
	// status rules).
	rule := domain.RuleForProject(b.jiraRules, projectFromKey(snap.Key))

	isSelf := (snap.AssigneeAccountID != "" && snap.AssigneeAccountID == b.localAccountID) ||
		(snap.AssigneeAccountID == "" && snap.Assignee == b.localDisplayName)
	isAvailable := snap.Assignee == "" && rule != nil && rule.PickupContains(snap.StatusRef())
	if !isSelf && !isAvailable {
		return fail(httpx.ReasonConflict, "ticket is not assigned to you and not in the available pickup queue")
	}
	bucket := bucketAssigned
	if !isSelf {
		bucket = bucketAvailable
	}

	// Defensive subtask gate: queue/claim on a parent with open subtasks
	// would create the exact non-atomic task the main flow works hard to
	// suppress. The GET handler already filters these out so legitimate UI
	// flows never submit them, but subtasks could be added between GET and
	// POST, or the request could come from a stale frontend. done is still
	// allowed on the assigned branch — closing a parent with dangling
	// subtasks is a valid cleanup action.
	if snap.OpenSubtaskCount > 0 && b.action != stockActionDone {
		return fail(httpx.ReasonConflict, "ticket has open subtasks — delegate those atomic subtasks directly rather than the parent")
	}

	// The available bucket never makes sense for done — closing an
	// unassigned ticket from carry-over isn't a supported cleanup (done is
	// for orphan cleanup on your own assigned tickets that are already in a
	// Done-rule status).
	if bucket == bucketAvailable && b.action == stockActionDone {
		return fail(httpx.ReasonConflict, "ticket is not assigned to you; done is only for cleaning up your own already-complete tickets")
	}

	// Assigned bucket: tickets in Done.Members are allowed through for done
	// (a no-op guard skips the Jira transition when the status is already a
	// Done member); queue/claim on an already-done ticket is pointless, so
	// reject those outright.
	if bucket == bucketAssigned && rule != nil && rule.DoneContains(snap.StatusRef()) && b.action != stockActionDone {
		return fail(httpx.ReasonConflict, "ticket is already in a done status — only the done action is valid")
	}

	if _, hasTask := b.taskedEntityIDs[entity.ID]; hasTask {
		return fail(httpx.ReasonConflict, "ticket already has an active task")
	}
	return entity, snap, rule, bucket, nil
}

// stockQueueTicket synthesizes the event a carry-over ticket has no upstream
// for and mints the task off it. No Jira write.
func (s *Server) stockQueueTicket(r *http.Request, b *stockBatch, entity *domain.Entity, snap domain.JiraSnapshot, bucket stockBucket) stockItemResult {
	if err := s.tx.WithTx(r.Context(), b.orgID, b.userID, func(tx db.TxStores) error {
		// Available tickets synthesize jira:issue:available (they're
		// unassigned — a synthesized jira:issue:assigned would be a lie).
		// Assigned tickets use jira:issue:assigned.
		eventType := domain.EventJiraIssueAssigned
		record := recordCarryOverAssignedEvent
		if bucket == bucketAvailable {
			eventType = domain.EventJiraIssueAvailable
			record = recordCarryOverAvailableEvent
		}
		eventID, e := record(r.Context(), tx.Events, b.orgID, entity.ID, snap)
		if e != nil {
			return e
		}
		_, _, e = tx.Tasks.FindOrCreate(r.Context(), b.orgID, b.teamID, entity.ID, eventType, "", eventID, 0.5)
		return e
	}); err != nil {
		stockLog.Error("stock: queue failed", "issue", snap.Key, "error", err)
		return stockFailed(snap.Key, httpx.ReasonInternal, "could not queue the ticket"+httpx.LocalDetail(err))
	}
	return stockItemResult{IssueKey: snap.Key, OK: true}
}

// stockClaimTicket assigns the ticket to the acting user in Jira, transitions
// it to the project's in-progress status, then mints the task and stamps the
// user claim.
func (s *Server) stockClaimTicket(r *http.Request, b *stockBatch, entity *domain.Entity, snap domain.JiraSnapshot, rule *domain.JiraProjectStatusRules) stockItemResult {
	if rule == nil || rule.InProgressCanonical.IsZero() {
		return stockFailed(snap.Key, httpx.ReasonNotConfigured, "in_progress canonical status not configured for this project")
	}

	// Do Jira mutations first: if Jira fails we bail before touching the task
	// table, so there's no claimed-task orphan pointing at a Jira issue that
	// never got assigned or transitioned. Claim-guard pattern skips the API
	// calls when state is already correct — containment against
	// InProgress.Members so a ticket in any in-progress variant isn't
	// transitioned back to canonical. For available tickets the state check is
	// a no-op (they're unassigned by definition), but GetClaimState is cheap
	// and keeps one code path for both buckets.
	state := b.jiraUserClient.GetClaimState(r.Context(), snap.Key)
	if state == nil || !state.AssignedToSelf {
		if err := b.jiraUserClient.AssignToSelf(r.Context(), snap.Key); err != nil {
			return stockFailed(snap.Key, httpx.ReasonUpstreamRejected, "assign: "+err.Error())
		}
	}
	if state == nil || !rule.InProgressContains(claimStatusRef(state)) {
		if err := b.jiraUserClient.TransitionTo(r.Context(), snap.Key,
			jira.Status{ID: rule.InProgressCanonical.ID, Name: rule.InProgressCanonical.Name}); err != nil {
			return stockFailed(snap.Key, httpx.ReasonUpstreamRejected, "transition: "+err.Error())
		}
		snap.Status = rule.InProgressCanonical.Name
	} else {
		snap.Status = state.StatusName
	}

	// Refresh the snap with the known post-mutation state so the synthesized
	// event metadata matches the ticket's actual Jira state at the moment of
	// claim. Both the display name (for UI) and the account ID (for predicate
	// matching) flip to self.
	snap.Assignee = b.localDisplayName
	snap.AssigneeAccountID = b.localAccountID

	// Both buckets end with a jira:issue:assigned event — after the
	// AssignToSelf call the user is the assignee in Jira too, so the event
	// metadata is accurate for either starting state.
	var claimOK bool
	if err := s.tx.WithTx(r.Context(), b.orgID, b.userID, func(tx db.TxStores) error {
		eventID, e := recordCarryOverAssignedEvent(r.Context(), tx.Events, b.orgID, entity.ID, snap)
		if e != nil {
			return fmt.Errorf("record event: %w", e)
		}
		task, _, e := tx.Tasks.FindOrCreate(r.Context(), b.orgID, b.teamID, entity.ID, domain.EventJiraIssueAssigned, "", eventID, 0.5)
		if e != nil {
			return e
		}
		// Claim is on the responsibility axis, not status. Stamp the user
		// claim optimistically — if the task pre-existed in some non-queued
		// state or was already claimed by someone else, surface that as a
		// failed row rather than stealing.
		claimOK, e = tx.Tasks.ClaimQueuedForUser(r.Context(), b.orgID, task.ID, b.userID)
		return e
	}); err != nil {
		stockLog.Error("stock: claim failed", "issue", snap.Key, "error", err)
		return stockFailed(snap.Key, httpx.ReasonInternal, "could not claim the ticket"+httpx.LocalDetail(err))
	}
	if !claimOK {
		return stockFailed(snap.Key, httpx.ReasonConflict, "task already claimed or no longer queued")
	}
	return stockItemResult{IssueKey: snap.Key, OK: true}
}

// stockDoneTicket transitions the ticket to the project's done status and
// closes the entity. No task is minted.
func (s *Server) stockDoneTicket(r *http.Request, b *stockBatch, entity *domain.Entity, rule *domain.JiraProjectStatusRules) stockItemResult {
	issueKey := entity.SourceID
	if rule == nil || rule.DoneCanonical.IsZero() {
		return stockFailed(issueKey, httpx.ReasonNotConfigured, "done canonical status not configured for this project")
	}
	// Skip the transition when the ticket is already in any Done member (not
	// just the canonical) — a ticket in "Verified" when
	// Done.Members=[Resolved,Verified] is already done from TF's perspective;
	// transitioning to Resolved would be a no-op at best and a workflow
	// violation at worst.
	state := b.jiraUserClient.GetClaimState(r.Context(), issueKey)
	if state == nil || !rule.DoneContains(claimStatusRef(state)) {
		if err := b.jiraUserClient.TransitionTo(r.Context(), issueKey,
			jira.Status{ID: rule.DoneCanonical.ID, Name: rule.DoneCanonical.Name}); err != nil {
			return stockFailed(issueKey, httpx.ReasonUpstreamRejected, "transition: "+err.Error())
		}
	}
	if err := s.tx.WithTx(r.Context(), b.orgID, b.userID, func(tx db.TxStores) error {
		_, e := tx.Entities.MarkClosed(r.Context(), b.orgID, entity.ID)
		return e
	}); err != nil {
		stockLog.Error("stock: close failed", "issue", issueKey, "entity", entity.ID, "error", err)
		return stockFailed(issueKey, httpx.ReasonInternal, "could not close the ticket"+httpx.LocalDetail(err))
	}
	return stockItemResult{IssueKey: issueKey, OK: true}
}

// recordCarryOverAssignedEvent writes a synthesized jira:issue:assigned event
// for a carry-over ticket and returns the event ID. Tasks require a non-null
// primary_event_id FK to events.id, but carry-over has no upstream event —
// the tracker seeded the snapshot silently on first poll per the "no events
// on initial load" rule. Semantically this matches what would have fired if
// the ticket had been assigned after we started watching. Routes through the
// app-pool EventStore.Record (not bus.Publish) so downstream handlers don't
// double-create a task. In multi-mode this caller will be WithTx-wrapped
// so JWT claims are set for RLS; local-mode passes through
// assertLocalOrg cleanly without a wrapping tx.
//
// Account ID flows through from the (caller-mutated) snap so the
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

// projectFromKey pulls the project prefix out of a Jira key (e.g. "ABC" out
// of "ABC-123"). Mirrors tracker.extractProject.
func projectFromKey(key string) string {
	if i := strings.IndexByte(key, '-'); i > 0 {
		return key[:i]
	}
	return key
}

// claimStatusRef pairs a live claim read's status name with its id, so the
// membership tests compare on the id whenever both sides carry one.
func claimStatusRef(state *jira.ClaimState) domain.JiraStatusRef {
	if state == nil {
		return domain.JiraStatusRef{}
	}
	return domain.JiraStatusRef{ID: state.StatusID, Name: state.StatusName}
}
