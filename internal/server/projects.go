package server

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/sky-ai-eng/triage-factory/internal/curator"
	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	ghclient "github.com/sky-ai-eng/triage-factory/internal/github"
	"github.com/sky-ai-eng/triage-factory/internal/projectbundle"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
	"github.com/sky-ai-eng/triage-factory/internal/server/teamscope"
	"github.com/sky-ai-eng/triage-factory/internal/worktree"
)

// Projects are the data layer underneath the Curator stack —
// the long-lived per-project context that the rest of the
// family hangs work onto. This file is pure CRUD + on-disk knowledge
// dir cleanup; the Curator runtime, classifier, and UI all land in
// later tickets and can hit the same handlers without changes here.

// createProjectRequest is the POST body shape. Most fields are
// optional — a project starts as an empty shell named by the user
// and gets filled in over time (description, pinned repos, summary).
type createProjectRequest struct {
	Name             string   `json:"name"`
	Description      string   `json:"description"`
	PinnedRepos      []string `json:"pinned_repos"`
	JiraProjectKey   string   `json:"jira_project_key"`
	LinearProjectKey string   `json:"linear_project_key"`
	CuratorSessionID string   `json:"curator_session_id"` // optional; usually set by the runtime, not the user
	// TeamID is the acting team the write picker supplied — the team the
	// new project (and its pinned-repo / tracker-key validation) is
	// scoped to. Required in the UI when the caller belongs to ≥2 teams;
	// empty (sole-team fallback) otherwise. Only consulted when
	// Visibility is "team" — private/org projects have no team in v1.
	TeamID string `json:"team_id"`
	// Visibility is one of domain's "private" / "team" / "org". Empty
	// defaults to "team", preserving pre-TFAC-562 behavior. Forced to
	// "team" in local mode regardless of what's sent — private/org is a
	// multi-mode sharing distinction that N=1 has no use for.
	Visibility string `json:"visibility"`
}

// patchProjectRequest is the PATCH body shape. Pointers distinguish
// "absent → leave unchanged" from "explicit → overwrite". PinnedRepos
// uses *[]string so a client can clear it with [] without colliding
// with the absent case.
type patchProjectRequest struct {
	Name                      *string   `json:"name"`
	Description               *string   `json:"description"`
	PinnedRepos               *[]string `json:"pinned_repos"`
	JiraProjectKey            *string   `json:"jira_project_key"`
	LinearProjectKey          *string   `json:"linear_project_key"`
	CuratorSessionID          *string   `json:"curator_session_id"`
	SpecAuthorshipBlueprintID *string   `json:"spec_authorship_blueprint_id"`
	// Visibility changes an existing project's private/team/org gate.
	// There's no TeamID field alongside it — v1 doesn't support
	// attaching a team to a project that doesn't already have one, so a
	// private/org project created teamless can move between private ⇄
	// org freely but can't upgrade to "team" (existing.TeamID stays
	// preserved across every transition; the handler 400s if a "team"
	// target has no team to land on). A project already on a team can
	// move freely across all three — RLS is the permission authority
	// (see db.ErrVisibilityForbidden): private requires being the
	// project's own creator, team requires write-membership on the
	// team, org requires org-admin — symmetric for upgrades and
	// downgrades alike.
	Visibility *string `json:"visibility"`
}

func (s *Server) handleProjectCreate(w http.ResponseWriter, r *http.Request) {
	orgID, ok := s.requireOrg(w, r)
	if !ok {
		return
	}
	userID := ClaimsFrom(r.Context()).Subject
	var req createProjectRequest
	if !decodeJSON(w, r, &req, "") {
		return
	}
	name := strings.TrimSpace(req.Name)
	if name == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "name is required"})
		return
	}
	jiraKey := strings.TrimSpace(req.JiraProjectKey)
	linearKey := strings.TrimSpace(req.LinearProjectKey)

	visibility := strings.TrimSpace(req.Visibility)
	if visibility == "" {
		visibility = domain.ProjectVisibilityTeam
	}
	if !domain.ValidProjectVisibility(visibility) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "visibility must be one of private, team, org"})
		return
	}
	// Multi-mode-only feature (TFAC-562): local's N=1 tenancy has no
	// team/org distinction worth making, so force team-scoped
	// regardless of what a stale or hand-crafted client sends.
	if runmode.Current() == runmode.ModeLocal {
		visibility = domain.ProjectVisibilityTeam
	}

	// Resolve the acting team, validate pinned repos + tracker keys
	// against that team, then create — all in ONE WithTx so the team the
	// row is written under is the same team validation ran against (no
	// resolve-then-write window where the team could change underneath
	// us) and so teams_select / team_github_repos_select / jira_rules_select
	// RLS gates all fire under the user's claims. Jira rules are loaded
	// lazily — only when the Jira side needs validation. The three
	// user-facing validation failures (bad visibility/team combo, bad
	// pinned repo, unknown tracker key) short-circuit the closure with a
	// nil error and surface their captured message as a 400 after the tx;
	// nothing is written on those paths. The acting team owns the new
	// project; UPDATE instead validates against the project's own
	// existing.TeamID, so only this create path resolves a team.
	var (
		teamID           string
		pinned           []string
		pinnedErrMsg     string
		trackerErrMsg    string
		visibilityErrMsg string
		teamJiraRules    []domain.JiraProjectStatusRules
		created          *domain.Project
	)
	if err := s.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		var e error
		if visibility == domain.ProjectVisibilityTeam {
			// teamscope.ResolveActing's zero-teams case returns
			// teamscope.ErrNoTeam (not a silent fallback — see its doc
			// comment), which WriteIfSelectionError maps to a clean 400
			// below. That's the exact opaque-500 case TFAC-562 reported;
			// the paired frontend fix grays out "team" visibility for
			// teamless users and defaults them to private instead.
			teamID, e = teamscope.ResolveActing(r.Context(), tx.Teams, tx.Users, orgID, userID, req.TeamID)
			if e != nil {
				return e
			}
		} else if len(req.PinnedRepos) > 0 || jiraKey != "" || linearKey != "" {
			// private/org projects have no team to validate these
			// against in v1 — they start as a bare shell (name +
			// description + knowledge base); pinning resources requires
			// team visibility.
			visibilityErrMsg = "pinned repos and tracker projects require team visibility"
			return nil
		}
		pinned, pinnedErrMsg = validatePinnedRepos(r.Context(), tx.TeamGitHubRepos, teamID, req.PinnedRepos)
		if pinnedErrMsg != "" {
			return nil // surfaced as 400 below; nothing written
		}
		if jiraKey != "" {
			teamJiraRules, e = tx.JiraStatusRules.ListForTeam(r.Context(), teamID)
			if e != nil {
				return fmt.Errorf("list jira rules: %w", e)
			}
		}
		if jiraKey != "" || linearKey != "" {
			jiraKey, linearKey, trackerErrMsg = validateTrackerKeys(teamJiraRules, jiraKey, linearKey)
			if trackerErrMsg != "" {
				return nil // surfaced as 400 below; nothing written
			}
		}

		// Validation passed — default the spec-authorship skill to the team's
		// seeded system blueprint when it exists. Doing this at the API layer
		// (not in db.CreateProject) keeps the DB layer free of any "system
		// blueprint must exist" coupling — tests that don't seed blueprints
		// get NULL on insert and the curator runtime falls back to the same
		// default at dispatch time anyway. Resolve the team's own copy of the
		// shipped spec-authorship blueprint by slug (id is a random UUID per
		// team copy); store the resolved UUID. Skipped entirely for a
		// teamless private/org project — there's no team copy to resolve.
		specBlueprintID := ""
		if teamID != "" {
			def, defErr := tx.Blueprints.GetBySystemSlug(r.Context(), orgID, teamID, domain.SystemTicketSpecPromptID)
			if defErr != nil {
				projectsLog.Warn("create: failed to load default spec-authorship blueprint", "slug", domain.SystemTicketSpecPromptID, "error", defErr)
			} else if def != nil {
				specBlueprintID = def.ID
			}
		}
		id, createErr := tx.Projects.Create(r.Context(), orgID, teamID, domain.Project{
			Name:                      name,
			Description:               req.Description,
			PinnedRepos:               pinned,
			JiraProjectKey:            jiraKey,
			LinearProjectKey:          linearKey,
			CuratorSessionID:          req.CuratorSessionID,
			SpecAuthorshipBlueprintID: specBlueprintID,
			Visibility:                visibility,
		})
		if createErr != nil {
			return createErr
		}
		var getErr error
		created, getErr = tx.Projects.Get(r.Context(), orgID, id)
		return getErr
	}); err != nil {
		// teamscope.ErrNoTeam gets a project-flavored message (mentioning the
		// private/org escape hatch) ahead of WriteIfSelectionError's generic
		// one; ErrRequired/ErrForbidden fall through to that generic 400.
		if errors.Is(err, teamscope.ErrNoTeam) {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "join a team to create a team-visibility project, or choose private/org visibility"})
			return
		}
		if teamscope.WriteIfSelectionError(w, err) {
			return
		}
		if errors.Is(err, db.ErrVisibilityForbidden) {
			writeJSON(w, http.StatusForbidden, map[string]string{"error": "you don't have permission to create a project with that visibility"})
			return
		}
		projectsLog.Error("create failed", "error", err)
		internalError(w, "projects", err)
		return
	}
	if visibilityErrMsg != "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": visibilityErrMsg})
		return
	}
	if pinnedErrMsg != "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": pinnedErrMsg})
		return
	}
	if trackerErrMsg != "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": trackerErrMsg})
		return
	}
	writeJSON(w, http.StatusCreated, created)
}

