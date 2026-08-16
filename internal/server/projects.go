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
	"github.com/sky-ai-eng/triage-factory/internal/server/httpx"
	"github.com/sky-ai-eng/triage-factory/internal/server/teamscope"
	"github.com/sky-ai-eng/triage-factory/internal/worktree"
)

// Projects are the data layer underneath the Curator stack —
// the long-lived per-project context that the rest of the
// family hangs work onto. This file is pure CRUD + on-disk knowledge
// dir cleanup; the Curator runtime, classifier, and UI all land in
// later tickets and can hit the same handlers without changes here.

// writeKnowledgePathError shapes resolveKnowledgePath's (status, message)
// pair: a client-side path fault names the path segment it came from, a
// server-side resolution failure is a plain 500. The message is already
// generic — resolveKnowledgePath logs the underlying error, which can carry
// filesystem layout, and only returns text safe to hand back.
func writeKnowledgePathError(w http.ResponseWriter, status int, msg string) {
	if status == http.StatusBadRequest {
		httpx.WriteErrors(w, http.StatusBadRequest, httpx.ErrorItem{
			Reason: httpx.ReasonInvalidParam, Message: msg, Field: "path",
		})
		return
	}
	httpx.WriteErrors(w, status, httpx.ErrorItem{Reason: httpx.ReasonInternal, Message: msg})
}

// writeNotARegularFile answers a knowledge path that resolved to something
// this endpoint will not stream — a symlink, a directory, a device node.
func writeNotARegularFile(w http.ResponseWriter) {
	httpx.WriteErrors(w, http.StatusBadRequest, httpx.ErrorItem{
		Reason: httpx.ReasonInvalidParam, Message: "not a regular file", Field: "path",
	})
}

// writeInvalidVisibility answers the one enum both create and PATCH validate.
func writeInvalidVisibility(w http.ResponseWriter) {
	httpx.WriteErrors(w, http.StatusBadRequest, httpx.ErrorItem{
		Reason:  httpx.ReasonInvalidField,
		Message: "visibility must be one of private, team, org",
		Field:   "visibility",
	})
}

// projectIDOr404 guards the {id} path value on every project-scoped route —
// projects.id is a uuid column on Postgres. See uuidPathOr404. Shared with the
// curator and knowledge surfaces, which address projects the same way.
func projectIDOr404(w http.ResponseWriter, r *http.Request) (string, bool) {
	return uuidPathOr404(w, r, "id", "project")
}

// createProjectRequest is the POST body shape. Most fields are
// optional — a project starts as an empty shell named by the user
// and gets filled in over time (description, pinned repos, summary).
type createProjectRequest struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	// PinnedRepositoryIDs is the pinned set as registry row ids. The field is
	// named for what it holds rather than reusing the old `pinned_repos`: an
	// element's meaning changed, and strict decoding turning an old-shaped
	// body into a loud 400 is the point — the alternative is a body full of
	// names being accepted and every one of them failing to resolve.
	PinnedRepositoryIDs []string `json:"pinned_repository_ids"`
	JiraProjectKey      string   `json:"jira_project_key"`
	LinearProjectKey    string   `json:"linear_project_key"`
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
// "absent → leave unchanged" from "explicit → overwrite".
// PinnedRepositoryIDs uses *[]string so a client can clear it with []
// without colliding with the absent case.
type patchProjectRequest struct {
	Name        *string `json:"name"`
	Description *string `json:"description"`
	// Registry row ids — see createProjectRequest.PinnedRepositoryIDs.
	PinnedRepositoryIDs       *[]string `json:"pinned_repository_ids"`
	JiraProjectKey            *string   `json:"jira_project_key"`
	LinearProjectKey          *string   `json:"linear_project_key"`
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
	if !httpx.DecodeJSONStrict(w, r, &req) {
		return
	}
	name := strings.TrimSpace(req.Name)
	if name == "" {
		httpx.WriteErrors(w, http.StatusBadRequest, httpx.ErrorItem{
			Reason: httpx.ReasonMissingField, Message: "name is required", Field: "name",
		})
		return
	}
	jiraKey := strings.TrimSpace(req.JiraProjectKey)
	linearKey := strings.TrimSpace(req.LinearProjectKey)

	visibility := strings.TrimSpace(req.Visibility)
	if visibility == "" {
		visibility = domain.ProjectVisibilityTeam
	}
	if !domain.ValidProjectVisibility(visibility) {
		writeInvalidVisibility(w)
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
		trackerErrField  string
		visibilityErrMsg string
		teamJiraRules    []domain.JiraProjectStatusRules
		created          *projectJSON
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
		} else if len(req.PinnedRepositoryIDs) > 0 || jiraKey != "" || linearKey != "" {
			// private/org projects have no team to validate these
			// against in v1 — they start as a bare shell (name +
			// description + knowledge base); pinning resources requires
			// team visibility.
			visibilityErrMsg = "pinned repos and tracker projects require team visibility"
			return nil
		}
		pinned, pinnedErrMsg = validatePinnedRepos(r.Context(), tx.Repos, tx.TeamGitHubRepos, orgID, teamID, req.PinnedRepositoryIDs)
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
			jiraKey, linearKey, trackerErrField, trackerErrMsg = validateTrackerKeys(teamJiraRules, jiraKey, linearKey)
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
			SpecAuthorshipBlueprintID: specBlueprintID,
			Visibility:                visibility,
		})
		if createErr != nil {
			return createErr
		}
		row, getErr := tx.Projects.Get(r.Context(), orgID, id)
		if getErr != nil || row == nil {
			return getErr
		}
		rendered, getErr := projectToJSON(r.Context(), tx.Repos, orgID, *row, nil)
		if getErr != nil {
			return getErr
		}
		created = &rendered
		return nil
	}); err != nil {
		// teamscope.ErrNoTeam gets a project-flavored message (mentioning the
		// private/org escape hatch) ahead of WriteIfSelectionError's generic
		// one; ErrRequired/ErrForbidden fall through to that generic 400.
		if errors.Is(err, teamscope.ErrNoTeam) {
			httpx.WriteErrors(w, http.StatusBadRequest, httpx.ErrorItem{
				Reason:  httpx.ReasonInvalidField,
				Message: "join a team to create a team-visibility project, or choose private/org visibility",
				Field:   "visibility",
			})
			return
		}
		if teamscope.WriteIfSelectionError(w, err) {
			return
		}
		if errors.Is(err, db.ErrVisibilityForbidden) {
			forbidden(w, "you don't have permission to create a project with that visibility")
			return
		}
		projectsLog.Error("create failed", "error", err)
		internalError(w, "projects", err)
		return
	}
	if visibilityErrMsg != "" {
		httpx.WriteErrors(w, http.StatusBadRequest, httpx.ErrorItem{
			Reason: httpx.ReasonInvalidField, Message: visibilityErrMsg, Field: "visibility",
		})
		return
	}
	if pinnedErrMsg != "" {
		httpx.WriteErrors(w, http.StatusBadRequest, httpx.ErrorItem{
			Reason: httpx.ReasonInvalidField, Message: pinnedErrMsg, Field: pinnedRepoIDsField,
		})
		return
	}
	if trackerErrField != "" {
		httpx.WriteErrors(w, http.StatusBadRequest, httpx.ErrorItem{
			Reason: httpx.ReasonInvalidField, Message: trackerErrMsg, Field: trackerErrField,
		})
		return
	}
	writeJSON(w, http.StatusCreated, created)
}

