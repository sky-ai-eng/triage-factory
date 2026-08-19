package server

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/server/httpx"
	"github.com/sky-ai-eng/triage-factory/pkg/websocket"
)

// taskActionRequest is shared by the two justified verb routes. Claim owns
// assignment (including reassignment); delegate owns spawning. Neither is a
// disguised field update.
type taskActionRequest struct {
	TargetUserID string `json:"target_user_id,omitempty"`
	BlueprintID  string `json:"blueprint_id,omitempty"`
	HesitationMs int    `json:"hesitation_ms,omitempty"`
}

func (s *Server) handleTaskClaim(w http.ResponseWriter, r *http.Request) {
	var body taskActionRequest
	if !httpx.DecodeJSONStrict(w, r, &body) || !validHesitationField(w, body.HesitationMs) {
		return
	}
	action := "claim"
	if body.TargetUserID != "" {
		action = "reassign"
	}
	s.handleTaskResponsibility(w, r, swipeRequest{Action: action, TargetUserID: body.TargetUserID, HesitationMs: body.HesitationMs})
}

func (s *Server) handleTaskDelegate(w http.ResponseWriter, r *http.Request) {
	var body taskActionRequest
	if !httpx.DecodeJSONStrict(w, r, &body) || !validHesitationField(w, body.HesitationMs) {
		return
	}
	s.handleTaskResponsibility(w, r, swipeRequest{Action: "delegate", BlueprintID: body.BlueprintID, HesitationMs: body.HesitationMs})
}

func (s *Server) handleTaskResponsibility(w http.ResponseWriter, r *http.Request, req swipeRequest) {
	orgID, ok := s.requireOrg(w, r)
	if !ok {
		return
	}
	userID := ClaimsFrom(r.Context()).Subject
	id, ok := taskIDOr404(w, r)
	if !ok || !s.az.RequireTaskWrite(w, r, orgID, userID, id) {
		return
	}
	response := map[string]any{}
	switch req.Action {
	case "claim":
		status, jc, accepted := s.swipeClaim(w, r, orgID, userID, id, req)
		if !accepted {
			return
		}
		response["status"] = status
		s.swipeTeardownConversations(r, orgID, userID, id, req.Action)
		// Jira assignment synchronization is deliberately best-effort: the local
		// guarded claim is authoritative and an upstream outage cannot roll it back.
		if jc != nil {
			s.swipeJiraClaimSync(r, orgID, userID, id, jc)
		}
	case "reassign":
		status, accepted := s.swipeReassign(w, r, orgID, userID, id, req)
		if !accepted {
			return
		}
		response["status"] = status
	case "delegate":
		status, accepted := s.swipeDelegate(w, r, orgID, userID, id, req)
		if !accepted {
			return
		}
		response["status"] = status
		s.swipeTeardownConversations(r, orgID, userID, id, req.Action)
		if s.spawner != nil && !s.swipeTriggerDelegation(w, r, orgID, userID, id, req, response) {
			return
		}
	}
	writeJSON(w, http.StatusOK, response)
}

type taskPatchRequest struct {
	Status       *string         `json:"status,omitempty"`
	SnoozeUntil  json.RawMessage `json:"snooze_until,omitempty"`
	HesitationMs *int            `json:"hesitation_ms,omitempty"`
}

