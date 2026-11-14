// A person's claim in multi mode: the mint, the takeover's boundary and the
// firings' cancellation requests all run under the claimant's own claims, on
// the claim's transaction, so each has to clear the policies a team member's
// session is held to.

package server

import (
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/db/dbseed"
	"github.com/sky-ai-eng/triage-factory/internal/db/pgtest"
	pgstore "github.com/sky-ai-eng/triage-factory/internal/db/postgres"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

func TestPersonClaim_Postgres_MintsUnderTheClaimantsClaims(t *testing.T) {
	for _, tc := range []struct {
		name      string
		fromAgent bool
	}{
		{name: "from the queue"},
		{name: "from the agent, with a firing queued", fromAgent: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newViewerRig(t)
			admin := r.h.AdminDB

			var entityID, eventID, taskID string
			repoID := dbseed.GitHubRepository(t, admin, r.orgID, "https://github.com", "octo", "claim")
			if err := admin.QueryRow(`
				INSERT INTO entities (org_id, source, source_id, kind, title, scope, repository_id)
				VALUES ($1, 'github', 'octo/claim#1', 'pr', 'test pr', 'https://github.com', $2) RETURNING id
			`, r.orgID, repoID).Scan(&entityID); err != nil {
				t.Fatalf("seed entity: %v", err)
			}
			if err := admin.QueryRow(`
				INSERT INTO events (org_id, entity_id, event_type) VALUES ($1, $2, 'github:pr:opened') RETURNING id
			`, r.orgID, entityID).Scan(&eventID); err != nil {
				t.Fatalf("seed event: %v", err)
			}
			if err := admin.QueryRow(`
				INSERT INTO tasks (org_id, creator_user_id, team_id, entity_id, event_type, primary_event_id)
				VALUES ($1, $2, $3, $4, 'github:pr:opened', $5) RETURNING id
			`, r.orgID, r.admin, r.teamID, entityID, eventID).Scan(&taskID); err != nil {
				t.Fatalf("seed task: %v", err)
			}

			var agentConversation string
			if tc.fromAgent {
				agentID := uuid.New().String()
				pgtest.MustExec(t, admin, `INSERT INTO agents (id, org_id, display_name) VALUES ($1, $2, 'Bot')`, agentID, r.orgID)
				pgtest.MustExec(t, admin,
					`UPDATE tasks SET claimed_by_agent_id = $2, status = 'in_progress' WHERE id = $1`, taskID, agentID)
				// The agent's conversation, mid-flight.
				agentConversation = uuid.New().String()
				pgtest.MustExec(t, admin, `
					INSERT INTO conversations (id, org_id, creator_user_id, team_id, visibility,
					                           type, origin, runtime, trigger_type, status, task_id, actor_agent_id)
					VALUES ($1, $2, $3, $4, 'team', 'delegation', 'interactive', 'native', 'manual', NULL, $5, $6)
				`, agentConversation, r.orgID, r.admin, r.teamID, taskID, agentID)

				// A firing queued behind it.
				promptID, blueprintID, triggerID := "p_"+uuid.NewString(), uuid.New().String(), uuid.New().String()
				pgtest.MustExec(t, admin, `
					INSERT INTO prompts (id, org_id, creator_user_id, team_id, name, body, source, allowed_tools, created_at, updated_at)
					VALUES ($1, $2, $3, $4, 'Claim Test', 'body', 'user', '', now(), now())`, promptID, r.orgID, r.admin, r.teamID)
				pgtest.MustExec(t, admin, `
					INSERT INTO blueprints (id, org_id, creator_user_id, team_id, name, source, created_at, updated_at)
					VALUES ($1, $2, $3, $4, 'Claim BP', 'user', now(), now())`, blueprintID, r.orgID, r.admin, r.teamID)
				pgtest.MustExec(t, admin, `
					INSERT INTO blueprint_steps (org_id, team_id, blueprint_id, step_index, step_prompt_id)
					VALUES ($1, $2, $3, 0, $4)`, r.orgID, r.teamID, blueprintID, promptID)
				pgtest.MustExec(t, admin, `
					INSERT INTO event_handlers (id, org_id, creator_user_id, team_id, kind, event_type, source, blueprint_id, breaker_threshold, min_autonomy_suitability, enabled, created_at, updated_at)
					VALUES ($1, $2, $3, $4, 'trigger', 'github:pr:opened', 'user', $5, 4, 0, true, now(), now())`,
					triggerID, r.orgID, r.admin, r.teamID, blueprintID)
				inserted, _, err := pgstore.New(admin, r.h.AppDB, pgtest.SecretKey).PendingFirings.Enqueue(
					t.Context(), r.orgID, entityID, taskID, triggerID, eventID, db.AgentClaimStamp{})
				if err != nil || !inserted {
					t.Fatalf("queue a firing: inserted=%v err=%v", inserted, err)
				}
			}

			// The claimant is a plain member of the task's team.
			req := r.req(http.MethodPost, "/api/tasks/"+taskID+"/claim", r.member,
				withTaskVersionIn(t, r.s, r.orgID, taskID, map[string]any{}))
			req.SetPathValue("id", taskID)
			rec := httptest.NewRecorder()
			r.s.handleTaskClaim(rec, req)
			if rec.Code != http.StatusOK {
				t.Fatalf("claim = %d, want 200; body=%s", rec.Code, rec.Body.String())
			}
			var body struct {
				ConversationID  string `json:"conversation_id"`
				ClaimedByUserID string `json:"claimed_by_user_id"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
				t.Fatalf("decode the claim response: %v", err)
			}
			if body.ClaimedByUserID != r.member {
				t.Fatalf("claimed_by_user_id = %q, want the claimant %q", body.ClaimedByUserID, r.member)
			}

			rows, err := admin.Query(`
				SELECT id::text, type, origin, runtime, COALESCE(creator_user_id::text, ''), team_id::text, COALESCE(status, '')
				FROM conversations
				WHERE task_id = $1 AND ended_at IS NULL AND parent_conversation_id IS NULL`, taskID)
			if err != nil {
				t.Fatalf("read un-ended conversations: %v", err)
			}
			defer func() { _ = rows.Close() }()
			type conversationRow struct{ id, convType, origin, runtime, creator, teamID, status string }
			var live []conversationRow
			for rows.Next() {
				var c conversationRow
				if err := rows.Scan(&c.id, &c.convType, &c.origin, &c.runtime, &c.creator, &c.teamID, &c.status); err != nil {
					t.Fatalf("scan conversation: %v", err)
				}
				live = append(live, c)
			}
			if err := rows.Err(); err != nil {
				t.Fatalf("un-ended conversations: %v", err)
			}
			if len(live) != 1 {
				t.Fatalf("un-ended conversations on the task = %+v, want exactly one", live)
			}
			minted := live[0]
			want := conversationRow{
				id: minted.id, convType: domain.ConversationTypeInteractive, origin: domain.ConversationOriginInteractive,
				runtime: domain.ConversationRuntimeNative, creator: r.member, teamID: r.teamID, status: domain.StatusOpen,
			}
			if minted != want {
				t.Errorf("minted conversation = %+v, want %+v", minted, want)
			}
			if body.ConversationID != minted.id {
				t.Errorf("the response names conversation_id %q, want %q", body.ConversationID, minted.id)
			}

			if !tc.fromAgent {
				return
			}
			if ended, reason := pgConversationBoundary(t, r.h, agentConversation); !ended || reason != string(domain.EndedTakenOver) {
				t.Errorf("the agent's conversation boundary = (ended=%v, reason=%q), want ended as %q",
					ended, reason, domain.EndedTakenOver)
			}
			var requestedAt sql.NullTime
			var by, reason sql.NullString
			if err := admin.QueryRow(`
				SELECT cancel_requested_at, cancel_requested_by, cancel_reason FROM pending_firings WHERE task_id = $1`, taskID,
			).Scan(&requestedAt, &by, &reason); err != nil {
				t.Fatalf("read the firing: %v", err)
			}
			if !requestedAt.Valid || by.String != r.member || reason.String != personClaimCancelReason {
				t.Errorf("firing after the takeover: requested=%v by=%q reason=%q; want a request by the claimant, reason %q",
					requestedAt.Valid, by.String, reason.String, personClaimCancelReason)
			}
		})
	}
}