func (s *Server) handleProjectList(w http.ResponseWriter, r *http.Request) {
	orgID, ok := s.requireOrg(w, r)
	if !ok {
		return
	}
	userID := ClaimsFrom(r.Context()).Subject
	var projects []domain.Project
	if err := s.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		var e error
		projects, e = tx.Projects.List(r.Context(), orgID)
		return e
	}); err != nil {
		internalError(w, "projects", err)
		return
	}
	writeJSON(w, http.StatusOK, projects)
}

func (s *Server) handleProjectGet(w http.ResponseWriter, r *http.Request) {
	orgID, ok := s.requireOrg(w, r)
	if !ok {
		return
	}
	userID := ClaimsFrom(r.Context()).Subject
	id := r.PathValue("id")
	var project *domain.Project
	if err := s.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		var e error
		project, e = tx.Projects.Get(r.Context(), orgID, id)
		return e
	}); err != nil {
		internalError(w, "projects", err)
		return
	}
	if project == nil {
		notFound(w, "project")
		return
	}
	writeJSON(w, http.StatusOK, project)
}

type projectBundleGitHubProbe struct {
	resolver ghclient.Resolver
	orgID    string
}

func (p projectBundleGitHubProbe) CloneURLForRepo(ctx context.Context, owner, repo string) (string, error) {
	if p.resolver == nil {
		return "", errors.New("GitHub is not configured")
	}
	// Resolve per repo (org App installation token → PAT) so App-only orgs
	// (no PAT) probe the clone URL through the installation client. A
	// no-credentials resolve surfaces through preflightPinnedRepos as the
	// existing MissingReposError import-failure shape.
	client, err := p.resolver.ClientForRepo(ctx, p.orgID, owner, repo)
	if err != nil {
		return "", err
	}
	meta, err := client.GetRepoMeta(ctx, owner, repo)
	if err != nil {
		return "", err
	}
	if meta == nil {
		return "", errors.New("repo metadata is missing")
	}
	return meta.CloneURL, nil
}

const (
	projectBundleMaxUploadBytes = int64(1024 * 1024 * 1024) // 1GiB
)

func (s *Server) handleProjectExportPreview(w http.ResponseWriter, r *http.Request) {
	orgID, ok := s.requireOrg(w, r)
	if !ok {
		return
	}
	id := r.PathValue("id")
	preview, err := projectbundle.Preview(r.Context(), s.db, s.projects, s.curatorStore, orgID, id)
	if err != nil {
		if errors.Is(err, projectbundle.ErrProjectNotFound) {
			notFound(w, "project")
			return
		}
		internalError(w, "projects", err)
		return
	}
	writeJSON(w, http.StatusOK, preview)
}

func (s *Server) handleProjectExport(w http.ResponseWriter, r *http.Request) {
	orgID, ok := s.requireOrg(w, r)
	if !ok {
		return
	}
	userID := ClaimsFrom(r.Context()).Subject
	id := r.PathValue("id")
	var project *domain.Project
	if err := s.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		var e error
		project, e = tx.Projects.Get(r.Context(), orgID, id)
		return e
	}); err != nil {
		internalError(w, "projects", err)
		return
	}
	if project == nil {
		notFound(w, "project")
		return
	}
	stream, err := projectbundle.Export(r.Context(), s.db, s.projects, s.curatorStore, orgID, id)
	if err != nil {
		if errors.Is(err, projectbundle.ErrProjectNotFound) {
			notFound(w, "project")
			return
		}
		internalError(w, "projects", err)
		return
	}
	defer stream.Close()

	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", projectBundleFilename(project.Name)))
	if _, err := io.Copy(w, stream); err != nil {
		projectsLog.Error("export stream failed", "project", id, "error", err)
	}
}

func (s *Server) handleProjectImport(w http.ResponseWriter, r *http.Request) {
	orgID, ok := s.requireOrg(w, r)
	if !ok {
		return
	}
	// projectbundle.Import is still SQLite-only — it opens its own
	// *sql.Tx and writes via raw `?`-placeholder INSERTs that have no
	// Postgres counterpart. Gate the endpoint until the bundle
	// import/export path is ported to per-dialect store methods so a
	// multi-mode tenant doesn't get a 500 the first time they try to
	// import. Local-mode behavior is unchanged.
	if runmode.Current() != runmode.ModeLocal {
		writeJSON(w, http.StatusNotImplemented, map[string]string{"error": "project import is not yet supported in multi-mode deployments"})
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, projectBundleMaxUploadBytes)
	if err := r.ParseMultipartForm(64 << 20); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "multipart parse: " + err.Error()})
		return
	}
	defer func() {
		if r.MultipartForm != nil {
			_ = r.MultipartForm.RemoveAll()
		}
	}()

	file, _, err := r.FormFile("bundle")
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bundle file is required (form field 'bundle')"})
		return
	}
	defer file.Close()

	size, err := file.Seek(0, io.SeekEnd)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "failed to determine bundle size"})
		return
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "failed to rewind bundle"})
		return
	}

	userID := ClaimsFrom(r.Context()).Subject
	// Resolve the acting team inside WithTx so teams_select RLS gates the
	// lookup under the user's claims. The downstream projectbundle.Import
	// does its own DB work; we don't include it in this tx to keep import
	// latency from holding a claims-bound connection. (Import is local-
	// mode-only — gated above — so the acting team is always the sole
	// team; resolveActingTeam keeps the choice centralized regardless.)
	var teamID string
	if err := s.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		var e error
		// Import is local-mode-only (gated above), so there's no picker
		// and the sole-team fallback always applies — pass an empty pick.
		teamID, e = teamscope.ResolveActing(r.Context(), tx.Teams, tx.Users, orgID, userID, "")
		return e
	}); err != nil {
		if teamscope.WriteIfSelectionError(w, err) {
			return
		}
		internalError(w, "projects", err)
		return
	}

	project, warnings, err := projectbundle.Import(
		r.Context(),
		s.db,
		s.projects,
		orgID,
		teamID,
		userID,
		file,
		size,
		projectBundleGitHubProbe{resolver: s.ghResolver, orgID: orgID},
	)
	if err != nil {
		var dupNameErr *projectbundle.DuplicateNameError
		if errors.As(err, &dupNameErr) {
			writeJSON(w, http.StatusConflict, map[string]any{
				"error":   "duplicate_name",
				"message": "rename or delete the existing project first",
			})
			return
		}
		var missingReposErr *projectbundle.MissingReposError
		if errors.As(err, &missingReposErr) {
			writeJSON(w, http.StatusConflict, map[string]any{
				"error":         "missing_repos",
				"missing_repos": missingReposErr.Missing,
			})
			return
		}
		var unsupported *projectbundle.UnsupportedFormatError
		if errors.As(err, &unsupported) || errors.Is(err, projectbundle.ErrManifestMissing) {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		internalError(w, "projects", err)
		return
	}

	writeJSON(w, http.StatusCreated, map[string]any{
		"project":  project,
		"warnings": warnings,
	})
}

