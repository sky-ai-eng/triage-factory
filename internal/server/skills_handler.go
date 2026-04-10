package server

import (
	"net/http"

	"github.com/sky-ai-eng/todo-triage/internal/skills"
)

func (s *Server) handleSkillsImport(w http.ResponseWriter, r *http.Request) {
	result := skills.ImportAll(s.db)
	writeJSON(w, http.StatusOK, result)
}