// projectListRequest is the body of POST /api/projects/list. The resource is
// "the projects I can see", which RLS decides; the body carries no filters.
type projectListRequest struct {
	httpx.PageRequest
}

// POST /api/projects/list
func (s *Server) handleProjectList(w http.ResponseWriter, r *http.Request) {
	orgID, ok := s.requireOrg(w, r)
	if !ok {
		return
	}
	userID := ClaimsFrom(r.Context()).Subject

	var req projectListRequest
	if !httpx.DecodeJSONStrict(w, r, &req) {
		return
	}
	var v httpx.Validation
	page := httpx.ResolvePage(&v, req.PageRequest, httpx.FilterFingerprint(struct{}{}), 0)
	if v.Flush(w, http.StatusBadRequest) {
		return
	}

	var (
		projects []projectJSON
		total    int
	)
	if err := s.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		rows, count, e := tx.Projects.List(r.Context(), orgID, db.ListOpts{Limit: page.Limit, Offset: page.Offset})
		if e != nil {
			return e
		}
		total = count
		// Rendered inside the read's own transaction: the pin ids come from
		// the same registry the pin names were joined out of, so the two
		// halves of one row cannot be read a rename apart.
		projects, e = projectsToJSON(r.Context(), tx.Repos, orgID, rows)
		return e
	}); err != nil {
		internalError(w, "projects", err)
		return
	}
	httpx.WriteList(w, page, projects, total)
}