func projectBundleFilename(name string) string {
	name = strings.TrimSpace(name)
	if name == "" {
		name = "project"
	}
	var b strings.Builder
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z':
			b.WriteRune(r)
		case r >= 'A' && r <= 'Z':
			b.WriteRune(r)
		case r >= '0' && r <= '9':
			b.WriteRune(r)
		case r == '-' || r == '_' || r == ' ':
			b.WriteRune(r)
		default:
			b.WriteRune('-')
		}
	}
	safe := strings.TrimSpace(b.String())
	if safe == "" {
		safe = "project"
	}
	return safe + ".tfproject"
}

func (s *Server) handleProjectUpdate(w http.ResponseWriter, r *http.Request) {
	orgID, ok := s.requireOrg(w, r)
	if !ok {
		return
	}
	userID := ClaimsFrom(r.Context()).Subject
	id := r.PathValue("id")
	var req patchProjectRequest
	if !decodeJSON(w, r, &req, "") {
		return
	}

	// Per-project lock around the read-merge-write window so two
	// concurrent autosaves don't lost-update each other. Two quick
	// edits from different widgets (pinned-repos chip + tracker
	// picker) would otherwise both read pre-edit state, merge their
	// own field, and serially overwrite the row — leaving whichever
	// landed second as the only contribution. Holding the lock
	// across read+merge+write makes the whole sequence atomic from
	// the perspective of other PATCHes for the same project — across
	// every control pod in multi mode, not just this process (TFAC-579).
	release, err := s.acquireKeyedLock(r.Context(), &s.projectMutexes, projectRMWLockSalt, id)
	if err != nil {
		internalError(w, "projects", err)
		return
	}
	defer release()

	var existing *domain.Project
	if err := s.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		var e error
		existing, e = tx.Projects.Get(r.Context(), orgID, id)
		return e
	}); err != nil {
		internalError(w, "projects", err)
		return
	}
	if existing == nil {
		notFound(w, "project")
		return
	}

	updated := *existing
	if req.Name != nil {
		trimmed := strings.TrimSpace(*req.Name)
		if trimmed == "" {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "name cannot be empty"})
			return
		}
		updated.Name = trimmed
	}
	if req.Description != nil {
		updated.Description = *req.Description
	}
	if req.PinnedRepos != nil {
		// Validate against the project's OWN team, not the org default — a
		// non-default-team project's pins must be tracked by that team.
		// existing.TeamID is populated by Projects.Get; the store preserves
		// team_id on update. Empty is a legitimate state since TFAC-562 (a
		// private/org-visibility project has no team), not a malformed row —
		// pinned repos just aren't available for one in v1.
		if existing.TeamID == "" {
			writeJSON(w, http.StatusBadRequest, map[string]string{
				"error": "this project has no team — pinned repos aren't available for a private/org visibility project",
			})
			return
		}
		var pinned []string
		var errMsg string
		if err := s.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
			pinned, errMsg = validatePinnedRepos(r.Context(), tx.TeamGitHubRepos, existing.TeamID, *req.PinnedRepos)
			return nil
		}); err != nil {
			internalError(w, "projects", err)
			return
		}
		if errMsg != "" {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": errMsg})
			return
		}
		updated.PinnedRepos = pinned
	}
	// Validate only the fields the client sent. Re-validating the
	// untouched side against the current rules would surface a
	// confusing error if the team's Jira config drifted (e.g. a
	// project got renamed in Settings) on an unrelated PATCH that's
	// only touching, say, the Linear key. The handler's contract is
	// "validate what the client asked to change," not "re-validate
	// the whole row on every PATCH."
	//
	// Rules are loaded lazily — and only when the Jira side is being
	// set to a non-empty value. Clearing either tracker, or setting
	// Linear at all, never reads them: validateTrackerKeys with an
	// empty Jira input doesn't consult rules, and Linear validation
	// rejects non-empty regardless of rules. This keeps a transient
	// DB hiccup from 500-ing requests like `{"linear_project_key":""}`
	// that have nothing to do with Jira.
	if req.JiraProjectKey != nil {
		jiraInput := strings.TrimSpace(*req.JiraProjectKey)
		if jiraInput == "" {
			updated.JiraProjectKey = ""
		} else {
			// Rule lookup against the project's OWN team (not the org
			// default), inside WithTx so the read goes through
			// tx.JiraStatusRules.ListForTeam (app pool, jira_rules_select
			// RLS gates by team membership). Empty is a legitimate state
			// since TFAC-562 (a private/org-visibility project has no
			// team) — a tracker key just isn't available for one in v1.
			if existing.TeamID == "" {
				writeJSON(w, http.StatusBadRequest, map[string]string{
					"error": "this project has no team — a tracker project key isn't available for a private/org visibility project",
				})
				return
			}
			var teamRules []domain.JiraProjectStatusRules
			if err := s.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
				var rerr error
				teamRules, rerr = tx.JiraStatusRules.ListForTeam(r.Context(), existing.TeamID)
				return rerr
			}); err != nil {
				projectsLog.Error("patch: jira rules load failed", "error", err)
				writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to load Jira rules"})
				return
			}
			jiraKey, _, errMsg := validateTrackerKeys(teamRules, *req.JiraProjectKey, "")
			if errMsg != "" {
				writeJSON(w, http.StatusBadRequest, map[string]string{"error": errMsg})
				return
			}
			updated.JiraProjectKey = jiraKey
		}
	}
	if req.LinearProjectKey != nil {
		// Linear is rejected if non-empty regardless of rules; passing
		// nil for the team rules makes the no-rules-needed contract
		// explicit. When the Linear integration ships, this branch will
		// need its own rule lookup — but only when the input is
		// non-empty, mirroring the Jira pattern above.
		_, linearKey, errMsg := validateTrackerKeys(nil, "", *req.LinearProjectKey)
		if errMsg != "" {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": errMsg})
			return
		}
		updated.LinearProjectKey = linearKey
	}
	if req.CuratorSessionID != nil {
		updated.CuratorSessionID = *req.CuratorSessionID
	}
	if req.SpecAuthorshipBlueprintID != nil {
		// Empty string clears the override (project falls back to the seeded
		// default on next dispatch). Non-empty values must reference an
		// existing blueprint owned by the project's team — without this guard
		// a typo would silently break ticket authorship.
		trimmed := strings.TrimSpace(*req.SpecAuthorshipBlueprintID)
		if trimmed != "" {
			// Validate against the project's OWN team, not just the org: the
			// curator runs this blueprint under existing.TeamID, so a
			// cross-team blueprint would let a team-A project run a team-B
			// blueprint. List the team's blueprints and require membership.
			// SQLite ignores teamID (local is single-team). Empty is a
			// legitimate state since TFAC-562 (a private/org-visibility
			// project has no team) — a blueprint override just isn't
			// available for one in v1.
			if existing.TeamID == "" {
				writeJSON(w, http.StatusBadRequest, map[string]string{
					"error": "this project has no team — a spec-authorship blueprint override isn't available for a private/org visibility project",
				})
				return
			}
			var visible []domain.Blueprint
			if err := s.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
				var e error
				visible, e = tx.Blueprints.List(r.Context(), orgID, existing.TeamID)
				return e
			}); err != nil {
				projectsLog.Error("patch: blueprint list failed", "project", existing.ID, "error", err)
				writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "load blueprint" + localDetail(err)})
				return
			}
			found := false
			for i := range visible {
				if visible[i].ID == trimmed {
					found = true
					break
				}
			}
			if !found {
				// Same 400 for "unknown" and "other team's" — don't leak the
				// existence of another team's blueprint by id.
				writeJSON(w, http.StatusBadRequest, map[string]string{"error": "spec_authorship_blueprint_id references unknown blueprint"})
				return
			}
		}
		updated.SpecAuthorshipBlueprintID = trimmed
	}
	if req.Visibility != nil {
		v := strings.TrimSpace(*req.Visibility)
		if !domain.ValidProjectVisibility(v) {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "visibility must be one of private, team, org"})
			return
		}
		// Multi-mode-only (TFAC-562) — mirrors the create-path guard.
		if runmode.Current() == runmode.ModeLocal {
			v = domain.ProjectVisibilityTeam
		}
		updated.Visibility = v
	}
	if updated.Visibility == domain.ProjectVisibilityTeam && updated.TeamID == "" {
		// v1 doesn't support attaching a team via PATCH — a project
		// created private/org with no team can't become team-visibility
		// without recreating it. existing.TeamID never changes here (no
		// PATCH field sets it), so this only fires for that teamless case.
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": "this project has no team — team visibility isn't available for it",
		})
		return
	}

	if err := s.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		return tx.Projects.Update(r.Context(), orgID, updated)
	}); err != nil {
		if errors.Is(err, db.ErrVisibilityForbidden) {
			writeJSON(w, http.StatusForbidden, map[string]string{"error": "you don't have permission to change this project to that visibility"})
			return
		}
		internalError(w, "projects", err)
		return
	}

	// Queue context-change deltas for the Curator session, if any. This
	// only fires when the project already has an active session
	// (CuratorSessionID populated) — there is no point queueing a delta
	// for a project the user has never chatted with: the next session's
	// static envelope will render fresh values directly. Failures here
	// are logged but do not abort the PATCH; the user's settings change
	// has already been persisted, and the worst case is the agent
	// missing one delta on its next turn (which a follow-up PATCH or
	// the user's next message itself can correct).
	//
	// Coalescing on (project, session, change_type) is enforced by the
	// partial unique index on curator_pending_context — the
	// InsertPendingContext helper uses ON CONFLICT DO NOTHING so a
	// second PATCH between user messages preserves the *earliest*
	// baseline_value, which is the correct anchor for diffing at
	// consume time.
	if existing.CuratorSessionID != "" {
		// Wrap under the request user's claims so the Postgres impl's
		// InsertPendingContext sees tf.current_user_id() for the NOT
		// NULL creator_user_id bind and for the curator_pending_context_modify
		// RLS WITH CHECK predicate. SQLite ignores the claims.
		if err := s.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
			queuePendingContextChanges(r.Context(), tx.Curator, orgID, *existing, updated)
			return nil
		}); err != nil {
			projectsLog.Warn("queue pending context failed", "project", id, "error", err)
		}
	}
	var fresh *domain.Project
	if err := s.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		var e error
		fresh, e = tx.Projects.Get(r.Context(), orgID, id)
		return e
	}); err != nil {
		projectsLog.Error("patch: read-back after update failed", "project", id, "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "updated but read-back failed" + localDetail(err)})
		return
	}
	writeJSON(w, http.StatusOK, fresh)
}

