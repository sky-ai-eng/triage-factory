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
	"github.com/sky-ai-eng/triage-factory/internal/server/httpx"
	"github.com/sky-ai-eng/triage-factory/internal/server/teamscope"
	"github.com/sky-ai-eng/triage-factory/internal/skills"
)

// promptImportHandler serves the two prompt-import routes: the local-mode
// filesystem scan of the process's ~/.claude/skills (db + prompts, the raw
// handles the importer still takes) and the mode-agnostic paste/upload flow
// (tx + az, the same claims-bound path the prompt create route uses).
//
// They live under /api/prompts because that is what they write. A skill is a
// SKILL.md file — a format an import reads — not a second kind of row: both
// routes land a prompts row with source='imported'.
type promptImportHandler struct {
	db      *sql.DB
	prompts db.PromptStore
	tx      db.TxRunner
	az      *authz.Checker
}

var opPromptImportFromDisk = httpx.NewJSONOp[httpx.NoBody, skills.ImportResult](httpx.OpSpec{
	Pattern:     "POST /api/prompts/from-disk",
	ID:          "create_prompts_from_disk",
	Summary:     "Import prompts from the server's skill files",
	Description: "Imports each SKILL.md under ~/.claude/skills and .claude/skills on the server's own disk as a prompt, updates a prompt imported from a file that has changed since, and returns how many files it scanned, imported, skipped, and could not import. A scan that fails still answers 200, with the failures in failed and errors. Works only in local mode, and answers 404 in multi mode.",
	Hints:       httpx.Hints{ReadOnly: httpx.HintNo, Destructive: httpx.HintYes, Idempotent: httpx.HintYes, OpenWorld: httpx.HintNo},
})

// handlePromptImportFromDisk scans the TF process's own ~/.claude/skills and
// imports each SKILL.md it finds as a prompt. Local-mode only, and the path
// says why: what it reads is a directory on this machine's disk, which only
// exists as a per-tenant thing when the tenant IS the machine.
//
// POST /api/prompts/from-disk
func (sk *promptImportHandler) handlePromptImportFromDisk(w http.ResponseWriter, r *http.Request) {
	// The filesystem scan reads the TF process's own ~/.claude/skills —
	// meaningful only in local mode where the process runs on the single
	// trusted user's machine. In multi mode there is no per-tenant
	// filesystem to scan (and the importer's raw SQLite SQL + sentinel
	// org would fail against Postgres anyway — previously as a 200 with
	// a swallowed errors array). Multi-mode skill import is the
	// paste/upload flow (POST /api/prompts/upload), which writes a prompts
	// row scoped to the requesting org/team.
	if runmode.Current() != runmode.ModeLocal {
		// A route that doesn't exist in this deployment mode is a 404, not a
		// 501 — the marketplace's mode gate already answered that way, and one
		// condition must not have two statuses.
		notFound(w, "route")
		return
	}
	result := skills.ImportAll(r.Context(), sk.db, sk.prompts)
	opPromptImportFromDisk.Write(w, result)
}

// promptUploadRequest is the paste/upload wire shape: the raw SKILL.md
// markdown (frontmatter + body), an optional display-name override, and
// the multi-team caller's acting-team pick.
type promptUploadRequest struct {
	Content string `json:"content" doc:"The SKILL.md file's text, frontmatter included, at most 256 KiB once trimmed; required."`
	Name    string `json:"name,omitempty" doc:"The prompt's name; absent or empty is the frontmatter's name, and one of the two is required."`
	TeamID  string `json:"team_id,omitempty" doc:"The id of the team to create the prompt under, required when the caller writes to more than one team; absent is the caller's one team."`
}

// maxPromptUploadBytes bounds a pasted SKILL.md. Real ones are a few KB;
// 256KiB is generous without letting a paste balloon a prompt row.
const maxPromptUploadBytes = 256 << 10

var opPromptUpload = httpx.NewJSONOp[promptUploadRequest, domain.Prompt](httpx.OpSpec{
	Pattern:     "POST /api/prompts/upload",
	ID:          "upload_prompt",
	Summary:     "Import a prompt from a SKILL.md file",
	Description: "Creates a prompt on the caller's team from a SKILL.md file's text, taking its body and allowed tools from the file, and returns the prompt. The prompt's source is imported.",
	Status:      http.StatusCreated,
	// The content cap is checked after the decode, so the body cap leaves
	// room for JSON string escaping and the other fields.
	BodyLimit: int64(maxPromptUploadBytes) * 4,
	Hints:     httpx.Hints{ReadOnly: httpx.HintNo, Destructive: httpx.HintNo, Idempotent: httpx.HintNo, OpenWorld: httpx.HintNo},
})

