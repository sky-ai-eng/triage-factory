package server

import (
	"database/sql"
	"net/http"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
	"github.com/sky-ai-eng/triage-factory/internal/skills"
)

// skillsHandler serves the skill-import endpoint, holding the DB handle and
// prompt store the importer writes through.
type skillsHandler struct {
	db      *sql.DB
	prompts db.PromptStore
}

func (sk *skillsHandler) handleSkillsImport(w http.ResponseWriter, r *http.Request) {
	// The filesystem scan reads the TF process's own ~/.claude/skills —
	// meaningful only in local mode where the process runs on the single
	// trusted user's machine. In multi mode there is no per-tenant
	// filesystem to scan (and the importer's raw SQLite SQL + sentinel
	// org would fail against Postgres anyway — previously as a 200 with
	// a swallowed errors array). Multi-mode skill import is the
	// paste/upload flow, which writes a prompts row scoped to the
	// requesting org/team.
	if runmode.Current() != runmode.ModeLocal {
		writeJSON(w, http.StatusNotImplemented, map[string]string{"error": "filesystem skill import is local-mode only; paste or upload skill markdown instead"})
		return
	}
	result := skills.ImportAll(r.Context(), sk.db, sk.prompts)
	writeJSON(w, http.StatusOK, result)
}