func (s *Server) handleProjectDelete(w http.ResponseWriter, r *http.Request) {
	orgID, ok := s.requireOrg(w, r)
	if !ok {
		return
	}
	userID := ClaimsFrom(r.Context()).Subject
	id := r.PathValue("id")

	// Take the same per-project lock that PATCH uses. Without this,
	// an in-flight autosave (holding the lock, mid read-merge-write)
	// races a DELETE: DELETE drops the row out from under PATCH, and
	// PATCH's UPDATE returns sql.ErrNoRows which the handler maps to
	// a 500. With the lock, DELETE waits for PATCH to finish
	// committing, then deletes — and a PATCH that arrives after the
	// DELETE finds the row gone and 404s cleanly.
	release, err := s.acquireKeyedLock(r.Context(), &s.projectMutexes, projectRMWLockSalt, id)
	if err != nil {
		internalError(w, "projects", err)
		return
	}
	defer release()

	// Snapshot pinned_repos BEFORE the cascade fires so we can prune
	// each affected bare clone's worktree registration list after the
	// project's repos/ subtree gets RemoveAll'd. Without the prune,
	// stale entries accumulate in <bare>/worktrees/ — recoverable but
	// noisy, and they block re-creating the same name in a future
	// project's worktree. A read failure here is non-fatal: skip the
	// prune step, the on-disk cleanup still happens.
	var pinned []string
	_ = s.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		existing, e := tx.Projects.Get(r.Context(), orgID, id)
		if e == nil && existing != nil {
			pinned = existing.PinnedRepos
		}
		return nil
	})

	// Stop any in-flight Curator chat for this project BEFORE the DB
	// delete: the goroutine writes terminal cancelled status into
	// curator_requests rows, which the FK cascade is about to drop.
	// Doing it in the right order means a user who deletes a project
	// mid-chat sees a deterministic terminal state on every row
	// rather than relying on cascade behavior to handle in-flight
	// rows. No-op when the curator runtime hasn't been wired (test
	// harnesses, fresh-install before first message).
	if s.curator != nil {
		s.curator.CancelProject(orgID, id, "project deleted")
	}

	if err := s.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		return tx.Projects.Delete(r.Context(), orgID, id)
	}); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			notFound(w, "project")
			return
		}
		internalError(w, "projects", err)
		return
	}

	// Best-effort on-disk cleanup. The DB delete is the source of
	// truth and a stale on-disk dir is recoverable (next run that
	// needs that path will recreate or surface the issue), so a
	// cleanup failure surfaces as a non-fatal warning rather than
	// a 5xx.
	//
	// Two failure modes both produce the same X-Cleanup-Warning so
	// the client always learns that on-disk state may be stale:
	//   - resolving the path failed (UserHomeDir error etc.) —
	//     cleanup couldn't even be attempted
	//   - resolving worked but RemoveAll failed
	//
	// The full error (with absolute path + OS-specific detail) is
	// logged server-side; the header is a generic message so we
	// don't leak filesystem layout to the client.
	const cleanupWarning = "on-disk cleanup of project knowledge dir failed; check server logs"
	dir, dirErr := curator.KnowledgeDir(orgID, id)
	switch {
	case dirErr != nil:
		projectsLog.Warn("cannot resolve knowledge dir, on-disk cleanup skipped", "project", id, "error", dirErr)
		w.Header().Set("X-Cleanup-Warning", cleanupWarning)
	default:
		if rmErr := os.RemoveAll(dir); rmErr != nil && !os.IsNotExist(rmErr) {
			projectsLog.Warn("cleanup of project knowledge dir failed", "project", id, "dir", dir, "error", rmErr)
			w.Header().Set("X-Cleanup-Warning", cleanupWarning)
		}
	}

	// Prune the bare clone of each pinned repo — the per-project
	// worktrees we just RemoveAll'd would otherwise leave behind
	// dangling entries in <bare>/worktrees/ that block re-creating
	// the same name in a future project. Best-effort, post-RemoveAll
	// because prune is what reads the now-missing dirs.
	for _, slug := range pinned {
		owner, repo, ok := splitOwnerRepo(slug)
		if !ok {
			continue
		}
		worktree.PruneCuratorBare(owner, repo)
	}

	w.WriteHeader(http.StatusNoContent)
}

// splitOwnerRepo splits "owner/repo" once. Mirrors the helper in
// internal/curator; duplicated here rather than imported to avoid
// pulling the curator package's surface into the projects handler.
func splitOwnerRepo(slug string) (owner, repo string, ok bool) {
	for i := 0; i < len(slug); i++ {
		if slug[i] == '/' {
			if i == 0 || i == len(slug)-1 {
				return "", "", false
			}
			return slug[:i], slug[i+1:], true
		}
	}
	return "", "", false
}

// validatePinnedRepoShape checks the "owner/repo" slug format and
// returns the normalized (trimmed) slice. Pure — does not touch the
// DB — so it stays cheap to test in isolation and stays usable in
// any future code path that just needs to canonicalize slug input.
//
// The trim-then-persist step matters: without it, " owner/repo "
// would pass validation (the validator trims internally) but get
// stored padded, breaking subsequent lookups by slug.
//
// Returns (normalized, "") on success and (nil, errMsg) on failure.
func validatePinnedRepoShape(repos []string) ([]string, string) {
	out := make([]string, len(repos))
	for i, r := range repos {
		trimmed := strings.TrimSpace(r)
		if trimmed == "" {
			return nil, "pinned_repos[" + strconv.Itoa(i) + "] is empty"
		}
		// Require exactly one '/' with non-empty owner and repo.
		// Anything else (no slash, leading/trailing slash, multiple
		// slashes) is rejected — the slug shape is "owner/repo".
		parts := strings.Split(trimmed, "/")
		if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
			return nil, "pinned_repos[" + strconv.Itoa(i) + "] must be 'owner/repo'"
		}
		out[i] = trimmed
	}
	return out, ""
}