func (s *Server) handleTaskPatch(w http.ResponseWriter, r *http.Request) {
	var body taskPatchRequest
	if !httpx.DecodeJSONStrict(w, r, &body) {
		return
	}
	if body.Status == nil && len(body.SnoozeUntil) == 0 {
		httpx.WriteErrors(w, http.StatusBadRequest, httpx.ErrorItem{Reason: httpx.ReasonMissingField, Message: "at least one of status or snooze_until is required"})
		return
	}
	if body.HesitationMs != nil && !validHesitationField(w, *body.HesitationMs) {
		return
	}
	orgID, ok := s.requireOrg(w, r)
	if !ok {
		return
	}
	userID := ClaimsFrom(r.Context()).Subject
	id, ok := taskIDOr404(w, r)
	if !ok || !s.az.RequireTaskWrite(w, r, orgID, userID, id) {
		return
	}
	var task *domain.Task
	if err := s.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error { var e error; task, e = tx.Tasks.Get(r.Context(), orgID, id); return e }); err != nil {
		internalError(w, "tasks", err)
		return
	}
	if task == nil {
		notFound(w, "task")
		return
	}
	if task.Status == "done" || task.Status == "dismissed" {
		httpx.WriteErrors(w, http.StatusConflict, httpx.ErrorItem{Reason: httpx.ReasonAlreadyTerminal, Message: "terminal tasks cannot be changed"})
		return
	}
	if len(body.SnoozeUntil) != 0 {
		if bytes.Equal(bytes.TrimSpace(body.SnoozeUntil), []byte("null")) {
			ok, err := s.requeueTaskOnly(r, orgID, userID, id)
			if err != nil {
				internalError(w, "tasks", err)
				return
			}
			if !ok {
				conflict(w, "task changed; refetch and retry")
				return
			}
			writeJSON(w, http.StatusOK, map[string]any{"status": "queued", "snooze_until": nil})
			return
		}
		var raw string
		if err := json.Unmarshal(body.SnoozeUntil, &raw); err != nil {
			httpx.WriteErrors(w, http.StatusBadRequest, httpx.ErrorItem{Reason: httpx.ReasonInvalidField, Message: "snooze_until must be an RFC3339 timestamp or null", Field: "snooze_until"})
			return
		}
		until, err := time.Parse(time.RFC3339, raw)
		if err != nil {
			httpx.WriteErrors(w, http.StatusBadRequest, httpx.ErrorItem{Reason: httpx.ReasonInvalidField, Message: "snooze_until must be an RFC3339 timestamp", Field: "snooze_until"})
			return
		}
		if !until.After(time.Now()) {
			httpx.WriteErrors(w, http.StatusUnprocessableEntity, httpx.ErrorItem{Reason: httpx.ReasonOutOfRange, Message: "snooze_until must be in the future", Field: "snooze_until"})
			return
		}
		ms := 0
		if body.HesitationMs != nil {
			ms = *body.HesitationMs
		}
		var changed bool
		sentinel := errors.New("snooze conflict")
		err = s.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
			var e error
			changed, e = tx.Swipes.SnoozeTask(r.Context(), orgID, id, until, ms)
			if e == nil && !changed {
				return sentinel
			}
			return e
		})
		if err != nil && !errors.Is(err, sentinel) {
			internalError(w, "tasks", err)
			return
		}
		if !changed {
			conflict(w, "task changed; refetch and retry")
			return
		}
		s.ws.Broadcast(websocket.Event{Type: "task_updated", OrgID: orgID, Data: map[string]any{"task_id": id, "status": "snoozed"}})
		writeJSON(w, http.StatusOK, map[string]any{"status": "snoozed", "snooze_until": until.UTC().Format(time.RFC3339)})
		return
	}
	status := *body.Status
	if status != "in_progress" && status != "in_review" && status != "done" && status != "dismissed" {
		httpx.WriteErrors(w, http.StatusBadRequest, httpx.ErrorItem{Reason: httpx.ReasonInvalidField, Message: "status must be one of: in_progress, in_review, done, dismissed", Field: "status"})
		return
	}
	if status == "done" || status == "dismissed" {
		action := "complete"
		if status == "dismissed" {
			action = "dismiss"
		}
		ms := 0
		if body.HesitationMs != nil {
			ms = *body.HesitationMs
		}
		_, accepted := s.swipeLifecycle(w, r, orgID, userID, id, swipeRequest{Action: action, HesitationMs: ms})
		if !accepted {
			return
		}
		s.swipeTeardownConversations(r, orgID, userID, id, action)
		writeJSON(w, http.StatusOK, map[string]string{"status": status})
		return
	}
	var advanced bool
	if err := s.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		var e error
		advanced, e = tx.Tasks.AdvanceStatusForUser(r.Context(), orgID, id, userID, status)
		return e
	}); err != nil {
		internalError(w, "tasks", err)
		return
	}
	if !advanced {
		httpx.WriteErrors(w, http.StatusForbidden, httpx.ErrorItem{Reason: httpx.ReasonForbidden, Message: "status requires your active claim"})
		return
	}
	s.ws.Broadcast(websocket.Event{Type: "task_updated", OrgID: orgID, Data: map[string]any{"task_id": id, "status": status}})
	writeJSON(w, http.StatusOK, map[string]string{"status": status})
}

func (s *Server) requeueTaskOnly(r *http.Request, orgID, userID, id string) (bool, error) {
	var ok bool
	err := s.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		var e error
		ok, e = tx.Swipes.RequeueTask(r.Context(), orgID, id)
		return e
	})
	return ok, err
}