func (s *Server) handleProjectGet(w http.ResponseWriter, r *http.Request) {
	orgID, ok := s.requireOrg(w, r)
	if !ok {
		return
	}
	userID := ClaimsFrom(r.Context()).Subject
	id, ok := projectIDOr404(w, r)
	if !ok {
		return
	}
	var project *projectJSON
	if err := s.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		row, e := tx.Projects.Get(r.Context(), orgID, id)
		if e != nil || row == nil {
			return e
		}
		rendered, e := projectToJSON(r.Context(), tx.Repos, orgID, *row, nil)
		if e != nil {
			return e
		}
		project = &rendered
		return nil
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
	userID := ClaimsFrom(r.Context()).Subject
	id, ok := projectIDOr404(w, r)
	if !ok {
		return
	}
	preview, err := projectbundle.Preview(r.Context(), s.tx, s.kb, orgID, userID, id)
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
	id, ok := projectIDOr404(w, r)
	if !ok {
		return
	}
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
	stream, err := projectbundle.Export(r.Context(), s.tx, s.kb, orgID, userID, id)
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
	r.Body = http.MaxBytesReader(w, r.Body, projectBundleMaxUploadBytes)
	if err := r.ParseMultipartForm(64 << 20); err != nil {
		httpx.WriteErrors(w, http.StatusBadRequest, httpx.ErrorItem{
			Reason: httpx.ReasonInvalidBody, Message: "multipart parse: " + err.Error(),
		})
		return
	}
	defer func() {
		if r.MultipartForm != nil {
			_ = r.MultipartForm.RemoveAll()
		}
	}()

	file, _, err := r.FormFile("bundle")
	if err != nil {
		httpx.WriteErrors(w, http.StatusBadRequest, httpx.ErrorItem{
			Reason: httpx.ReasonMissingField, Message: "bundle file is required (form field 'bundle')", Field: "bundle",
		})
		return
	}
	defer file.Close()

	size, err := file.Seek(0, io.SeekEnd)
	if err != nil {
		httpx.WriteErrors(w, http.StatusBadRequest, httpx.ErrorItem{
			Reason: httpx.ReasonInvalidBody, Message: "failed to determine bundle size", Field: "bundle",
		})
		return
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		httpx.WriteErrors(w, http.StatusBadRequest, httpx.ErrorItem{
			Reason: httpx.ReasonInvalidBody, Message: "failed to rewind bundle", Field: "bundle",
		})
		return
	}

	userID := ClaimsFrom(r.Context()).Subject
	// Resolve the acting team inside WithTx so teams_select RLS gates
	// the lookup under the user's claims. Import's own DB writes run in
	// a separate WithTx inside projectbundle.Import, AFTER zip
	// extraction — a multi-GiB extraction must not hold a claims-bound
	// connection. A multi-team caller picks via the optional team_id
	// form field (same contract as project create); a single-team
	// caller (every local install) falls back to their sole team.
	var teamID string
	if err := s.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		var e error
		teamID, e = teamscope.ResolveActing(r.Context(), tx.Teams, tx.Users, orgID, userID, r.FormValue("team_id"))
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
		s.tx,
		s.curatorStore,
		s.kb,
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
			httpx.WriteErrors(w, http.StatusConflict, httpx.ErrorItem{
				Reason:  httpx.ReasonDuplicateName,
				Message: dupNameErr.Error() + " — rename or delete the existing project first",
			})
			return
		}
		var missingReposErr *projectbundle.MissingReposError
		if errors.As(err, &missingReposErr) {
			// One item per unreachable repo, each carrying the probe's own
			// reason: the envelope reports every failure, so the uploader
			// sees the whole list instead of the first name.
			items := make([]httpx.ErrorItem, 0, len(missingReposErr.Missing))
			for _, m := range missingReposErr.Missing {
				items = append(items, httpx.ErrorItem{
					Reason:  httpx.ReasonNotFound,
					Message: "pinned repo " + m.Repo + " is unreachable with this workspace's GitHub credentials: " + m.Error,
					Field:   "pinned_repos",
				})
			}
			httpx.WriteErrors(w, http.StatusConflict, items...)
			return
		}
		if errors.Is(err, projectbundle.ErrPermissionDenied) {
			// Visible resource, refused action: adding to the team's tracked
			// set is an admin write. Surfaced as a 500 before this.
			forbidden(w, err.Error())
			return
		}
		if projectbundle.IsClientFault(err) {
			// Malformed zip, unparseable manifest, an entry past the
			// extraction budget, a path that escapes the tree: all properties
			// of the uploaded file, all previously answered 500.
			httpx.WriteErrors(w, http.StatusBadRequest, httpx.ErrorItem{
				Reason: httpx.ReasonInvalidBody, Message: err.Error(), Field: "bundle",
			})
			return
		}
		internalError(w, "projects", err)
		return
	}

	// The import returns the domain row; the response carries the same wire
	// shape every other project read does, pins included as registry ids.
	var rendered projectJSON
	if err := s.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		var e error
		rendered, e = projectToJSON(r.Context(), tx.Repos, orgID, *project, nil)
		return e
	}); err != nil {
		internalError(w, "projects", fmt.Errorf("render imported project %s: %w", project.ID, err))
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{
		"project":  rendered,
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
	id, ok := projectIDOr404(w, r)
	if !ok {
		return
	}
	var req patchProjectRequest
	if !httpx.DecodeJSONStrict(w, r, &req) {
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
			httpx.WriteErrors(w, http.StatusBadRequest, httpx.ErrorItem{
				Reason: httpx.ReasonInvalidField, Message: "name cannot be empty", Field: "name",
			})
			return
		}
		updated.Name = trimmed
	}
	if req.Description != nil {
		updated.Description = *req.Description
	}
	if req.PinnedRepositoryIDs != nil {
		// Validate against the project's OWN team, not the org default — a
		// non-default-team project's pins must be tracked by that team.
		// existing.TeamID is populated by Projects.Get; the store preserves
		// team_id on update. Empty is a legitimate state since TFAC-562 (a
		// private/org-visibility project has no team), not a malformed row —
		// pinned repos just aren't available for one in v1.
		if existing.TeamID == "" {
			httpx.WriteErrors(w, http.StatusBadRequest, httpx.ErrorItem{
				Reason:  httpx.ReasonInvalidField,
				Message: "this project has no team — pinned repos aren't available for a private/org visibility project",
				Field:   pinnedRepoIDsField,
			})
			return
		}
		var pinned []string
		var errMsg string
		if err := s.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
			pinned, errMsg = validatePinnedRepos(r.Context(), tx.Repos, tx.TeamGitHubRepos, orgID, existing.TeamID, *req.PinnedRepositoryIDs)
			return nil
		}); err != nil {
			internalError(w, "projects", err)
			return
		}
		if errMsg != "" {
			httpx.WriteErrors(w, http.StatusBadRequest, httpx.ErrorItem{
				Reason: httpx.ReasonInvalidField, Message: errMsg, Field: pinnedRepoIDsField,
			})
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
				httpx.WriteErrors(w, http.StatusBadRequest, httpx.ErrorItem{
					Reason:  httpx.ReasonInvalidField,
					Message: "this project has no team — a tracker project key isn't available for a private/org visibility project",
					Field:   "jira_project_key",
				})
				return
			}
			var teamRules []domain.JiraProjectStatusRules
			if err := s.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
				var rerr error
				teamRules, rerr = tx.JiraStatusRules.ListForTeam(r.Context(), existing.TeamID)
				return rerr
			}); err != nil {
				internalError(w, "projects", fmt.Errorf("load jira rules for project %s: %w", existing.ID, err))
				return
			}
			jiraKey, _, errField, errMsg := validateTrackerKeys(teamRules, *req.JiraProjectKey, "")
			if errMsg != "" {
				httpx.WriteErrors(w, http.StatusBadRequest, httpx.ErrorItem{
					Reason: httpx.ReasonInvalidField, Message: errMsg, Field: errField,
				})
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
		_, linearKey, errField, errMsg := validateTrackerKeys(nil, "", *req.LinearProjectKey)
		if errMsg != "" {
			httpx.WriteErrors(w, http.StatusBadRequest, httpx.ErrorItem{
				Reason: httpx.ReasonInvalidField, Message: errMsg, Field: errField,
			})
			return
		}
		updated.LinearProjectKey = linearKey
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
				httpx.WriteErrors(w, http.StatusBadRequest, httpx.ErrorItem{
					Reason:  httpx.ReasonInvalidField,
					Message: "this project has no team — a spec-authorship blueprint override isn't available for a private/org visibility project",
					Field:   "spec_authorship_blueprint_id",
				})
				return
			}
			var visible []domain.Blueprint
			if err := s.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
				var e error
				// Unwindowed (ListOpts zero Limit): this is a membership
				// check over the team's blueprints, not a browse, and a page
				// would make "is this blueprint in my team" depend on where it
				// sorted.
				visible, _, e = tx.Blueprints.List(r.Context(), orgID, db.BlueprintListFilter{TeamID: existing.TeamID}, db.Unwindowed)
				return e
			}); err != nil {
				internalError(w, "projects", fmt.Errorf("list blueprints for project %s: %w", existing.ID, err))
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
				httpx.WriteErrors(w, http.StatusBadRequest, httpx.ErrorItem{
					Reason:  httpx.ReasonInvalidField,
					Message: "spec_authorship_blueprint_id references unknown blueprint",
					Field:   "spec_authorship_blueprint_id",
				})
				return
			}
		}
		updated.SpecAuthorshipBlueprintID = trimmed
	}
	if req.Visibility != nil {
		v := strings.TrimSpace(*req.Visibility)
		if !domain.ValidProjectVisibility(v) {
			writeInvalidVisibility(w)
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
		httpx.WriteErrors(w, http.StatusBadRequest, httpx.ErrorItem{
			Reason:  httpx.ReasonInvalidField,
			Message: "this project has no team — team visibility isn't available for it",
			Field:   "visibility",
		})
		return
	}

	if err := s.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		return tx.Projects.Update(r.Context(), orgID, updated)
	}); err != nil {
		if errors.Is(err, db.ErrVisibilityForbidden) {
			forbidden(w, "you don't have permission to change this project to that visibility")
			return
		}
		internalError(w, "projects", err)
		return
	}

	// Queue context-change deltas for every LIVE curator conversation of
	// the project (all creators — a teammate's mid-session agent needs the
	// note just as much as the PATCHing user's, and the app-pool
	// private-visibility RLS couldn't reach their conversations, which is
	// why the producer is an admin-pool System method that no-ops when no
	// live conversation exists). Failures here are logged but do not abort
	// the PATCH; the user's settings change has already been persisted, and
	// the worst case is the agent missing one delta on its next turn (which
	// a follow-up PATCH or the user's next message itself can correct).
	queuePendingContextChanges(r.Context(), s.curatorStore, orgID, *existing, updated)
	var fresh *projectJSON
	if err := s.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		row, e := tx.Projects.Get(r.Context(), orgID, id)
		if e != nil || row == nil {
			return e
		}
		rendered, e := projectToJSON(r.Context(), tx.Repos, orgID, *row, nil)
		if e != nil {
			return e
		}
		fresh = &rendered
		return nil
	}); err != nil {
		internalError(w, "projects", fmt.Errorf("read back project %s after update: %w", id, err))
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
	id, ok := projectIDOr404(w, r)
	if !ok {
		return
	}

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
	// delete: the goroutine releases the turn's claim, and the claim rows
	// are what the FK cascade is about to drop. Doing it in the right
	// order means a user who deletes a project mid-chat sees a
	// deterministic terminal state rather than relying on cascade
	// behavior to handle a live engagement. Queued turns are undelivered
	// messages the cascade simply deletes — the queued-cancel semantic.
	// No-op when the curator runtime hasn't been wired (test
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

	// The row is gone — any concurrent PATCH/upload racing in from here
	// will do its own existence check and 404 cleanly, so the lock has
	// nothing left to guard. Release now rather than holding it (a
	// Postgres connection, in multi mode) through the best-effort disk
	// cleanup and worktree prune below, which can be slow for a large
	// knowledge-base dir or a big pinned-repo list. release is
	// idempotent, so the deferred call above still fires safely as a
	// no-op on every return path.
	release()

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
	if runmode.Current() == runmode.ModeMulti {
		// Multi mode: the blob store is the KB source of truth, so clear its
		// prefix (best-effort, same warning contract). Then drop any local
		// materialization — present at role=all where control + executor are
		// co-located — and nudge the home executor to os.RemoveAll its own via
		// the project_deleted doorbell (best-effort, self-filtered to an idle
		// project so it never races a live turn's teardown).
		if delErr := s.kb.DeletePrefix(r.Context(), orgID, id); delErr != nil {
			projectsLog.Warn("cleanup of project kb store prefix failed", "project", id, "error", delErr)
			w.Header().Set("X-Cleanup-Warning", cleanupWarning)
		}
		if dir, dirErr := curator.KnowledgeDir(orgID, id); dirErr == nil {
			if rmErr := os.RemoveAll(dir); rmErr != nil && !os.IsNotExist(rmErr) {
				projectsLog.Warn("cleanup of local project dir failed", "project", id, "dir", dir, "error", rmErr)
			}
		}
		s.kbChanged("project_deleted", orgID, id)
	} else {
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

// pinnedRepoIDsField is the body field every pinned-repo fault is attributed
// to. Named once so the create and PATCH arms cannot disagree about it.
const pinnedRepoIDsField = "pinned_repository_ids"

// validatePinnedRepoShape checks that each pinned entry is a non-empty
// repository id and returns the normalized (trimmed) slice. Pure — does not
// touch the DB — so it stays cheap to test in isolation.
//
// The trim-then-resolve step matters: an id with surrounding whitespace would
// otherwise reach the lookup verbatim and miss, reporting "not a repository in
// this workspace" for a repository that is one.
//
// Deliberately no format check beyond non-empty. A registry id is an opaque
// handle this server minted, and the only question worth asking about one is
// whether a row answers to it — which validatePinnedRepos asks next. A
// shape rule here would be a second, weaker copy of that answer.
//
// Returns (normalized, "") on success and (nil, errMsg) on failure.
func validatePinnedRepoShape(repoIDs []string) ([]string, string) {
	out := make([]string, len(repoIDs))
	for i, r := range repoIDs {
		trimmed := strings.TrimSpace(r)
		if trimmed == "" {
			return nil, pinnedRepoIDsField + "[" + strconv.Itoa(i) + "] is empty"
		}
		out[i] = trimmed
	}
	return out, ""
}

// validatePinnedRepos resolves the wire's repository ids and composes that
// with the must-be-tracked check: every id must name a registry row, and that
// row must be a repo the project's *team* tracks (team_github_repos). Repos
// are per-team, so pinning is gated on the team's set rather than the org-wide
// union — a repo another team tracks (and that therefore has a row in the
// shared registry) is still rejected here if the project's own team doesn't
// track it. This pins the UX contract — the frontend presents the pin picker
// as a multi-select over the team's tracked repos, so an untracked id arriving
// here is a stale client or a hand-crafted curl. Rejecting it up front keeps
// the Curator from later trying to materialize a worktree for a repo the team
// can't authenticate against.
//
// It returns what the store persists: each pin as the provider's current name
// (see domain.Project.PinnedRepos). The translation lives here, at the edge,
// and nowhere else.
//
// Runs under the caller's claims (repositories + team_github_repos_select
// RLS), so it must be invoked inside a WithTx for the requesting user. Returns
// (names, "") on success and (nil, errMsg) on failure.
func validatePinnedRepos(ctx context.Context, repos db.RepositoryStore, teamRepos db.TeamGitHubReposStore, orgID, teamID string, repoIDs []string) ([]string, string) {
	ids, errMsg := validatePinnedRepoShape(repoIDs)
	if errMsg != "" {
		return nil, errMsg
	}
	if len(ids) == 0 {
		return []string{}, ""
	}

	tracked, err := teamRepos.ListForTeam(ctx, teamID)
	if err != nil {
		return nil, "failed to load tracked repos: " + err.Error()
	}
	// Key both sides case-insensitively: GitHub owner/repo names are
	// case-insensitive and the rest of the codebase folds case (TracksRepoSystem,
	// newlyAddedRepos, the registry's own identity index). A repo re-saved into
	// team_github_repos with different capitalization than the registry row's
	// is the same repo, so an exact-string match would wrongly reject an
	// otherwise-valid pin.
	known := make(map[string]struct{}, len(tracked))
	for _, t := range tracked {
		known[strings.ToLower(t.Slug())] = struct{}{}
	}

	out := make([]string, 0, len(ids))
	for _, id := range ids {
		row, err := repos.Get(ctx, orgID, id)
		if errors.Is(err, db.ErrNoSuchRepository) {
			// The id is the caller's to get right, so this is a rejection and
			// not a server fault. It reports the id back rather than a name,
			// because a name is not what was sent.
			return nil, pinnedRepoIDsField + ": " + id + " is not a repository in this workspace"
		}
		if err != nil {
			return nil, "failed to load repository " + id + ": " + err.Error()
		}
		if _, ok := known[strings.ToLower(row.Slug())]; !ok {
			return nil, pinnedRepoIDsField + ": " + row.Slug() + " is not a repo this team tracks (add it on the GitHub config page first)"
		}
		out = append(out, row.Slug())
	}
	return out, ""
}

// projectJSON is a project's wire shape: the domain row, with its pinned
// repositories rendered as registry row ids.
//
// The embedding is what keeps this from becoming a second copy of the domain
// struct that drifts from it — every other field is the domain's own and stays
// so. domain.Project.PinnedRepos carries `json:"-"` for exactly this reason:
// the one field whose wire form differs is replaced here rather than shadowed,
// so a project serialized anywhere else cannot quietly put names back on the
// wire.
type projectJSON struct {
	domain.Project
	// PinnedRepositoryIDs is the pinned set as registry row ids, in the
	// project's own pin order — the same ids /api/repos serves and the same
	// ids a PATCH sends back. The repository's name is a display property the
	// client reads off the repo, never a key it round-trips through here.
	PinnedRepositoryIDs []string `json:"pinned_repository_ids"`
}

// projectToJSON renders one project for the wire, resolving its stored pin
// names back to registry ids through repos.
//
// memo may be nil; a caller rendering a page of projects passes one so a repo
// pinned by several of them is looked up once.
func projectToJSON(ctx context.Context, repos db.RepositoryStore, orgID string, p domain.Project, memo map[string]string) (projectJSON, error) {
	ids := make([]string, 0, len(p.PinnedRepos))
	for _, slug := range p.PinnedRepos {
		owner, repo, ok := splitOwnerRepo(slug)
		if !ok {
			return projectJSON{}, fmt.Errorf("project %s pins a malformed repo name %q", p.ID, slug)
		}
		key := strings.ToLower(slug)
		if id, hit := memo[key]; hit {
			ids = append(ids, id)
			continue
		}
		row, err := repos.GetByRef(ctx, orgID, domain.RepoRef{Owner: owner, Repo: repo})
		if err != nil {
			return projectJSON{}, fmt.Errorf("resolve pinned repo %s of project %s: %w", slug, p.ID, err)
		}
		if row == nil {
			// Not reachable, and not silently survivable either. The store
			// builds these names by joining project_pinned_repos to
			// repositories, so a name with no row would mean the join and this
			// lookup disagreed inside one transaction. Dropping the pin would
			// hand the client a short list it would then PATCH back, unpinning
			// a repo nobody asked to unpin.
			return projectJSON{}, fmt.Errorf("project %s pins %s, which resolves to no repository row", p.ID, slug)
		}
		if memo != nil {
			memo[key] = row.ID
		}
		ids = append(ids, row.ID)
	}
	return projectJSON{Project: p, PinnedRepositoryIDs: ids}, nil
}

// projectsToJSON is projectToJSON over a page, sharing one memo across it.
func projectsToJSON(ctx context.Context, repos db.RepositoryStore, orgID string, ps []domain.Project) ([]projectJSON, error) {
	memo := make(map[string]string)
	out := make([]projectJSON, len(ps))
	for i, p := range ps {
		j, err := projectToJSON(ctx, repos, orgID, p, memo)
		if err != nil {
			return nil, err
		}
		out[i] = j
	}
	return out, nil
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
// validateTrackerKeys returns the normalized keys plus, on rejection, the
// body field at fault and its message — the field travels with the message so
// the envelope can attribute the error instead of leaving the client to parse
// a "jira_project_key: ..." prefix out of the prose.
func validateTrackerKeys(teamRules []domain.JiraProjectStatusRules, jiraKey, linearKey string) (jira, linear, errField, errMsg string) {
	jiraNorm := normalizeJiraProjectKey(jiraKey)
	linearNorm := strings.TrimSpace(linearKey)

	if linearNorm != "" {
		// Linear integration is future work. Surfacing this as an
		// explicit "not configured" error matches the UX contract:
		// the frontend renders a disabled Linear picker, so a
		// non-empty value arriving here is a bypass attempt or a
		// stale client.
		return "", "", "linear_project_key", "Linear integration is not configured"
	}

	if jiraNorm == "" {
		return "", "", "", ""
	}
	for _, p := range teamRules {
		if p.ProjectKey == jiraNorm {
			return jiraNorm, "", "", ""
		}
	}
	return "", "", "jira_project_key", jiraNorm + " is not in the configured Jira projects list (add it on the Settings page first)"
}

// queuePendingContextChanges diffs the project's pre-PATCH state against the
// freshly-merged value and records an 'injection:context' delta on every
// live curator conversation of the project for each field that meaningfully
// changed. The store's producer no-ops when no live conversation exists —
// there's no point queueing for a project nobody has chatted with: the next
// conversation's static envelope renders fresh values directly.
//
// One-pending-per-type: the producer replaces an existing undelivered row of
// the same change_type per conversation, so a run of PATCHes between user
// messages leaves one delta per field.
//
// Failures are logged, not returned. The PATCH itself has already
// committed (queueing is best-effort context decoration), and the
// agent will at worst miss a delta on its next turn — recoverable
// from the user's next message or a follow-up PATCH. Surfacing the
// failure as a 500 would imply the settings change failed, which is
// strictly worse.
func queuePendingContextChanges(ctx context.Context, curatorStore db.CuratorStore, orgID string, before, after domain.Project) {
	if !pinnedReposSetEqual(before.PinnedRepos, after.PinnedRepos) {
		baseline, err := curator.EncodeStringSliceBaseline(before.PinnedRepos)
		if err != nil {
			projectsLog.Warn("encode pinned-repos baseline failed", "project", before.ID, "error", err)
		} else if err := curatorStore.QueueContextChangeSystem(ctx, orgID, before.ID, domain.ChangeTypePinnedRepos, baseline); err != nil {
			projectsLog.Warn("queue pinned-repos pending context failed", "project", before.ID, "error", err)
		}
	}
	if before.JiraProjectKey != after.JiraProjectKey {
		baseline, err := curator.EncodeNullableStringBaseline(before.JiraProjectKey)
		if err != nil {
			projectsLog.Warn("encode jira baseline failed", "project", before.ID, "error", err)
		} else if err := curatorStore.QueueContextChangeSystem(ctx, orgID, before.ID, domain.ChangeTypeJiraProjectKey, baseline); err != nil {
			projectsLog.Warn("queue jira pending context failed", "project", before.ID, "error", err)
		}
	}
	if before.LinearProjectKey != after.LinearProjectKey {
		baseline, err := curator.EncodeNullableStringBaseline(before.LinearProjectKey)
		if err != nil {
			projectsLog.Warn("encode linear baseline failed", "project", before.ID, "error", err)
		} else if err := curatorStore.QueueContextChangeSystem(ctx, orgID, before.ID, domain.ChangeTypeLinearProjectKey, baseline); err != nil {
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
// knowledgeListRequest is the body of POST /api/projects/{id}/knowledge/list.
// The project is the path id and the knowledge base has no filters, so the
// body is paging alone.
type knowledgeListRequest struct {
	httpx.PageRequest
}

// POST /api/projects/{id}/knowledge/list
//
// The rows keep the 256KB inline-content rule per file (knowledgeInlineMaxBytes):
// a text-shaped file under the cap carries its body, anything larger carries
// metadata and is fetched by path. Paging does not change that — the cap is
// about one file, the page is about how many files.
//
// The window is applied in Go rather than at the source. Both backends
// enumerate a flat directory (or its blob-store equivalent) as a unit and
// neither offers a windowed listing, so a page here is a slice of an
// already-complete answer — which is also why total_count is exact.
func (s *Server) handleProjectKnowledge(w http.ResponseWriter, r *http.Request) {
	orgID, ok := s.requireOrg(w, r)
	if !ok {
		return
	}
	userID := ClaimsFrom(r.Context()).Subject
	id, ok := projectIDOr404(w, r)
	if !ok {
		return
	}

	var req knowledgeListRequest
	if !httpx.DecodeJSONStrict(w, r, &req) {
		return
	}
	var v httpx.Validation
	page := httpx.ResolvePage(&v, req.PageRequest, httpx.FilterFingerprint(struct{}{}), 0)
	if v.Flush(w, http.StatusBadRequest) {
		return
	}

	var project *domain.Project
	if err := s.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		var e error
		project, e = tx.Projects.Get(r.Context(), orgID, id)
		return e
	}); err != nil {
		internalError(w, "projects", fmt.Errorf("load project %s: %w", id, err))
		return
	}
	if project == nil {
		notFound(w, "project")
		return
	}

	var files []knowledgeFile
	// Multi mode: the blob store is the KB source of truth (control's own disk
	// hosts no KB). Local mode keeps the byte-identical on-disk read below.
	if runmode.Current() == runmode.ModeMulti {
		var err error
		files, err = s.listKnowledgeFromStore(r.Context(), orgID, id)
		if err != nil {
			internalError(w, "projects", fmt.Errorf("list knowledge from store for project %s: %w", id, err))
			return
		}
	} else {
		root, err := curator.KnowledgeDir(orgID, id)
		if err != nil {
			internalError(w, "projects", fmt.Errorf("resolve knowledge dir for project %s: %w", id, err))
			return
		}
		kbDir := filepath.Join(root, "knowledge-base")
		files, err = readKnowledgeFiles(kbDir)
		if err != nil {
			internalError(w, "projects", fmt.Errorf("read knowledge dir %s: %w", kbDir, err))
			return
		}
	}
	httpx.WriteList(w, page, windowSlice(files, page), len(files))
}

// windowSlice takes the page's slice of an already-materialized list. An
// offset past the end yields an empty page rather than a panic, which is the
// same answer a store gives for a window past its result set.
func windowSlice[T any](all []T, page httpx.Page) []T {
	if page.Offset >= len(all) {
		return nil
	}
	end := min(page.Offset+page.Limit, len(all))
	return all[page.Offset:end]
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
//  1. sanitizeKnowledgeFilename, which rejects "..", path separators,
//     and leading dots.
//  2. Resolve to absolute via filepath.Join(kbDir, name) and re-check
//     the result has kbDir as its prefix. Belt-and-suspenders against
//     anything the sanitizer might miss on a future filesystem.
//
// rawPath is r.PathValue("path"), which the mux has ALREADY
// percent-decoded — so it is the filename, verbatim, and decoding it
// again here would be a second decode the client never encoded for.
// That mismatch put the listing and the per-file routes at odds: a
// file listed as "notes%20final.md", correctly requested as
// "notes%2520final.md", second-decoded into a different name and
// 404'd, and "100%.md" failed the second decode outright as a 400.
// The listing's names are the names; a client encodes each exactly
// once.
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
	name, errMsg := sanitizeKnowledgeFilename(rawPath)
	if errMsg != "" {
		return "", "", http.StatusBadRequest, errMsg
	}
	// The sanitizer strips path components (its upload callers need that for
	// browser multipart filenames), but here the delivered value must already
	// BE the name: the knowledge base is flat and the listing never emits a
	// name with a separator in it. Refuse anything the sanitizer had to
	// rewrite — an encoded separator ("a%2Fb.md", which the mux hands over as
	// "a/b.md") would otherwise quietly serve a file the request never named.
	if name != rawPath {
		return "", "", http.StatusBadRequest, "filename cannot contain path separators"
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
	id, ok := projectIDOr404(w, r)
	if !ok {
		return
	}
	var project *domain.Project
	if err := s.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		var e error
		project, e = tx.Projects.Get(r.Context(), orgID, id)
		return e
	}); err != nil {
		internalError(w, "projects", fmt.Errorf("load project %s: %w", id, err))
		return
	}
	if project == nil {
		notFound(w, "project")
		return
	}
	_, full, status, errMsg := resolveKnowledgePath(orgID, id, r.PathValue("path"))
	if errMsg != "" {
		writeKnowledgePathError(w, status, errMsg)
		return
	}
	// Multi mode: stream from the blob store with single-range support (video
	// scrubbing). resolveKnowledgePath already validated the name; the sanitized
	// base is the flat KB filename. Local mode keeps the on-disk serve below.
	if runmode.Current() == runmode.ModeMulti {
		s.serveKnowledgeFromStore(w, r, orgID, id, filepath.Base(full))
		return
	}
	linfo, err := os.Lstat(full)
	if err != nil {
		if os.IsNotExist(err) {
			notFound(w, "file")
			return
		}
		internalError(w, "projects", fmt.Errorf("lstat knowledge file %s: %w", full, err))
		return
	}
	if linfo.Mode()&os.ModeSymlink != 0 || !linfo.Mode().IsRegular() {
		writeNotARegularFile(w)
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
		if isSymlinkRejection(err) {
			projectsLog.Error("knowledge fetch: open failed", "path", full, "error", err)
			writeNotARegularFile(w)
			return
		}
		internalError(w, "projects", fmt.Errorf("open knowledge file %s: %w", full, err))
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
	id, ok := projectIDOr404(w, r)
	if !ok {
		return
	}

	// Parse the multipart body BEFORE taking the per-project lock.
	// ParseMultipartForm reads the whole request off the wire — the
	// slow, client-bandwidth-bound part of this handler — and doesn't
	// touch the project row or its on-disk directory, so it doesn't
	// need the lock. Acquiring the lock only after this point means a
	// slow uploader doesn't hold a Postgres connection (and, in multi
	// mode, the advisory lock's session) idle for the duration of the
	// network transfer; see the lock acquisition below for what it
	// does need to cover. MaxBytesReader caps the WHOLE request,
	// including form fields and per-file streams; per-file caps are
	// enforced separately during the copy below so a single oversize
	// file doesn't poison siblings that were within budget.
	r.Body = http.MaxBytesReader(w, r.Body, knowledgeMaxRequestBytes)
	if err := r.ParseMultipartForm(8 << 20); err != nil {
		httpx.WriteErrors(w, http.StatusBadRequest, httpx.ErrorItem{
			Reason: httpx.ReasonInvalidBody, Message: "multipart parse: " + err.Error(),
		})
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
		httpx.WriteErrors(w, http.StatusBadRequest, httpx.ErrorItem{
			Reason: httpx.ReasonMissingField, Message: "no files in upload (use form field 'file')", Field: "file",
		})
		return
	}

	// Take the same per-project lock that PATCH and DELETE use.
	// Without it, an upload that has just verified the project
	// exists could race a DELETE: DELETE drops the row and
	// RemoveAll's the project dir, then this handler re-creates
	// knowledge-base/ and writes files into a project that no
	// longer exists in the DB — leaving orphaned on-disk state
	// after a successful delete. Holding the lock across the
	// existence check + write makes the upload visibly atomic
	// to a concurrent delete. Acquired here, after the body is
	// already fully read above, so the held connection only spans
	// the DB check + local disk writes below, not the upload itself.
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
		internalError(w, "projects", fmt.Errorf("load project %s: %w", id, err))
		return
	}
	if project == nil {
		notFound(w, "project")
		return
	}
	// Multi mode targets the blob store (control hosts no KB on disk); local
	// mode writes the on-disk knowledge-base dir. Only the local path needs the
	// dir materialized up front.
	multi := runmode.Current() == runmode.ModeMulti
	var kbDir string
	if !multi {
		root, err := curator.KnowledgeDir(orgID, id)
		if err != nil {
			internalError(w, "projects", fmt.Errorf("resolve knowledge dir for project %s: %w", id, err))
			return
		}
		kbDir = filepath.Join(root, "knowledge-base")
		if err := os.MkdirAll(kbDir, 0o755); err != nil {
			internalError(w, "projects", fmt.Errorf("create knowledge dir %s: %w", kbDir, err))
			return
		}
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
		if multi {
			// Preserve reject-on-conflict: the per-project lock this handler
			// holds serializes the Exists+Put window within a pod. A cross-
			// control-pod race on the same name degrades to last-writer-wins
			// (accepted — the lock is in-process only).
			exists, err := s.kb.Exists(r.Context(), orgID, id, name)
			if err != nil {
				results = append(results, uploadResult{Original: original, Error: "check existing: " + err.Error()})
				continue
			}
			if exists {
				results = append(results, uploadResult{Original: original, Error: "file already exists — delete it first"})
				continue
			}
			if errMsg := s.putUploadedKB(r.Context(), orgID, id, name, fh); errMsg != "" {
				results = append(results, uploadResult{Original: original, Error: errMsg})
				continue
			}
		} else {
			full := filepath.Join(kbDir, name)
			if errMsg := writeUploadedFile(fh, full); errMsg != "" {
				results = append(results, uploadResult{Original: original, Error: errMsg})
				continue
			}
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
		// Multi mode: tell browsers the panel changed and nudge the home
		// executor to materialize the upload into a live session's dir.
		if multi {
			s.kbChanged("", orgID, id)
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
	id, ok := projectIDOr404(w, r)
	if !ok {
		return
	}

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
		internalError(w, "projects", fmt.Errorf("load project %s: %w", id, err))
		return
	}
	if project == nil {
		notFound(w, "project")
		return
	}
	_, full, status, errMsg := resolveKnowledgePath(orgID, id, r.PathValue("path"))
	if errMsg != "" {
		writeKnowledgePathError(w, status, errMsg)
		return
	}
	multi := runmode.Current() == runmode.ModeMulti
	if multi {
		// Store DELETE is idempotent, so check Exists first to preserve the
		// on-disk path's 404-on-missing behavior. resolveKnowledgePath already
		// validated the name; its base is the flat KB filename.
		name := filepath.Base(full)
		exists, err := s.kb.Exists(r.Context(), orgID, id, name)
		if err != nil {
			internalError(w, "projects", fmt.Errorf("check knowledge file %s in store: %w", name, err))
			return
		}
		if !exists {
			notFound(w, "file")
			return
		}
		if err := s.kb.Delete(r.Context(), orgID, id, name); err != nil {
			internalError(w, "projects", fmt.Errorf("delete knowledge file %s from store: %w", name, err))
			return
		}
	} else if err := os.Remove(full); err != nil {
		if os.IsNotExist(err) {
			notFound(w, "file")
			return
		}
		internalError(w, "projects", fmt.Errorf("remove knowledge file %s: %w", full, err))
		return
	}
	if err := s.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		return tx.Projects.BumpUpdatedAt(r.Context(), orgID, id)
	}); err != nil {
		internalError(w, "projects", fmt.Errorf("bump updated_at for project %s: %w", id, err))
		return
	}
	if multi {
		s.kbChanged("", orgID, id)
	}
	w.WriteHeader(http.StatusNoContent)
}