// validatePinnedRepos composes shape validation with the must-be-tracked
// existence check: every slug must be a repo the project's *team* tracks
// (team_github_repos). Repos are per-team, so pinning is
// gated on the team's set rather than the org-wide union — a repo another
// team tracks (and that therefore exists in the shared repo_profiles
// cache) is still rejected here if the project's own team doesn't track
// it. This pins the UX contract — the frontend presents pinned_repos as a
// multi-select over the team's tracked repos, so an untracked slug
// arriving here is a stale client or a hand-crafted curl. Rejecting it up
// front keeps the Curator from later trying to materialize a worktree for
// a repo the team can't authenticate against.
//
// Runs under the caller's claims (team_github_repos_select RLS), so it
// must be invoked inside a WithTx for the requesting user. Returns
// (normalized, "") on success and (nil, errMsg) on failure.
func validatePinnedRepos(ctx context.Context, teamRepos db.TeamGitHubReposStore, teamID string, slugs []string) ([]string, string) {
	out, errMsg := validatePinnedRepoShape(slugs)
	if errMsg != "" {
		return nil, errMsg
	}
	if len(out) == 0 {
		return out, ""
	}

	tracked, err := teamRepos.ListForTeam(ctx, teamID)
	if err != nil {
		return nil, "failed to load tracked repos: " + err.Error()
	}
	// Key both sides case-insensitively: GitHub owner/repo names are
	// case-insensitive and the rest of the codebase folds case (TracksRepoSystem,
	// newlyAddedRepos, the reconcile). A repo re-saved into team_github_repos
	// with different capitalization than the incoming pin is the same repo,
	// so an exact-string match would wrongly reject an otherwise-valid pin.
	known := make(map[string]struct{}, len(tracked))
	for _, t := range tracked {
		known[strings.ToLower(t.Slug())] = struct{}{}
	}
	for _, slug := range out {
		if _, ok := known[strings.ToLower(slug)]; !ok {
			return nil, "pinned_repos: " + slug + " is not a repo this team tracks (add it on the GitHub config page first)"
		}
	}
	return out, ""
}

// validateTrackerKeys validates jira_project_key and linear_project_key
// independently. Each is optional; when non-empty, jira_project_key
// must be present in the team's configured Jira project rules and
// linear_project_key is rejected outright until the Linear integration
// ships. The Jira key is normalized via normalizeJiraProjectKey
// (TrimSpace + ToUpper) to match the canonical form persisted by
// handleSettingsPost — without ToUpper, a stored "SKY" would silently
// fail to match an inbound "sky".
//
// Takes the team's rules as a parameter so the function is testable in
// isolation and so a single PATCH/POST only reads them once even when
// both fields need validating.
//
// Returns the normalized values and an empty error string on success,
// or two empty strings and an error message on failure.
func validateTrackerKeys(teamRules []domain.JiraProjectStatusRules, jiraKey, linearKey string) (string, string, string) {
	jiraNorm := normalizeJiraProjectKey(jiraKey)
	linearNorm := strings.TrimSpace(linearKey)

	if linearNorm != "" {
		// Linear integration is future work. Surfacing this as an
		// explicit "not configured" error matches the UX contract:
		// the frontend renders a disabled Linear picker, so a
		// non-empty value arriving here is a bypass attempt or a
		// stale client.
		return "", "", "linear_project_key: Linear integration is not configured"
	}

	if jiraNorm == "" {
		return "", "", ""
	}
	for _, p := range teamRules {
		if p.ProjectKey == jiraNorm {
			return jiraNorm, "", ""
		}
	}
	return "", "", "jira_project_key: " + jiraNorm + " is not in the configured Jira projects list (add it on the Settings page first)"
}

// queuePendingContextChanges diffs the project's pre-PATCH state
// against the freshly-merged value and writes a curator_pending_context
// row for each field that meaningfully changed. Caller has already
// confirmed CuratorSessionID is non-empty — there's no point queuing
// for a session that doesn't exist yet.
//
// Failures are logged, not returned. The PATCH itself has already
// committed (queueing is best-effort context decoration), and the
// agent will at worst miss a delta on its next turn — recoverable
// from the user's next message or a follow-up PATCH. Surfacing the
// failure as a 500 would imply the settings change failed, which is
// strictly worse.
func queuePendingContextChanges(ctx context.Context, curatorStore db.CuratorStore, orgID string, before, after domain.Project) {
	sessionID := after.CuratorSessionID
	if sessionID == "" {
		return
	}
	if !pinnedReposSetEqual(before.PinnedRepos, after.PinnedRepos) {
		baseline, err := curator.EncodeStringSliceBaseline(before.PinnedRepos)
		if err != nil {
			projectsLog.Warn("encode pinned-repos baseline failed", "project", before.ID, "error", err)
		} else if err := curatorStore.InsertPendingContext(ctx, orgID, before.ID, sessionID, domain.ChangeTypePinnedRepos, baseline); err != nil {
			projectsLog.Warn("queue pinned-repos pending context failed", "project", before.ID, "error", err)
		}
	}
	if before.JiraProjectKey != after.JiraProjectKey {
		baseline, err := curator.EncodeNullableStringBaseline(before.JiraProjectKey)
		if err != nil {
			projectsLog.Warn("encode jira baseline failed", "project", before.ID, "error", err)
		} else if err := curatorStore.InsertPendingContext(ctx, orgID, before.ID, sessionID, domain.ChangeTypeJiraProjectKey, baseline); err != nil {
			projectsLog.Warn("queue jira pending context failed", "project", before.ID, "error", err)
		}
	}
	if before.LinearProjectKey != after.LinearProjectKey {
		baseline, err := curator.EncodeNullableStringBaseline(before.LinearProjectKey)
		if err != nil {
			projectsLog.Warn("encode linear baseline failed", "project", before.ID, "error", err)
		} else if err := curatorStore.InsertPendingContext(ctx, orgID, before.ID, sessionID, domain.ChangeTypeLinearProjectKey, baseline); err != nil {
			projectsLog.Warn("queue linear pending context failed", "project", before.ID, "error", err)
		}
	}
}

// pinnedReposSetEqual compares two pinned-repo slices as sets — order
// AND multiplicity are irrelevant. The diff renderer in the curator
// package treats pinned_repos as a set (added/removed semantics), so
// a PATCH that only reorders OR re-emits the same elements with
// duplicates collapsed (e.g. ["a","a"] → ["a"]) produces no rendered
// diff; without dedup-aware equality here we'd queue a pending row
// that goes through claim/render/finalize and produces an empty note,
// which is wasted I/O and a noisy audit trail.
//
// validatePinnedRepoShape currently does not dedupe, so duplicates
// can in principle land in the column. Comparing as sets here means
// the queue-side check stays correct even if the validator never
// tightens; if the validator does tighten in the future, the dedup
// here is a cheap no-op.
func pinnedReposSetEqual(a, b []string) bool {
	aset := make(map[string]struct{}, len(a))
	for _, v := range a {
		aset[v] = struct{}{}
	}
	bset := make(map[string]struct{}, len(b))
	for _, v := range b {
		bset[v] = struct{}{}
	}
	if len(aset) != len(bset) {
		return false
	}
	for k := range aset {
		if _, ok := bset[k]; !ok {
			return false
		}
	}
	return true
}

// knowledgeFile is the per-file shape returned by the knowledge
// endpoint. We surface the relative path (under
// <KnowledgeDir>/knowledge-base/) rather than the absolute path so
// the API doesn't leak the user's home directory layout.
//
// Content is inlined ONLY for text-shaped files under
// knowledgeInlineMaxBytes — markdown, plain text, json/yaml/etc. that
// the panel renders inline. Non-text files (images, PDFs, archives)
// and large text files leave Content empty; the frontend fetches
// them on-demand from the per-file raw endpoint when a preview is
// actually needed. Two reasons:
//   - Image bytes don't survive a JSON round-trip without base64
//     encoding, which inflates the listing payload an order of
//     magnitude even when the user never expands the file.
//   - The list endpoint runs on every panel mount; inlining a
//     50MB upload would block that response on disk reads the user
//     hasn't asked for yet.
//
// MimeType drives the frontend's render switch (markdown / image /
// text / no-preview). Detected from the file extension first
// (mime.TypeByExtension) and falls back to "application/octet-stream"
// for unknown types.
type knowledgeFile struct {
	Path      string `json:"path"`
	MimeType  string `json:"mime_type"`
	Content   string `json:"content"`
	UpdatedAt string `json:"updated_at"`
	SizeBytes int64  `json:"size_bytes"`
}

