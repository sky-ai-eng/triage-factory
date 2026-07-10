package server

import (
	"database/sql"
	"net/http"
	"strings"

	"github.com/google/uuid"
	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
	"github.com/sky-ai-eng/triage-factory/internal/server/authz"
	"github.com/sky-ai-eng/triage-factory/internal/server/teamscope"
	"github.com/sky-ai-eng/triage-factory/internal/skills"
)

// skillsHandler serves the skill-import endpoints: the local-mode
// filesystem scan (db + prompts, raw handles the legacy importer still
// takes) and the mode-agnostic paste/upload flow (tx + az, the same
// claims-bound path handlePromptCreate uses).
type skillsHandler struct {
	db      *sql.DB
	prompts db.PromptStore
	tx      db.TxRunner
	az      *authz.Checker
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

// skillUploadRequest is the paste/upload wire shape: the raw SKILL.md
// markdown (frontmatter + body), an optional display-name override, and
// the multi-team caller's acting-team pick.
type skillUploadRequest struct {
	Content string `json:"content"`
	Name    string `json:"name,omitempty"`
	TeamID  string `json:"team_id,omitempty"`
}

// maxSkillUploadBytes bounds a pasted SKILL.md. Real skills are a few
// KB; 256KiB is generous without letting a paste balloon a prompt row.
const maxSkillUploadBytes = 256 << 10

// handleSkillUpload imports one skill from pasted/uploaded SKILL.md
// content — the multi-mode counterpart of the filesystem scan (the
// design of record: skills are not a separate concept in multi mode;
// they're imported as prompts). Mode-agnostic on purpose: in multi
// there is no per-tenant filesystem to scan, and in local a paste is
// just another way to add a prompt. The row is created through the
// exact WithTx + Prompts.Create path handlePromptCreate uses, scoped
// to the requesting org and acting team, with source='imported' so the
// library renders it alongside scan-imported skills.
func (sk *skillsHandler) handleSkillUpload(w http.ResponseWriter, r *http.Request) {
	orgID, ok := requireOrg(w, r)
	if !ok {
		return
	}
	// Bound the body BEFORE decoding — the content cap below is checked
	// post-decode, so without this an oversized upload would still be
	// allocated in full by the JSON decoder before rejection. 4x the
	// content cap leaves room for JSON string escaping (worst-case
	// \uXXXX inflation) plus the envelope fields.
	r.Body = http.MaxBytesReader(w, r.Body, int64(maxSkillUploadBytes)*4)
	var req skillUploadRequest
	if !decodeJSON(w, r, &req, "") {
		return
	}
	content := strings.TrimSpace(req.Content)
	if content == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "content is required (the SKILL.md markdown)"})
		return
	}
	if len(content) > maxSkillUploadBytes {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "skill content exceeds the 256KiB limit"})
		return
	}

	// ParseSkillMeta's path argument only seeds the default name (the
	// skill's directory on disk); a paste has no directory, so the
	// fallback chain is: explicit request name → frontmatter name: →
	// 400.
	meta := skills.ParseSkillMeta(content, "")
	name := strings.TrimSpace(req.Name)
	if name == "" && meta.Name != "" && meta.Name != "." {
		name = meta.Name
	}
	if name == "" || name == "." {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "skill needs a name — set frontmatter `name:` or pass name explicitly"})
		return
	}

	userID := ClaimsFrom(r.Context()).Subject

	// Same viewer gate as handlePromptCreate: resolve read-only first
	// so a viewer gets a clean 403 instead of an RLS 500.
	var actingTeam string
	if err := sk.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		var e error
		actingTeam, e = teamscope.ResolveActingNoStamp(r.Context(), tx.Teams, tx.Users, orgID, userID, req.TeamID)
		return e
	}); err != nil {
		if teamscope.WriteIfSelectionError(w, err) {
			return
		}
		internalError(w, "skills", err)
		return
	}
	if !sk.az.RequireTeamWrite(w, r, orgID, userID, actingTeam) {
		return
	}

	id := uuid.New().String()
	prompt := domain.Prompt{
		ID:           id,
		Name:         name,
		Body:         meta.Body,
		Source:       "imported",
		AllowedTools: meta.AllowedTools,
	}

	var created *domain.Prompt
	if err := sk.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		teamID, e := teamscope.ResolveActing(r.Context(), tx.Teams, tx.Users, orgID, userID, req.TeamID)
		if e != nil {
			return e
		}
		if e := tx.Prompts.Create(r.Context(), orgID, teamID, prompt); e != nil {
			return e
		}
		var ge error
		created, ge = tx.Prompts.Get(r.Context(), orgID, id)
		return ge
	}); err != nil {
		if teamscope.WriteIfSelectionError(w, err) {
			return
		}
		internalError(w, "skills", err)
		return
	}
	writeJSON(w, http.StatusCreated, created)
}
