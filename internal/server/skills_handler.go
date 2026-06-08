package server

import (
	"database/sql"
	"net/http"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/skills"
)

// skillsHandler serves the skill-import endpoint, holding the DB handle and
// prompt store the importer writes through.
type skillsHandler struct {
	db      *sql.DB
	prompts db.PromptStore
}

func (sk *skillsHandler) handleSkillsImport(w http.ResponseWriter, r *http.Request) {
	result := skills.ImportAll(r.Context(), sk.db, sk.prompts)
	writeJSON(w, http.StatusOK, result)
}