// knowledgeInlineMaxBytes caps the size of content we inline in the
// list response. Above this, even text files get fetched lazily from
// the raw endpoint when expanded. 256KB covers ~25k lines of code
// or ~50 pages of prose — well past anything a knowledge file
// reasonably needs to be, and small enough that loading the listing
// stays snappy.
const knowledgeInlineMaxBytes = 256 * 1024

// knowledgeMaxUploadBytes caps individual file uploads at 5MB. The
// Curator's agent has to read these into context at some point, so
// extremely large files are the wrong shape for a knowledge base.
// Multi-file uploads are bounded by knowledgeMaxRequestBytes below.
const knowledgeMaxUploadBytes = 5 * 1024 * 1024

// knowledgeMaxRequestBytes caps a single multipart request at 25MB
// total. Big enough for a handful of images or PDFs at the per-file
// limit, small enough that a malicious or accidental "drag in 500
// files" doesn't lock up the process on disk + memory.
const knowledgeMaxRequestBytes = 25 * 1024 * 1024

// handleProjectKnowledge serves the files under the project's
// knowledge-base directory.
//
// Returns an empty list (not 404) when the project exists but the
// knowledge subdir doesn't, so the frontend can render an empty state
// instead of a noisy error. A real I/O failure (permission denied,
// etc.) is a 500 — but the response body is a generic message: the
// underlying os.* errors include absolute paths under the user's
// home dir, which would otherwise leak filesystem layout to the
// browser. Detail goes to the server log where the operator can find
// it; the client gets a stable string.
func (s *Server) handleProjectKnowledge(w http.ResponseWriter, r *http.Request) {
	orgID, ok := s.requireOrg(w, r)
	if !ok {
		return
	}
	userID := ClaimsFrom(r.Context()).Subject
	id := r.PathValue("id")
	var project *domain.Project
	if err := s.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		var e error
		project, e = tx.Projects.Get(r.Context(), orgID, id)
		return e
	}); err != nil {
		projectsLog.Error("knowledge list: db get failed", "project", id, "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to load project"})
		return
	}
	if project == nil {
		notFound(w, "project")
		return
	}
	root, err := curator.KnowledgeDir(orgID, id)
	if err != nil {
		projectsLog.Error("knowledge list: resolve dir failed", "project", id, "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to read knowledge base"})
		return
	}
	kbDir := filepath.Join(root, "knowledge-base")
	files, err := readKnowledgeFiles(kbDir)
	if err != nil {
		projectsLog.Error("knowledge list: read dir failed", "dir", kbDir, "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to read knowledge base"})
		return
	}
	writeJSON(w, http.StatusOK, files)
}

// readKnowledgeFiles walks one level of the knowledge-base directory
// and returns every regular file in stable order, with mime type
// detected per file and content inlined for text-shaped files under
// knowledgeInlineMaxBytes. Single level by design — the agent is
// supposed to keep a flat layout under knowledge-base/, and recursing
// here would surface scratch state from any nested dirs.
//
// "Doesn't exist" is not an error: a fresh project hasn't had any
// knowledge written yet, so an empty list is the truthful response.
//
// Symlinks and non-regular files are skipped — the agent shouldn't be
// pointing this directory at anything outside it, and reading through
// a symlink could exfiltrate file contents from elsewhere on the
// user's machine via a malicious upload (a `notes.md` symlink to
// ~/.ssh/id_rsa, etc.). Better to skip silently than to follow.
func readKnowledgeFiles(dir string) ([]knowledgeFile, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return []knowledgeFile{}, nil
		}
		return nil, err
	}
	out := make([]knowledgeFile, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		// Skip filenames that the per-file endpoints would reject.
		// Otherwise the listing would surface entries the user can't
		// open or delete from the UI — for instance, a leading-dot
		// name written directly by the agent (`.cache.json`) shows
		// up in the list but the DELETE endpoint refuses the path,
		// leaving the user with phantom files. Filtering at list
		// time keeps the visible set actionable.
		if _, errMsg := sanitizeKnowledgeFilename(name); errMsg != "" {
			continue
		}
		full := filepath.Join(dir, name)
		info, err := os.Lstat(full)
		if err != nil {
			return nil, err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			continue
		}
		if !info.Mode().IsRegular() {
			continue
		}
		mimeType := detectMimeType(name)
		entry := knowledgeFile{
			Path:      name,
			MimeType:  mimeType,
			UpdatedAt: info.ModTime().UTC().Format("2006-01-02T15:04:05Z07:00"),
			SizeBytes: info.Size(),
		}
		if isTextMime(mimeType) && info.Size() <= knowledgeInlineMaxBytes {
			// openNoFollow closes the TOCTOU window between the
			// Lstat above and reading the file: an attacker who can
			// swap the path for a symlink in the intervening
			// microseconds would otherwise have its target's bytes
			// inlined into the listing response. Mirrors the same
			// defense the raw-file endpoint uses; on non-unix
			// builds this falls back to plain os.Open (Windows
			// symlinks are admin-only and out of threat model).
			f, err := openNoFollow(full)
			if err != nil {
				// If the open is a symlink rejection, skip the
				// file rather than aborting the whole listing —
				// the listing has to keep working for unrelated
				// files even if a malicious one slips in.
				if isSymlinkRejection(err) {
					continue
				}
				return nil, err
			}
			body, err := io.ReadAll(io.LimitReader(f, knowledgeInlineMaxBytes+1))
			f.Close()
			if err != nil {
				return nil, err
			}
			// Bound the inline content to the same cap the listing
			// promises — defense in depth in case the file grew
			// between Lstat and read.
			if int64(len(body)) > knowledgeInlineMaxBytes {
				body = body[:knowledgeInlineMaxBytes]
			}
			entry.Content = string(body)
		}
		out = append(out, entry)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out, nil
}

// detectMimeType maps a filename to a content-type using the
// extension table Go's `mime` package ships. Falls back to
// "application/octet-stream" for unknown extensions rather than
// sniffing — sniffing requires reading the file, which is wasted
// work for the listing path where we already skip non-text.
//
// We special-case .md → text/markdown. Go's stdlib mime table
// includes it on most platforms but not all (depends on the system
// mime.types file), so we pin the value to keep the frontend's
// markdown-vs-other branch stable across machines.
func detectMimeType(filename string) string {
	ext := strings.ToLower(filepath.Ext(filename))
	switch ext {
	case ".md", ".markdown":
		return "text/markdown; charset=utf-8"
	}
	if t := mime.TypeByExtension(ext); t != "" {
		return t
	}
	return "application/octet-stream"
}

// isTextMime decides whether a content-type's bytes can be rendered
// as a string in the frontend's <pre> or markdown renderer. Anything
// in the text/ family is text by definition; a few application/
// types (JSON, YAML, XML, JS) are also actually-text and worth
// inlining so the panel can show them without an extra fetch.
func isTextMime(mimeType string) bool {
	if i := strings.Index(mimeType, ";"); i >= 0 {
		mimeType = mimeType[:i]
	}
	mimeType = strings.TrimSpace(mimeType)
	if strings.HasPrefix(mimeType, "text/") {
		return true
	}
	switch mimeType {
	case "application/json",
		"application/yaml",
		"application/x-yaml",
		"application/xml",
		"application/javascript",
		"application/typescript",
		"application/toml":
		return true
	}
	return false
}

// sanitizeKnowledgeFilename hardens an uploaded filename before it
// touches the filesystem. The agent is the primary writer here, but
// uploads come from the browser and a malicious or careless filename
// shouldn't be able to escape the knowledge-base/ subdir or shadow
// system files.
//
// Rules:
//   - Strip any path component via filepath.Base (platform-aware:
//     uses '/' on Unix, '/' or '\\' on Windows).
//   - Reject empty results, "..", "." segments.
//   - Reject leading dot (no hidden files — the agent's own scratch
//     state lives at the project root with leading dots, and we don't
//     want uploads colliding with it).
//   - Reject the OS path separator post-Base (defense in depth).
//
// We deliberately don't normalize backslash to slash before Base: on
// Unix '\\' is a legitimate filename character (e.g. `a\b.md` is one
// file, not two path components), and the manual replace would have
// turned that filename into `b.md` for upload but left it alone in
// the listing — leaving the user with a listing entry that the raw
// and delete endpoints can't resolve. Browsers strip path components
// from multipart filenames before sending them, so the cross-platform
// case the manual replace was guarding against doesn't actually arise
// in practice.
//
// Returns the sanitized base name and an error message string for
// the handler to surface as a 400.
func sanitizeKnowledgeFilename(raw string) (string, string) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return "", "filename is required"
	}
	base := filepath.Base(trimmed)
	if base == "." || base == ".." || base == "/" || base == "" {
		return "", "filename is invalid"
	}
	if strings.HasPrefix(base, ".") {
		return "", "filename cannot start with a dot"
	}
	// Reject only the OS's actual path separator — '\\' is a literal
	// character on Unix, not a separator, and stripping it from a
	// legitimate filename would desync the listing from the per-file
	// endpoints.
	if strings.ContainsRune(base, filepath.Separator) {
		return "", "filename cannot contain path separators"
	}
	return base, ""
}

