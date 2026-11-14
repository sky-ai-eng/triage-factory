// The multi-mode user→user handoff: a boundary only when it moves the task to
// another team.

package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"

	"github.com/sky-ai-eng/triage-factory/internal/db/dbseed"
	"github.com/sky-ai-eng/triage-factory/internal/db/pgtest"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// pgConversationBoundary reads one conversation's stamp off the admin pool.
func pgConversationBoundary(t *testing.T, h *pgtest.Harness, conversationID string) (ended bool, reason string) {
	t.Helper()
	if err := h.AdminDB.QueryRow(
		`SELECT ended_at IS NOT NULL, COALESCE(ended_reason, '') FROM conversations WHERE id = $1`,
		conversationID,
	).Scan(&ended, &reason); err != nil {
		t.Fatalf("read boundary of %s: %v", conversationID, err)
	}
	return ended, reason
}

// TestTaskReassign_TheConversationFollowsTheTeam: the conversation a person
// works a task in belongs to the task's team. A handoff that leaves the task
// on its team keeps that conversation un-ended, since the same work continues
// under a new owner. A handoff that moves the task to another team ends it as
// `reassigned`, leaves it with the team that made it, and mints one for the
// new holder on their team. Either way the task holds exactly one un-ended
// conversation, and the task read names it.
//
// The caller is the claimant throughout, who is on the first team only: after
// a cross-team handoff they can no longer see the task, which is why the
// boundary and the mint share the claim write's admin-pool transaction.
func TestTaskReassign_TheConversationFollowsTheTeam(t *testing.T) {
	type conversationRow struct {
		id, teamID, creator, convType string
	}
	unended := func(t *testing.T, h *pgtest.Harness, taskID string) []conversationRow {
		t.Helper()
		rows, err := h.AdminDB.Query(`
			SELECT id::text, team_id::text, COALESCE(creator_user_id::text, ''), type
			FROM conversations
			WHERE task_id = $1 AND ended_at IS NULL AND parent_conversation_id IS NULL
			ORDER BY started_at, id`, taskID)
		if err != nil {
			t.Fatalf("read un-ended conversations: %v", err)
		}
		defer func() { _ = rows.Close() }()
		var out []conversationRow
		for rows.Next() {
			var c conversationRow
			if err := rows.Scan(&c.id, &c.teamID, &c.creator, &c.convType); err != nil {
				t.Fatalf("scan conversation: %v", err)
			}
			out = append(out, c)
		}
		if err := rows.Err(); err != nil {
			t.Fatalf("un-ended conversations: %v", err)
		}
		return out
	}

	for _, tc := range []struct {
		name string
		// target seeds the handoff's target and answers their id and the team
		// the task lands on.
		target     func(t *testing.T, r *viewerRig, taskID string) (userID, teamID string)
		acrossTeam bool
	}{
		{
			name: "within the team",
			target: func(_ *testing.T, r *viewerRig, _ string) (string, string) {
				return r.member, r.teamID
			},
		},
		{
			name: "across teams",
			target: func(t *testing.T, r *viewerRig, taskID string) (string, string) {
				// A second team the task is visible to, and a person on that
				// team alone.
				other := pgtest.SeedTeam(t, r.h, r.orgID, "other-team")
				pgtest.MustExec(t, r.h.AdminDB, `INSERT INTO task_teams (task_id, team_id) VALUES ($1, $2)`, taskID, other)
				outsider := pgtest.SeedUser(t, r.h, "outsider")
				pgtest.AddOrgMember(t, r.h, outsider, r.orgID, other, "member", "member")
				return outsider, other
			},
			acrossTeam: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newViewerRig(t)

			var entityID, evtID, taskID string
			repoID := dbseed.GitHubRepository(t, r.h.AdminDB, r.orgID, "https://github.com", "octo", "reassign")
			if err := r.h.AdminDB.QueryRow(`
				INSERT INTO entities (org_id, source, source_id, kind, title, scope, repository_id)
				VALUES ($1, 'github', 'octo/reassign#1', 'pr', 'test pr', 'https://github.com', $2) RETURNING id
			`, r.orgID, repoID).Scan(&entityID); err != nil {
				t.Fatalf("seed entity: %v", err)
			}
			if err := r.h.AdminDB.QueryRow(`
				INSERT INTO events (org_id, entity_id, event_type) VALUES ($1, $2, 'github:pr:opened') RETURNING id
			`, r.orgID, entityID).Scan(&evtID); err != nil {
				t.Fatalf("seed event: %v", err)
			}
			if err := r.h.AdminDB.QueryRow(`
				INSERT INTO tasks (org_id, creator_user_id, team_id, entity_id, event_type, primary_event_id, claimed_by_user_id, status)
				VALUES ($1, $2, $3, $4, 'github:pr:opened', $5, $2, 'in_progress') RETURNING id
			`, r.orgID, r.admin, r.teamID, entityID, evtID).Scan(&taskID); err != nil {
				t.Fatalf("seed task: %v", err)
			}
			// The claimant's conversation, as their claim minted it, with a
			// turn on it so it is one a boundary owes a memory for.
			held := uuid.New().String()
			pgtest.MustExec(t, r.h.AdminDB, `
				INSERT INTO conversations (id, org_id, creator_user_id, team_id, visibility,
				                           type, origin, runtime, trigger_type, status, parked_at, task_id)
				VALUES ($1, $2, $3, $4, 'team', 'interactive', 'interactive', 'native', 'manual', 'open', now(), $5)
			`, held, r.orgID, r.admin, r.teamID, taskID)
			pgtest.MustExec(t, r.h.AdminDB,
				`INSERT INTO messages (org_id, conversation_id, role, subtype, content) VALUES ($1, $2, 'assistant', '', 'a turn')`,
				r.orgID, held)
			target, landsOn := tc.target(t, r, taskID)

			req := r.req(http.MethodPost, "/api/tasks/"+taskID+"/claim", r.admin,
				withTaskVersionIn(t, r.s, r.orgID, taskID, map[string]any{"target_user_id": target}))
			req.SetPathValue("id", taskID)
			rec := httptest.NewRecorder()
			r.s.handleTaskClaim(rec, req)
			if rec.Code != http.StatusOK {
				t.Fatalf("reassign = %d, want 200; body=%s", rec.Code, rec.Body.String())
			}
			var body struct {
				ConversationID  string `json:"conversation_id"`
				ClaimedByUserID string `json:"claimed_by_user_id"`
				TeamID          string `json:"team_id"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
				t.Fatalf("decode the reassign response: %v", err)
			}
			if body.ClaimedByUserID != target || body.TeamID != landsOn {
				t.Fatalf("task after the handoff: claimant %q on team %q; want %q on %q",
					body.ClaimedByUserID, body.TeamID, target, landsOn)
			}

			live := unended(t, r.h, taskID)
			if len(live) != 1 {
				t.Fatalf("un-ended conversations on the task = %+v, want exactly one", live)
			}
			if body.ConversationID != live[0].id {
				t.Errorf("the response names conversation_id %q, want the task's conversation %q", body.ConversationID, live[0].id)
			}

			ended, reason := pgConversationBoundary(t, r.h, held)
			if !tc.acrossTeam {
				if ended || reason != "" {
					t.Errorf("%s boundary = (ended=%v, reason=%q), want none: a handoff within the team is not a boundary",
						held, ended, reason)
				}
				if live[0].id != held {
					t.Errorf("the task's conversation = %s, want the one it held, %s", live[0].id, held)
				}
				return
			}

			if !ended || reason != string(domain.EndedReassigned) {
				t.Errorf("%s boundary = (ended=%v, reason=%q), want ended as %q", held, ended, reason, domain.EndedReassigned)
			}
			// The old conversation stays with the team that made it.
			var heldTeam string
			if err := r.h.AdminDB.QueryRow(`SELECT team_id::text FROM conversations WHERE id = $1`, held).Scan(&heldTeam); err != nil {
				t.Fatalf("read the old conversation's team: %v", err)
			}
			if heldTeam != r.teamID {
				t.Errorf("the old conversation moved to team %s, want it left on %s", heldTeam, r.teamID)
			}
			minted := live[0]
			if minted.id == held {
				t.Fatal("the task's conversation is still the old team's")
			}
			if minted.teamID != landsOn || minted.creator != target || minted.convType != domain.ConversationTypeInteractive {
				t.Errorf("minted conversation = %+v; want an interactive one on team %s created by the target %s",
					minted, landsOn, target)
			}
			// It has not run, so the route's cleanup neither stopped nor
			// ended it.
			var stopRequested bool
			var messages int
			if err := r.h.AdminDB.QueryRow(`
				SELECT stop_requested_at IS NOT NULL,
				       (SELECT COUNT(*) FROM messages m WHERE m.conversation_id = c.id)
				FROM conversations c WHERE c.id = $1`, minted.id).Scan(&stopRequested, &messages); err != nil {
				t.Fatalf("read the minted conversation: %v", err)
			}
			if stopRequested || messages != 0 {
				t.Errorf("the route's cleanup touched the minted conversation: stop requested = %v, messages = %d", stopRequested, messages)
			}
		})
	}
}