// handlePromptUpload imports one prompt from pasted/uploaded SKILL.md
// content — the counterpart of the filesystem scan. Mode-agnostic on
// purpose: in multi there is no per-tenant filesystem to scan, and in local
// a paste is just another way to add a prompt. The row is created through
// the exact WithTx + Prompts.Create path the prompt create route uses, scoped
// to the requesting org and acting team, with source='imported' so the
// library renders it alongside scan-imported rows.
//
// POST /api/prompts/upload
func (sk *promptImportHandler) handlePromptUpload(w http.ResponseWriter, r *http.Request) {
	orgID, ok := requireOrg(w, r)
	if !ok {
		return
	}
	// Bound the body BEFORE decoding — the content cap below is checked
	// post-decode, so without this an oversized upload would still be
	// allocated in full by the JSON decoder before rejection. The op's
	// BodyLimit is 4x the content cap, which leaves room for JSON string
	// escaping (worst-case \uXXXX inflation) plus the envelope fields.
	req, ok := opPromptUpload.Decode(w, r)
	if !ok {
		return
	}
	var v httpx.Validation
	content := strings.TrimSpace(req.Content)
	switch {
	case content == "":
		v.Add(httpx.ErrorItem{
			Reason: httpx.ReasonMissingField, Message: "content is required (the SKILL.md markdown)", Field: "content",
		})
	case len(content) > maxPromptUploadBytes:
		v.OutOfRange("content", "skill content exceeds the 256KiB limit")
	}

	// ParseSkillMeta's path argument only seeds the default name (the
	// skill's directory on disk); a paste has no directory, so the
	// fallback chain is: explicit request name → frontmatter name: →
	// 400. Empty content parses to no name, so a body with neither is
	// refused on both fields.
	meta := skills.ParseSkillMeta(content, "")
	name := strings.TrimSpace(req.Name)
	if name == "" && meta.Name != "" && meta.Name != "." {
		name = meta.Name
	}
	if name == "" || name == "." {
		v.Add(httpx.ErrorItem{
			Reason:  httpx.ReasonMissingField,
			Message: "skill needs a name — set frontmatter `name:` or pass name explicitly",
			Field:   "name",
		})
	}
	if v.Flush(w, http.StatusBadRequest) {
		return
	}

	userID := ClaimsFrom(r.Context()).Subject

	// Same viewer gate as the prompt create route: resolve read-only first
	// so a viewer gets a clean 403 instead of an RLS 500.
	var actingTeam string
	if err := sk.tx.WithReadTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		var e error
		actingTeam, e = teamscope.ResolveActingNoStamp(r.Context(), tx.Teams, tx.Users, orgID, userID, req.TeamID)
		return e
	}); err != nil {
		if teamscope.WriteIfSelectionError(w, err) {
			return
		}
		internalError(w, "prompts/import", err)
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

	var created domain.Prompt
	if err := sk.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		teamID, e := teamscope.ResolveActing(r.Context(), tx.Teams, tx.Users, orgID, userID, req.TeamID)
		if e != nil {
			return e
		}
		created, e = tx.Prompts.Create(r.Context(), orgID, teamID, prompt)
		return e
	}); err != nil {
		if teamscope.WriteIfSelectionError(w, err) {
			return
		}
		internalError(w, "prompts/import", err)
		return
	}
	opPromptUpload.Write(w, created)
}

func (s *Server) registerPromptImportRoutes() {
	// Prompt imports. Two routes rather than one with a source field: they
	// take different bodies (a paste vs. nothing) and only one of them can
	// exist in multi mode, where the process has no per-tenant disk to scan.
	pi := &promptImportHandler{db: s.db, prompts: s.prompts, tx: s.tx, az: s.az}
	s.op(opPromptImportFromDisk, pi.handlePromptImportFromDisk)
	s.op(opPromptUpload, pi.handlePromptUpload)
}