// resolveKnowledgePath maps a URL path parameter to an absolute file
// path inside the project's knowledge-base directory and validates
// that the resolved path stays within. Two layers of defense:
//  1. URL-decode + sanitize via sanitizeKnowledgeFilename, which
//     rejects "..", path separators, and leading dots.
//  2. Resolve to absolute via filepath.Join(kbDir, name) and re-check
//     the result has kbDir as its prefix. Belt-and-suspenders against
//     anything the sanitizer might miss on a future filesystem.
//
// Errors that include filesystem detail (KnowledgeDir's UserHomeDir
// failure, etc.) are logged server-side; the returned message is
// generic so the response body doesn't leak path layout to the
// browser.
//
// Returns (kbDir, fullPath, status, errMsg). status is 0 on success,
// http.StatusBadRequest for client-side mistakes (bad input, path
// traversal), and http.StatusInternalServerError for server-side
// failures (UserHomeDir, etc.). Earlier versions collapsed both
// cases to a 400, which misclassified internal failures as bad
// input and made operator triage harder.
func resolveKnowledgePath(orgID, projectID, rawPath string) (string, string, int, string) {
	root, err := curator.KnowledgeDir(orgID, projectID)
	if err != nil {
		projectsLog.Error("resolve knowledge dir failed", "project", projectID, "error", err)
		return "", "", http.StatusInternalServerError, "failed to resolve knowledge dir"
	}
	kbDir := filepath.Join(root, "knowledge-base")
	decoded, err := url.PathUnescape(rawPath)
	if err != nil {
		return "", "", http.StatusBadRequest, "invalid path encoding"
	}
	name, errMsg := sanitizeKnowledgeFilename(decoded)
	if errMsg != "" {
		return "", "", http.StatusBadRequest, errMsg
	}
	full := filepath.Join(kbDir, name)
	rel, err := filepath.Rel(kbDir, full)
	if err != nil || strings.HasPrefix(rel, "..") || rel == ".." {
		return "", "", http.StatusBadRequest, "path escapes knowledge-base directory"
	}
	return kbDir, full, 0, ""
}

// handleProjectKnowledgeFile streams the raw bytes of a single
// knowledge file. Used by the frontend for image <img src=> tags,
// preview-not-available "Open" links, and lazy-fetch of large text
// files. Sets Content-Disposition: inline so the browser previews
// rather than downloads — this is a sidebar viewer, not a download
// hub. Users who want to save a file can use Save As from there.
//
// Symlink defense: Lstat (not Stat) gates regular-file admission.
// Stat follows symlinks and would happily green-light a symlink
// whose target is a regular file outside the knowledge-base dir,
// leaking arbitrary file contents through this endpoint. Also
// switches from http.ServeFile (which re-opens the path and could
// be raced) to http.ServeContent against an already-open file
// handle so the file we serve is the one we just verified.
func (s *Server) handleProjectKnowledgeFile(w http.ResponseWriter, r *http.Request) {
	orgID, ok := s.requireOrg(w, r)
	if !ok {
		return
	}
	userID := ClaimsFrom(r.Context()).Subject
	id := r.PathValue("id")
	var project *domain.Project
	if err := s.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		var e error
		project, e = tx.Projects.Get(r.Context(), orgID, id)
		return e
	}); err != nil {
		projectsLog.Error("knowledge fetch: db get failed", "project", id, "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to load project"})
		return
	}
	if project == nil {
		notFound(w, "project")
		return
	}
	_, full, status, errMsg := resolveKnowledgePath(orgID, id, r.PathValue("path"))
	if errMsg != "" {
		writeJSON(w, status, map[string]string{"error": errMsg})
		return
	}
	linfo, err := os.Lstat(full)
	if err != nil {
		if os.IsNotExist(err) {
			notFound(w, "file")
			return
		}
		projectsLog.Error("knowledge fetch: lstat failed", "path", full, "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to read file"})
		return
	}
	if linfo.Mode()&os.ModeSymlink != 0 || !linfo.Mode().IsRegular() {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "not a regular file"})
		return
	}
	// O_NOFOLLOW closes the TOCTOU window between Lstat and Open:
	// even if the path was swapped for a symlink in the microseconds
	// between, the kernel refuses to traverse it. Without this, the
	// Lstat check above would be advisory rather than authoritative.
	// See open_nofollow_unix.go / open_nofollow_other.go for the
	// platform split.
	f, err := openNoFollow(full)
	if err != nil {
		if os.IsNotExist(err) {
			notFound(w, "file")
			return
		}
		// Distinguish ELOOP (symlink rejected by O_NOFOLLOW) from
		// other openNoFollow failures. Permission denied, EIO, and
		// similar are server-side problems that should surface as
		// 500 so the operator can spot them — collapsing all
		// failures to "not a regular file" 400 would mask real
		// production issues behind a misleading client error.
		projectsLog.Error("knowledge fetch: open failed", "path", full, "error", err)
		if isSymlinkRejection(err) {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "not a regular file"})
			return
		}
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to read file"})
		return
	}
	defer f.Close()

	w.Header().Set("Content-Type", detectMimeType(filepath.Base(full)))
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Disposition", "inline; filename="+strconv.Quote(filepath.Base(full)))
	http.ServeContent(w, r, filepath.Base(full), linfo.ModTime(), f)
}

