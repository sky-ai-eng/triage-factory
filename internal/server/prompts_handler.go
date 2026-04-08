package server

import (
	"encoding/json"
	"net/http"

	"github.com/google/uuid"
	"github.com/sky-ai-eng/todo-tinder/internal/db"
	"github.com/sky-ai-eng/todo-tinder/internal/domain"
)

func (s *Server) handleEventTypes(w http.ResponseWriter, r *http.Request) {
	types, err := db.ListEventTypes(s.db)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	if types == nil {
		types = []domain.EventType{}
	}
	writeJSON(w, http.StatusOK, types)
}

func (s *Server) handleEventTypeToggle(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var req struct {
		Enabled bool `json:"enabled"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid request body"})
		return
	}
	if err := db.UpdateEventTypeEnabled(s.db, id, req.Enabled); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"id": id, "enabled": req.Enabled})
}

func (s *Server) handleEventTypeReorder(w http.ResponseWriter, r *http.Request) {
	var ids []string
	if err := json.NewDecoder(r.Body).Decode(&ids); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "expected array of event type IDs"})
		return
	}
	if err := db.ReorderEventTypes(s.db, ids); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "reordered"})
}

func (s *Server) handleAllBindings(w http.ResponseWriter, r *http.Request) {
	bindings, err := db.ListAllBindings(s.db)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	if bindings == nil {
		bindings = []domain.PromptBinding{}
	}
	writeJSON(w, http.StatusOK, bindings)
}

func (s *Server) handlePromptsList(w http.ResponseWriter, r *http.Request) {
	prompts, err := db.ListPrompts(s.db)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	if prompts == nil {
		prompts = []domain.Prompt{}
	}
	writeJSON(w, http.StatusOK, prompts)
}

func (s *Server) handlePromptGet(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	prompt, err := db.GetPrompt(s.db, id)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	if prompt == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "prompt not found"})
		return
	}

	bindings, _ := db.GetBindingsForPrompt(s.db, id)
	if bindings == nil {
		bindings = []domain.PromptBinding{}
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"prompt":   prompt,
		"bindings": bindings,
	})
}

type createPromptRequest struct {
	Name     string                 `json:"name"`
	Body     string                 `json:"body"`
	Bindings []domain.PromptBinding `json:"bindings"`
}

func (s *Server) handlePromptCreate(w http.ResponseWriter, r *http.Request) {
	var req createPromptRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid request body"})
		return
	}
	if req.Name == "" || req.Body == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "name and body are required"})
		return
	}

	id := uuid.New().String()
	prompt := domain.Prompt{
		ID:     id,
		Name:   req.Name,
		Body:   req.Body,
		Source: "user",
	}

	if err := db.CreatePrompt(s.db, prompt); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}

	if len(req.Bindings) > 0 {
		for i := range req.Bindings {
			req.Bindings[i].PromptID = id
		}
		if err := db.SetBindingsForPrompt(s.db, id, req.Bindings); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
	}

	created, _ := db.GetPrompt(s.db, id)
	writeJSON(w, http.StatusCreated, created)
}

type updatePromptRequest struct {
	Name string `json:"name"`
	Body string `json:"body"`
}

func (s *Server) handlePromptPut(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")

	var req updatePromptRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid request body"})
		return
	}
	if req.Name == "" || req.Body == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "name and body are required"})
		return
	}

	if err := db.UpdatePrompt(s.db, id, req.Name, req.Body); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}

	updated, _ := db.GetPrompt(s.db, id)
	writeJSON(w, http.StatusOK, updated)
}

func (s *Server) handlePromptDelete(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")

	prompt, err := db.GetPrompt(s.db, id)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	if prompt == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "prompt not found"})
		return
	}

	// System and imported prompts are soft-deleted (hidden), user prompts are hard-deleted
	if prompt.Source == "system" || prompt.Source == "imported" {
		if err := db.HidePrompt(s.db, id); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"status": "hidden"})
		return
	}

	if err := db.DeletePrompt(s.db, id); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
}

func (s *Server) handleBindingCreate(w http.ResponseWriter, r *http.Request) {
	var b domain.PromptBinding
	if err := json.NewDecoder(r.Body).Decode(&b); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid request body"})
		return
	}
	if b.PromptID == "" || b.EventType == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "prompt_id and event_type required"})
		return
	}
	if err := db.CreateBinding(s.db, b); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusCreated, b)
}

func (s *Server) handleBindingDelete(w http.ResponseWriter, r *http.Request) {
	promptID := r.URL.Query().Get("prompt_id")
	eventType := r.URL.Query().Get("event_type")
	if promptID == "" || eventType == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "prompt_id and event_type query params required"})
		return
	}
	if err := db.DeleteBinding(s.db, promptID, eventType); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
}

func (s *Server) handleBindingSetDefault(w http.ResponseWriter, r *http.Request) {
	var b domain.PromptBinding
	if err := json.NewDecoder(r.Body).Decode(&b); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid request body"})
		return
	}
	if err := db.SetBindingDefault(s.db, b.PromptID, b.EventType, b.IsDefault); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, b)
}

func (s *Server) handlePromptStats(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	stats, err := db.GetPromptStats(s.db, id)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, stats)
}

func (s *Server) handlePromptBindingsGet(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	bindings, err := db.GetBindingsForPrompt(s.db, id)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	if bindings == nil {
		bindings = []domain.PromptBinding{}
	}
	writeJSON(w, http.StatusOK, bindings)
}

func (s *Server) handlePromptBindingsSet(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")

	var bindings []domain.PromptBinding
	if err := json.NewDecoder(r.Body).Decode(&bindings); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid request body"})
		return
	}

	for i := range bindings {
		bindings[i].PromptID = id
	}

	if err := db.SetBindingsForPrompt(s.db, id, bindings); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}

	writeJSON(w, http.StatusOK, bindings)
}