// handleProjectKnowledgeUpload accepts one or more files via
// multipart/form-data and writes each to the project's knowledge-base
// directory. The dir is created lazily on first upload so a project
// that's never been uploaded to (and never been chatted with) doesn't
// accumulate empty directories.
//
// Conflict policy: REJECT. If a file with the same sanitized name
// already exists, that file is reported as a conflict in the per-file
// results — the user explicitly picked "delete the old one first"
// over auto-rename or overwrite. Implementation uses O_CREATE|O_EXCL
// so the check + write is atomic against a concurrent upload of the
// same name.
//
// Per-file size limit and total request limit are enforced during
// upload handling. If a file exceeds knowledgeMaxUploadBytes while
// streaming, that file is reported as a failed per-file result and any
// partially-written file is removed.
//
// Multi-file requests are partial-success: each file is processed
// independently, and the response is HTTP 200 with per-file results.
// A duplicate filename or oversize-file failure doesn't block
// siblings from succeeding.
func (s *Server) handleProjectKnowledgeUpload(w http.ResponseWriter, r *http.Request) {
	orgID, ok := s.requireOrg(w, r)
	if !ok {
		return
	}
	id := r.PathValue("id")

	// Take the same per-project lock that PATCH and DELETE use.
	// Without it, an upload that has just verified the project
	// exists could race a DELETE: DELETE drops the row and
	// RemoveAll's the project dir, then this handler re-creates
	// knowledge-base/ and writes files into a project that no
	// longer exists in the DB — leaving orphaned on-disk state
	// after a successful delete. Holding the lock across the
	// existence check + write makes the upload visibly atomic
	// to a concurrent delete.
	release, err := s.acquireKeyedLock(r.Context(), &s.projectMutexes, projectRMWLockSalt, id)
	if err != nil {
		internalError(w, "projects", err)
		return
	}
	defer release()

	userID := ClaimsFrom(r.Context()).Subject
	var project *domain.Project
	if err := s.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		var e error
		project, e = tx.Projects.Get(r.Context(), orgID, id)
		return e
	}); err != nil {
		projectsLog.Error("knowledge upload: db get failed", "project", id, "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to load project"})
		return
	}
	if project == nil {
		notFound(w, "project")
		return
	}
	root, err := curator.KnowledgeDir(orgID, id)
	if err != nil {
		projectsLog.Error("knowledge upload: resolve dir failed", "project", id, "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to resolve knowledge dir"})
		return
	}
	kbDir := filepath.Join(root, "knowledge-base")
	if err := os.MkdirAll(kbDir, 0o755); err != nil {
		projectsLog.Error("knowledge upload: mkdir failed", "dir", kbDir, "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to create knowledge dir"})
		return
	}

	// MaxBytesReader caps the WHOLE request, including form fields
	// and per-file streams. Per-file caps are enforced separately
	// during the copy below so a single oversize file doesn't poison
	// siblings that were within budget.
	r.Body = http.MaxBytesReader(w, r.Body, knowledgeMaxRequestBytes)
	if err := r.ParseMultipartForm(8 << 20); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "multipart parse: " + err.Error()})
		return
	}
	defer func() {
		if r.MultipartForm != nil {
			if err := r.MultipartForm.RemoveAll(); err != nil {
				projectsLog.Warn("knowledge upload: cleanup multipart form failed", "project", id, "error", err)
			}
		}
	}()
	files := r.MultipartForm.File["file"]
	if len(files) == 0 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "no files in upload (use form field 'file')"})
		return
	}

	type uploadResult struct {
		Path     string `json:"path,omitempty"`
		Original string `json:"original"`
		Error    string `json:"error,omitempty"`
	}
	results := make([]uploadResult, 0, len(files))
	for _, fh := range files {
		original := fh.Filename
		name, errMsg := sanitizeKnowledgeFilename(original)
		if errMsg != "" {
			results = append(results, uploadResult{Original: original, Error: errMsg})
			continue
		}
		if fh.Size > knowledgeMaxUploadBytes {
			results = append(results, uploadResult{
				Original: original,
				Error:    fmt.Sprintf("file exceeds %d-byte limit", knowledgeMaxUploadBytes),
			})
			continue
		}
		full := filepath.Join(kbDir, name)
		if errMsg := writeUploadedFile(fh, full); errMsg != "" {
			results = append(results, uploadResult{Original: original, Error: errMsg})
			continue
		}
		results = append(results, uploadResult{Path: name, Original: original})
	}

	// Bump updated_at if anything actually landed. A pure-failures
	// request (every file rejected for conflict / size / sanitize)
	// leaves the on-disk state unchanged, so we don't need to bump
	// the timestamp in that case.
	wroteAny := false
	for _, r := range results {
		if r.Error == "" {
			wroteAny = true
			break
		}
	}
	if wroteAny {
		if err := s.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
			return tx.Projects.BumpUpdatedAt(r.Context(), orgID, id)
		}); err != nil {
			// Log but don't fail the upload — the files are on disk
			// and the user expects the response to reflect that.
			// The activity timestamp is a hint for follow-on display,
			// not part of the upload's correctness contract.
			projectsLog.Warn("knowledge upload: bump updated_at failed", "project", id, "error", err)
		}
	}

	// 207-ish semantics in a 200: each entry carries its own status.
	// We pick 200 here rather than 207 (Multi-Status) because the
	// rest of the API speaks plain JSON, not WebDAV; a single shape
	// keeps client handling consistent across endpoints.
	writeJSON(w, http.StatusOK, map[string]any{"results": results})
}

// writeUploadedFile copies the uploaded file's bytes to the target
// path atomically: stream into a sibling temp file, then os.Link
// the temp into the destination. Concurrent listings / raw-fetches
// see either no file (pre-link) or the fully-written file
// (post-link) — never the partial bytes mid-stream.
//
// Conflict detection rides on the link step: os.Link refuses if the
// destination already exists, which is the race-safe enforcement of
// "reject conflict" — a quick os.Stat check beforehand is racy
// because two uploads could both see "doesn't exist" and proceed.
//
// Temp files use a leading-dot prefix so the listing's
// sanitizeKnowledgeFilename filter hides them from the user-visible
// listing while the upload is in progress.
//
// Cleanup invariants: the temp file is removed on every error path
// (and on the success path after link). The destination is never
// touched if any step fails. On Windows os.Remove fails on a
// still-open file, so the temp handle is closed before the remove.
func writeUploadedFile(fh *multipart.FileHeader, dst string) string {
	src, err := fh.Open()
	if err != nil {
		return "open upload: " + err.Error()
	}
	defer src.Close()

	dir := filepath.Dir(dst)
	tmp, err := os.CreateTemp(dir, ".upload-*.partial")
	if err != nil {
		return "create temp file: " + err.Error()
	}
	tmpName := tmp.Name()
	closed := false
	closeOnce := func() {
		if !closed {
			tmp.Close()
			closed = true
		}
	}
	// removePartial cleans the temp file. Safe to call multiple times
	// (os.Remove of a missing file is benign on both platforms).
	removePartial := func() {
		closeOnce()
		_ = os.Remove(tmpName)
	}

	// Cap the per-file copy at knowledgeMaxUploadBytes + 1 so we
	// detect oversize uploads even if the multipart header lied
	// about Size. If we hit the cap, remove the partial file.
	limited := io.LimitReader(src, knowledgeMaxUploadBytes+1)
	written, err := io.Copy(tmp, limited)
	if err != nil {
		removePartial()
		return "write file: " + err.Error()
	}
	if written > knowledgeMaxUploadBytes {
		removePartial()
		return fmt.Sprintf("file exceeds %d-byte limit", knowledgeMaxUploadBytes)
	}
	if err := tmp.Close(); err != nil {
		closed = true
		_ = os.Remove(tmpName)
		return "close temp file: " + err.Error()
	}
	closed = true

	// Atomic publish via link: succeeds iff dst doesn't exist, races
	// concurrent uploads cleanly, and never leaves a partial dst on
	// disk. After link, both paths point at the same inode; the
	// remove of tmpName drops the temp name and leaves dst as the
	// persistent file.
	if err := os.Link(tmpName, dst); err != nil {
		_ = os.Remove(tmpName)
		if errors.Is(err, os.ErrExist) {
			return "file already exists — delete it first"
		}
		return "publish file: " + err.Error()
	}
	if err := os.Remove(tmpName); err != nil {
		// Failed to remove the temp name post-link. dst is correct;
		// the orphan is just a leading-dot file the listing will
		// filter out. Log and move on.
		projectsLog.Warn("knowledge upload: remove temp failed", "path", tmpName, "error", err)
	}
	return ""
}

// handleProjectKnowledgeDelete removes a single file from the
// knowledge-base directory. The Curator's RemoveAll on project
// delete handles whole-dir cleanup; this is the per-file pair to
// the upload endpoint, mirrored on the frontend by the per-file
// trash button.
func (s *Server) handleProjectKnowledgeDelete(w http.ResponseWriter, r *http.Request) {
	orgID, ok := s.requireOrg(w, r)
	if !ok {
		return
	}
	id := r.PathValue("id")

	// Same per-project lock as upload + project PATCH/DELETE.
	// Holding the lock keeps the file remove + updated_at bump
	// atomic from the perspective of other writers, so a racing
	// PATCH can't sandwich a stale snapshot of the project row
	// over the timestamp bump.
	release, err := s.acquireKeyedLock(r.Context(), &s.projectMutexes, projectRMWLockSalt, id)
	if err != nil {
		internalError(w, "projects", err)
		return
	}
	defer release()

	userID := ClaimsFrom(r.Context()).Subject
	var project *domain.Project
	if err := s.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		var e error
		project, e = tx.Projects.Get(r.Context(), orgID, id)
		return e
	}); err != nil {
		projectsLog.Error("knowledge delete: db get failed", "project", id, "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to load project"})
		return
	}
	if project == nil {
		notFound(w, "project")
		return
	}
	_, full, status, errMsg := resolveKnowledgePath(orgID, id, r.PathValue("path"))
	if errMsg != "" {
		writeJSON(w, status, map[string]string{"error": errMsg})
		return
	}
	if err := os.Remove(full); err != nil {
		if os.IsNotExist(err) {
			notFound(w, "file")
			return
		}
		projectsLog.Error("knowledge delete: remove failed", "path", full, "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to remove file"})
		return
	}
	if err := s.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		return tx.Projects.BumpUpdatedAt(r.Context(), orgID, id)
	}); err != nil {
		projectsLog.Error("knowledge delete: bump updated_at failed", "project", id, "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to update project state"})
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
