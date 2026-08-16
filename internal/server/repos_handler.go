package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/auth"
	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	ghclient "github.com/sky-ai-eng/triage-factory/internal/github"
	"github.com/sky-ai-eng/triage-factory/internal/integrations"
	"github.com/sky-ai-eng/triage-factory/internal/reachcache"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
	"github.com/sky-ai-eng/triage-factory/internal/server/httpx"
)

// errRepoGone marks the window between a repository being visible to the
// caller's access gate and its registry row being read a statement later. It
// never leaves this package — the handler maps it to the same 404 an
// out-of-tracked-set repository gets.
var errRepoGone = errors.New("repository row no longer resolves")

// --------------------------------------------------------------------
// POST /api/github/repos/list — the repository picker's option list, and
// POST /api/github/repos/refresh — its explicit refresh.
//
// The picker used to proxy GitHub live on every read, from a different endpoint
// per credential class, which is why it was the one list on this surface that
// could not report a total. It now reads reachable_repositories — one mirror
// both tiers populate — so it is an ordinary store-backed list: a real
// total_count, a server-side filter, offset paging, and a schema identical
// whether the org authenticates with a PAT or its own App.
//
// Nothing here calls GitHub. A stale or empty mirror kicks an out-of-band
// refresh (internal/reachcache) and answers from what is on hand; on a large PAT
// account the enumeration this replaces is ~30 upstream pages, which is a
// multi-second request nobody should be waiting through.
// --------------------------------------------------------------------

// githubRepoListRequest is the body of POST /api/github/repos/list: the
// server-side search the picker used to do in the browser, plus the paging pair.
type githubRepoListRequest struct {
	// Q is a case-insensitive substring matched against the slug, the
	// description and the language. Empty matches everything.
	Q string `json:"q"`
	httpx.PageRequest
}

// githubRepoListFilterKey is the canonicalized filter set the page token is
// fingerprinted against. Canonicalized to lowercase-and-trimmed because that is
// what the store matches on, so two spellings of one query are one query and a
// token minted under either addresses the same result set.
type githubRepoListFilterKey struct {
	Q string `json:"q"`
}

// Readiness discriminator for the picker list. A first-ever open has nothing to
// serve, and an empty list there reads as "you have no repositories" rather than
// "we have not looked yet" — so the two states are told apart by a field rather
// than by an empty array. Modelled on the Jira stock deck's readiness gate: one
// schema in both states, every key present, the client rendering off `status`.
const (
	// githubRepoStatusReady means the rows below are the mirror's answer. It is
	// the answer even when the mirror is stale — the refresh this read kicked
	// lands out of band — and even when the answer is legitimately empty (an App
	// installed on no repositories).
	githubRepoStatusReady = "ready"
	// githubRepoStatusDiscovering means nothing has been mirrored for this org
	// yet and a refresh has been asked for. items is empty and total_count is 0
	// because those are the true counts of what is known, not a claim about what
	// exists.
	githubRepoStatusDiscovering = "discovering"
)

// githubRepoJSON is one picker option. The field set is the picker's own — the
// columns the mirror carries for it — and deliberately not "whatever GitHub's
// repository object has", which is what a proxy list leaks by default.
type githubRepoJSON struct {
	FullName    string `json:"full_name"`
	HTMLURL     string `json:"html_url"`
	Description string `json:"description"`
	Language    string `json:"language"`
	PushedAt    string `json:"pushed_at"`
	Private     bool   `json:"private"`
}

// githubRepoListResponse is the list envelope plus the readiness discriminator.
//
// next_page_token carries no omitempty, unlike httpx's shared envelope: this
// route promises the same keys in both readiness states, and a discovering
// response has no next page by construction. "" and absent mean the same thing
// to every client (the API client normalizes a missing token to ""), so the
// only difference is that a reader can tell the two states apart field-for-field.
type githubRepoListResponse struct {
	Items         []githubRepoJSON `json:"items"`
	NextPageToken string           `json:"next_page_token"`
	// TotalCount is what the filter matches across every page — a real number
	// on both credential classes, which is the whole point of sourcing this
	// locally. It is never null here; a proxy list's "this resource cannot count
	// itself" no longer applies.
	TotalCount int    `json:"total_count"`
	Status     string `json:"status"`
}

func (s *Server) handleGitHubRepos(w http.ResponseWriter, r *http.Request) {
	orgID := OrgIDFrom(r.Context())
	userID := ClaimsFrom(r.Context()).Subject

	var req githubRepoListRequest
	if !httpx.DecodeJSONStrict(w, r, &req) {
		return
	}
	q := strings.ToLower(strings.TrimSpace(req.Q))
	var v httpx.Validation
	page := httpx.ResolvePage(&v, req.PageRequest, httpx.FilterFingerprint(githubRepoListFilterKey{Q: q}), 0)
	if v.Flush(w, http.StatusBadRequest) {
		return
	}

	// The configuration preflight runs BEFORE the mirror read, and it is what
	// keeps an unconfigured org out of the readiness state: "we have not looked
	// yet" is a promise that looking will eventually produce something, and for
	// an org with no usable credential it never will. It costs no GitHub call —
	// every read here is the store or the keychain.
	class, ok := s.pickerCredentialClass(w, r, orgID, userID)
	if !ok {
		return
	}

	state, err := s.reachableRepos.ReachableStateSystem(r.Context(), orgID, class)
	if err != nil {
		reposLog.Error("read reachable repo cache state failed", "org", orgID, "error", err)
		internalError(w, "repos", err)
		return
	}
	// Serve stale, kick the refresh. The trigger is idempotent inside the
	// refresher's own TTL, so N opens across one window cost one enumeration; a
	// fresh mirror asks for nothing at all, which is what makes the steady-state
	// read reach GitHub zero times.
	if state.StaleAt(time.Now(), reachcache.TTL) {
		s.kickReachRefresh(orgID, false)
	}
	// Known, not "has rows": a scope that genuinely reaches nothing writes zero
	// entries, so keying the readiness state on the count would leave an org with
	// an empty grant saying "discovering repositories…" forever — and kicking a
	// refresh on every open, which is the unbounded enumeration the TTL exists to
	// prevent.
	if !state.Known() {
		writeJSON(w, http.StatusOK, githubRepoListResponse{
			Items:  []githubRepoJSON{},
			Status: githubRepoStatusDiscovering,
		})
		return
	}

	rows, total, err := s.reachableRepos.ListReachableSystem(r.Context(), orgID, class, q,
		db.ListOpts{Limit: page.Limit, Offset: page.Offset})
	if err != nil {
		reposLog.Error("list reachable repos failed", "org", orgID, "error", err)
		internalError(w, "repos", err)
		return
	}
	items := make([]githubRepoJSON, 0, len(rows))
	for _, row := range rows {
		items = append(items, githubRepoJSON{
			FullName:    row.Slug(),
			HTMLURL:     row.HTMLURL,
			Description: row.Description,
			Language:    row.Language,
			PushedAt:    row.PushedAt,
			Private:     row.Private,
		})
	}
	writeJSON(w, http.StatusOK, githubRepoListResponse{
		Items:         items,
		NextPageToken: httpx.NextPageToken(page, len(items), total),
		TotalCount:    total,
		Status:        githubRepoStatusReady,
	})
}

// handleGitHubReposRefresh forces a reachable-mirror refresh past the TTL.
//
// A verb route rather than a field write, because it is process control over a
// background pass plus the GitHub calls that pass makes — there is no row a
// caller could set to mean this. It answers 202 the moment the refresh is asked
// for: the pass runs out of band, and the client learns it landed from the
// `github_repos_updated` websocket ping or its next read.
//
// It is the affordance the TTL requires. A repository granted since the last
// refresh is invisible to the picker and rejected by the write gate until the
// mirror moves, and waiting out six hours is not a fix; every credential change
// forces the same refresh for the same reason. Gated to the same audience as the
// list read — reachability is an org-level fact about the org's own credential —
// and bounded by the refresher's per-org single flight, so a caller that clicks
// it repeatedly gets one enumeration, not one per click.
//
// POST /api/github/repos/refresh
func (s *Server) handleGitHubReposRefresh(w http.ResponseWriter, r *http.Request) {
	// No body is decoded: the refresh takes no arguments, exactly like the
	// sibling verb routes (archive, restore, revoke), so there is no payload
	// schema for a caller to get wrong.
	orgID := OrgIDFrom(r.Context())
	if s.reachTrigger == nil {
		// No refresh manager wired: this process cannot ask for one and must not
		// report that it did. 503 rather than 500 — it is a capability this
		// deployment does not have here, not a fault in the request.
		httpx.WriteErrors(w, http.StatusServiceUnavailable, httpx.ErrorItem{
			Reason:  httpx.ReasonNotConfigured,
			Message: "repository refresh is not configured on this server",
		})
		return
	}
	s.kickReachRefresh(orgID, true)
	writeJSON(w, http.StatusAccepted, map[string]any{"status": "refreshing"})
}

// pickerCredentialClass resolves the class the org's reachable entries are keyed
// under, and refuses the request when GitHub is not usable for the org at all.
// It writes the response on every false return.
//
// The two refusals are deliberately different, and the difference is the
// first-run dead-end this handler exists to keep diagnosable: an org whose App
// is registered and active but installed nowhere is told to install it, while
// everything else gets the generic "not connected". Reporting the first as the
// second reads as "add a PAT", which is the one thing that will not help.
func (s *Server) pickerCredentialClass(w http.ResponseWriter, r *http.Request, orgID, userID string) (domain.GitHubCredentialClass, bool) {
	// The EFFECTIVE class — the org's stored class narrowed by the App-XOR-PAT
	// gate — because that is what the refresh keys its rows under. An
	// unreadable or unrecognised class fails the request: the picker's whole job
	// is to show what the org can reach, and showing the wrong credential
	// system's idea of that is worse than showing an error.
	class, err := s.reachableCredentialClass(r.Context(), orgID)
	if err != nil {
		reposLog.Error("resolve reachable credential class failed", "org", orgID, "error", err)
		internalError(w, "repos", err)
		return "", false
	}
	if class == domain.GitHubCredentialClassBYOApp {
		// An active App with at least one installation is usable by definition —
		// the installations ARE the reach. A read failure here is not a reason to
		// refuse: it would be reported as "not installed", which is a claim only a
		// successful read of zero installations can support.
		insts, err := s.githubApps.ListInstallationsForOrgSystem(r.Context(), orgID)
		if err != nil {
			reposLog.Warn("list installations failed; serving the mirror as-is", "org", orgID, "error", err)
			return class, true
		}
		if len(insts) == 0 {
			reposLog.Warn("app registered and active but installed on zero accounts", "org", orgID)
			writeNotConfigured(w, "the GitHub App is not installed on any account")
			return "", false
		}
		return class, true
	}

	// PAT tier. Credentials + per-org base URL read through the app pool inside
	// WithTx so SecretStore decrypts under the user's claims and
	// org_settings_select RLS is enforced — the same shape the rest of the
	// settings surface uses.
	var (
		creds  auth.Credentials
		orgSet domain.OrgSettings
	)
	if err := s.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		// A failed credential read is a backend fault, not "no credentials":
		// swallowing it here answered "GitHub not configured" and told the user to
		// re-enter a token they already have.
		var lerr error
		creds, lerr = integrations.Load(r.Context(), tx.Secrets, orgID)
		if lerr != nil {
			return fmt.Errorf("load integration credentials: %w", lerr)
		}
		orgSet, lerr = tx.Orgs.GetSettings(r.Context(), orgID)
		return lerr
	}); err != nil {
		internalError(w, "repos", err)
		return "", false
	}
	if creds.GitHubPAT == "" || (orgSet.GitHubBaseURL == "" && creds.GitHubURL == "") {
		reposLog.Warn("github not configured, no usable app installation and no pat", "org", orgID)
		writeNotConfigured(w, "GitHub is not connected for this workspace")
		return "", false
	}
	return class, true
}

// isOrgAdmin reports whether userID is an org admin of orgID, short-circuiting
// to true under local mode (N=1 has a single implicit owner and no team
// boundary — mirrors the org-admin gates elsewhere in this package).
func (s *Server) isOrgAdmin(ctx context.Context, orgID, userID string) (bool, error) {
	if runmode.Current() == runmode.ModeLocal {
		return true, nil
	}
	return s.az.UserIsOrgAdmin(ctx, userID, orgID)
}

// repoAccessAllowed reports whether the caller may *read* the given repo
// (TFAC-559): an org admin always may; a non-admin member may only when the
// repo is tracked by at least one of their teams. Local mode (N=1) has no
// team boundary and is covered by isOrgAdmin's short-circuit.
//
// Mutations are a strictly narrower gate — see repoMutationAccess.
func (s *Server) repoAccessAllowed(ctx context.Context, orgID, userID, owner, repo string) (bool, error) {
	isAdmin, err := s.isOrgAdmin(ctx, orgID, userID)
	if err != nil {
		return false, err
	}
	if isAdmin {
		return true, nil
	}
	var tracked bool
	err = s.tx.WithTx(ctx, orgID, userID, func(tx db.TxStores) error {
		var e error
		tracked, e = tx.TeamGitHubRepos.TracksRepoViewerScoped(ctx, orgID, owner, repo)
		return e
	})
	return tracked, err
}

// repoWriteAccess is the outcome of the repo-mutation gate. Three-valued
// rather than a bool because "may not" splits into two different answers to
// the caller: a repo they can't see at all versus one they can see but may
// not change.
type repoWriteAccess int

const (
	// repoWriteInvisible — the repo is outside the caller's tracked set.
	// It isn't in their GET /api/repos list either, so the handler answers
	// 404 and discloses nothing about whether it exists. Deliberately the
	// zero value, so a repoWriteAccess that was never assigned denies
	// rather than permits.
	repoWriteInvisible repoWriteAccess = iota
	// repoWriteForbidden — the caller can see the repo but administers
	// none of the teams tracking it. 404 here would contradict their own
	// repo list, so this is an honest 403.
	repoWriteForbidden
	// repoWriteAllowed — org admin, or team admin of a tracking team.
	repoWriteAllowed
)

// repoMutationAccess resolves the write gate for a repo's org-wide
// configuration. repositories carries no team_id, so a write by any member
// of any tracking team lands on every tracking team's runs — membership is
// therefore the read gate only, and mutating requires an admin: org admin,
// or team admin of at least one team that tracks the repo.
//
// There is no DB backstop for this. repositories_all's RLS is org-wide by
// construction (it cannot express team scoping for an entity that has no
// team), so this predicate is the whole enforcement — keep it here, in front
// of every mutating repo handler.
//
// Local mode (N=1) resolves to repoWriteAllowed via isOrgAdmin's
// short-circuit, before any store call.
func (s *Server) repoMutationAccess(ctx context.Context, orgID, userID, owner, repo string) (repoWriteAccess, error) {
	isAdmin, err := s.isOrgAdmin(ctx, orgID, userID)
	if err != nil {
		return repoWriteInvisible, err
	}
	if isAdmin {
		return repoWriteAllowed, nil
	}

	var teamAdmin, tracked bool
	if err := s.tx.WithTx(ctx, orgID, userID, func(tx db.TxStores) error {
		var e error
		teamAdmin, e = tx.TeamGitHubRepos.TracksRepoViewerAdminScoped(ctx, orgID, owner, repo)
		if e != nil || teamAdmin {
			// A team admin of a tracking team is necessarily a member of
			// one, so the visibility read below would only confirm what we
			// already know.
			return e
		}
		tracked, e = tx.TeamGitHubRepos.TracksRepoViewerScoped(ctx, orgID, owner, repo)
		return e
	}); err != nil {
		return repoWriteInvisible, err
	}

	switch {
	case teamAdmin:
		return repoWriteAllowed, nil
	case tracked:
		return repoWriteForbidden, nil
	default:
		return repoWriteInvisible, nil
	}
}

// repoJSON is the registry's row shape — the list rows and the single read
// alike. CanEdit mirrors the PATCH gate (repoMutationAccess) per row, so the
// page can render a read-only base branch instead of a control that 403s on
// use. The client must not re-derive it from role: org admin and team admin
// are orthogonal, and only the server knows which teams tracking a given repo
// the caller administers.
type repoJSON struct {
	ID             string  `json:"id"`
	Owner          string  `json:"owner"`
	Repo           string  `json:"repo"`
	Description    string  `json:"description,omitempty"`
	HasReadme      bool    `json:"has_readme"`
	HasClaudeMd    bool    `json:"has_claude_md"`
	HasAgentsMd    bool    `json:"has_agents_md"`
	ProfileText    string  `json:"profile_text,omitempty"`
	DefaultBranch  string  `json:"default_branch,omitempty"`
	BaseBranch     string  `json:"base_branch,omitempty"`
	ProfiledAt     *string `json:"profiled_at,omitempty"`
	CloneStatus    string  `json:"clone_status,omitempty"`
	CloneError     string  `json:"clone_error,omitempty"`
	CloneErrorKind string  `json:"clone_error_kind,omitempty"`
	CanEdit        bool    `json:"can_edit"`
}

func repoToJSON(row domain.Repository, canEdit bool) repoJSON {
	out := repoJSON{
		// id stays the "owner/repo" slug on the wire: it is what the frontend
		// keys rows on, what it puts back in the PATCH and branches paths, and
		// what a person reads. row.ID is the registry handle (TFAC-834) and is
		// deliberately NOT serialized — sending it here would compile, pass
		// every test that doesn't assert on the field, and hand the frontend a
		// uuid where it round-trips a slug.
		ID:             row.Slug(),
		Owner:          row.Owner,
		Repo:           row.Repo,
		Description:    row.Description,
		HasReadme:      row.HasReadme,
		HasClaudeMd:    row.HasClaudeMd,
		HasAgentsMd:    row.HasAgentsMd,
		ProfileText:    row.ProfileText,
		DefaultBranch:  row.DefaultBranch,
		BaseBranch:     row.BaseBranch,
		CloneStatus:    row.CloneStatus,
		CloneError:     row.CloneError,
		CloneErrorKind: row.CloneErrorKind,
		CanEdit:        canEdit,
	}
	if row.ProfiledAt != nil {
		t := row.ProfiledAt.UTC().Format(time.RFC3339)
		out.ProfiledAt = &t
	}
	return out
}

// repoListRequest is the body of POST /api/repos/list. The registry read has
// no filters of its own — which rows a caller sees is decided by their role,
// not by the body.
type repoListRequest struct {
	httpx.PageRequest
}

// handleRepositories returns the configured repository rows from the DB. Org
// admins get the org-wide union; non-admin members get only the repos
// tracked by their own teams (TFAC-559) — the table is org-wide, so an
// unscoped read would leak every team's repos to a teammate on none of them.
// Local mode (N=1) has no team boundary and stays unscoped.
//
// POST /api/repos/list
func (s *Server) handleRepositories(w http.ResponseWriter, r *http.Request) {
	orgID, ok := s.requireOrg(w, r)
	if !ok {
		return
	}
	userID := ClaimsFrom(r.Context()).Subject

	var req repoListRequest
	if !httpx.DecodeJSONStrict(w, r, &req) {
		return
	}
	var v httpx.Validation
	page := httpx.ResolvePage(&v, req.PageRequest, httpx.FilterFingerprint(struct{}{}), 0)
	if v.Flush(w, http.StatusBadRequest) {
		return
	}

	isAdmin, err := s.isOrgAdmin(r.Context(), orgID, userID)
	if err != nil {
		internalError(w, "repos", err)
		return
	}

	var (
		repos   []domain.Repository
		total   int
		canEdit []bool
	)
	if err := s.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		var e error
		opts := db.ListOpts{Limit: page.Limit, Offset: page.Offset}
		if isAdmin {
			repos, total, e = tx.Repos.List(r.Context(), orgID, opts)
		} else {
			repos, total, e = tx.Repos.ListTeamScoped(r.Context(), orgID, opts)
		}
		if e != nil {
			return e
		}
		canEdit = make([]bool, len(repos))
		if isAdmin {
			for i := range canEdit {
				canEdit[i] = true
			}
			return nil
		}
		// One EXISTS per row of THIS page — bounded by the page size now
		// rather than by the caller's whole tracked set, and still inside the
		// single transaction the list read used.
		for i, row := range repos {
			canEdit[i], e = tx.TeamGitHubRepos.TracksRepoViewerAdminScoped(r.Context(), orgID, row.Owner, row.Repo)
			if e != nil {
				return e
			}
		}
		return nil
	}); err != nil {
		internalError(w, "repos", err)
		return
	}

	result := make([]repoJSON, len(repos))
	for i, row := range repos {
		result[i] = repoToJSON(row, canEdit[i])
	}
	httpx.WriteList(w, page, result, total)
}

// handleRepoGet is the canonical single read for a registry row, in the list's
// row shape. Gated exactly like the list: a repo outside the caller's tracked
// set is 404 (it isn't in their list either, so disclose nothing), and the
// can_edit annotation is resolved the same way the list resolves it.
//
// GET /api/repos/{owner}/{repo}
func (s *Server) handleRepoGet(w http.ResponseWriter, r *http.Request) {
	orgID, ok := s.requireOrg(w, r)
	if !ok {
		return
	}
	userID := ClaimsFrom(r.Context()).Subject
	owner, repo, ok := repoSlugPath(w, r)
	if !ok {
		return
	}

	if allowed, err := s.repoAccessAllowed(r.Context(), orgID, userID, owner, repo); err != nil {
		internalError(w, "repos", err)
		return
	} else if !allowed {
		notFound(w, "repo")
		return
	}

	var (
		row     *domain.Repository
		canEdit bool
	)
	if err := s.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		var e error
		// Ref-keyed, not id-keyed: the path segments are the provider's
		// current NAME for the repository, and the registry id is a separate
		// handle (TFAC-834). Get would take that handle and answer
		// ErrNoSuchRepository for a slug — a 500 on a repo that exists.
		// GetByRef's (nil, nil) miss is the answer this route wants anyway,
		// since a name that resolves to nothing is a 404 here.
		row, e = tx.Repos.GetByRef(r.Context(), orgID, domain.RepoRef{Owner: owner, Repo: repo})
		if e != nil || row == nil {
			return e
		}
		isAdmin, e := s.isOrgAdmin(r.Context(), orgID, userID)
		if e != nil {
			return e
		}
		if isAdmin {
			canEdit = true
			return nil
		}
		canEdit, e = tx.TeamGitHubRepos.TracksRepoViewerAdminScoped(r.Context(), orgID, row.Owner, row.Repo)
		return e
	}); err != nil {
		internalError(w, "repos", err)
		return
	}
	if row == nil {
		// Reachable by the tracked set (or org-admin) but absent from the
		// registry — a repo a team tracks that has never been profiled.
		notFound(w, "repo")
		return
	}
	writeJSON(w, http.StatusOK, repoToJSON(*row, canEdit))
}

// repoSlugPath reads the two-segment {owner}/{repo} path id. Empty halves are
// an invalid id rather than a lookup that would match nothing in particular.
func repoSlugPath(w http.ResponseWriter, r *http.Request) (owner, repo string, ok bool) {
	owner, repo = r.PathValue("owner"), r.PathValue("repo")
	if owner == "" || repo == "" {
		httpx.WriteErrors(w, http.StatusBadRequest, httpx.ErrorItem{
			Reason: httpx.ReasonInvalidID, Message: "owner and repo are required",
		})
		return "", "", false
	}
	return owner, repo, true
}

// handleRepoUpdate updates per-repo settings like base_branch. The write
// gate is narrower than the read gate: a repository row is org-wide, so
// changing one changes behaviour for every team tracking it, and that takes
// an admin — org admin, or team admin of a tracking team. Plain members of a
// tracking team read the repo but can't repoint it.
func (s *Server) handleRepoUpdate(w http.ResponseWriter, r *http.Request) {
	orgID, ok := s.requireOrg(w, r)
	if !ok {
		return
	}
	userID := ClaimsFrom(r.Context()).Subject
	owner := r.PathValue("owner")
	repo := r.PathValue("repo")
	if owner == "" || repo == "" {
		httpx.WriteErrors(w, http.StatusBadRequest, httpx.ErrorItem{
			Reason: httpx.ReasonInvalidID, Message: "owner and repo are required",
		})
		return
	}
	ref := domain.RepoRef{Owner: owner, Repo: repo}

	access, err := s.repoMutationAccess(r.Context(), orgID, userID, owner, repo)
	if err != nil {
		internalError(w, "repos", err)
		return
	}
	// Every arm is spelled out, including the permitting one, and anything
	// unrecognized denies. This is the only enforcement point for an
	// org-wide write (RLS can't back it up), so a future enum member must
	// fail closed by default rather than reach the write by falling off the
	// end of the switch.
	switch access {
	case repoWriteAllowed:
		// Proceed to the write below.
	case repoWriteInvisible:
		// 404, not 403 — a repo outside the caller's tracked set doesn't
		// appear in their GET /api/repos list either, so don't disclose
		// its existence via a role-boundary error.
		notFound(w, "repo")
		return
	case repoWriteForbidden:
		// The inverse of the above: this repo *is* in the caller's own
		// GET /api/repos list, so 404 would read as a bug. Say plainly
		// that it's a permission boundary.
		forbidden(w, "changing repo settings requires org admin or team admin of a team tracking this repo")
		return
	default:
		internalError(w, "repos", fmt.Errorf("unhandled repo write access %d for %s", access, ref.Slug()))
		return
	}

	// Use json.RawMessage to distinguish null (clear) from omitted (no change).
	// *string can't tell them apart — both decode to nil.
	var req struct {
		BaseBranch json.RawMessage `json:"base_branch,omitempty"`
	}
	if !httpx.DecodeJSONStrict(w, r, &req) {
		return
	}

	// A PATCH that names no field wrote nothing, so it must not answer
	// "updated" — the one response a client cannot tell from a real write.
	if req.BaseBranch == nil {
		httpx.WriteErrors(w, http.StatusBadRequest, httpx.ErrorItem{
			Reason:  httpx.ReasonMissingField,
			Message: "no fields to update: provide base_branch (null clears it)",
			Field:   "base_branch",
		})
		return
	}
	var branch string
	if string(req.BaseBranch) == "null" {
		branch = "" // explicit null → clear
	} else if err := json.Unmarshal(req.BaseBranch, &branch); err != nil {
		httpx.WriteErrors(w, http.StatusBadRequest, httpx.ErrorItem{
			Reason: httpx.ReasonInvalidField, Message: "base_branch must be a string or null", Field: "base_branch",
		})
		return
	}
	// Resolve the path segment to a row and write by its id, inside the one
	// transaction so the resolution and the write see the same row. The path
	// segment is a name — it is what the picker rendered and what the frontend
	// echoes back — and a name is a thing that stops resolving: a rename
	// landing between the page load and the save used to leave the UPDATE
	// matching zero rows while this handler answered "updated".
	//
	// The lookup sits beside repoMutationAccess above rather than replacing
	// it: that gate reads team_github_repos to decide who may write, and this
	// reads repositories to decide what is being written. A repository outside
	// the caller's tracked set never reaches here at all.
	if err := s.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		row, e := tx.Repos.GetByRef(r.Context(), orgID, ref)
		if e != nil {
			return e
		}
		if row == nil {
			return errRepoGone
		}
		return tx.Repos.UpdateBaseBranch(r.Context(), orgID, row.ID, branch)
	}); err != nil {
		// Either half can report the row is gone — the read as a nil, the
		// write as a miss on an id that resolved a statement ago. Both mean
		// the same thing to the caller, and neither is a server fault.
		if errors.Is(err, errRepoGone) || errors.Is(err, db.ErrNoSuchRepository) {
			notFound(w, "repo")
			return
		}
		internalError(w, "repos", err)
		return
	}

	writeJSON(w, http.StatusOK, map[string]string{"status": "updated"})
}

// maxBranchPageSize is the branch list's declared page ceiling. It replaces
// the hardcoded 30 the route used to apply silently: a caller now asks for the
// window it wants and learns when it asked for too much, instead of receiving
// a truncated answer that looks complete. It is lower than the shared
// MaxPageSize because every page is an upstream GitHub call, and the picker
// this feeds is a type-to-filter box, not a browse.
const maxBranchPageSize = 100

// branchListRequest is the body of POST /api/repos/{owner}/{repo}/branches/list.
// Q is the same substring filter the old ?q= carried, moved to a body field
// with its semantics unchanged.
type branchListRequest struct {
	Q string `json:"q"`

	httpx.PageRequest
}

type branchListFilterKey struct {
	Q string `json:"q"`
}

// branchJSON is one row. It is an object rather than a bare string so the
// shape can gain a field (protected, default, last commit) without becoming a
// second list contract.
type branchJSON struct {
	Name string `json:"name"`
}

// handleRepoBranches returns branches for a repo, with optional search
// filtering. Gated the same as handleRepositories/handleRepoUpdate
// (TFAC-559): a non-admin member can only list branches for a repo their own
// team tracks.
//
// It is a proxy list: the rows come from GitHub, which reports no total for
// this read, so total_count is null and next_page_token wraps the upstream
// page cursor. See httpx.WriteProxyList.
//
// POST /api/repos/{owner}/{repo}/branches/list
func (s *Server) handleRepoBranches(w http.ResponseWriter, r *http.Request) {
	owner, repo, ok := repoSlugPath(w, r)
	if !ok {
		return
	}

	orgID, ok := s.requireOrg(w, r)
	if !ok {
		return
	}
	userID := ClaimsFrom(r.Context()).Subject

	var req branchListRequest
	if !httpx.DecodeJSONStrict(w, r, &req) {
		return
	}
	var v httpx.Validation
	page := httpx.ResolvePage(&v, req.PageRequest,
		httpx.FilterFingerprint(branchListFilterKey{Q: req.Q}), maxBranchPageSize)
	upstreamPage, ok := upstreamPageFromCursor(&v, page.Cursor)
	if v.Flush(w, http.StatusBadRequest) {
		return
	}
	_ = ok // the fault, if any, is already recorded on v

	if allowed, err := s.repoAccessAllowed(r.Context(), orgID, userID, owner, repo); err != nil {
		internalError(w, "repos", err)
		return
	} else if !allowed {
		notFound(w, "repo")
		return
	}

	// Resolve per-repo (org App installation token → PAT) so App-only orgs
	// (no PAT) list branches through the installation client instead of 400-ing
	// on a nil global client. owner/repo are path values; a selective App
	// install that doesn't cover this repo falls through to the PAT.
	client, err := s.ghResolver.ClientForRepo(r.Context(), orgID, owner, repo)
	if err != nil {
		if errors.Is(err, ghclient.ErrNoGitHubCredentials) {
			reposLog.Warn("github not configured", "org", orgID, "owner", owner, "repo", repo, "error", err)
			writeNotConfigured(w, "GitHub is not connected for this workspace")
			return
		}
		internalError(w, "repos", err)
		return
	}

	branches, rawCount, err := client.ListBranchesPage(r.Context(), owner, repo, req.Q, page.Limit, upstreamPage)
	if err != nil {
		reposLog.Warn("fetch branches failed", "org", orgID, "owner", owner, "repo", repo, "error", err)
		writeUpstreamGitHub(w, "failed to fetch branches from GitHub", err)
		return
	}

	out := make([]branchJSON, len(branches))
	for i, b := range branches {
		out[i] = branchJSON{Name: b.Name}
	}
	// "Is there more" is decided by the RAW upstream page, not by how many
	// rows survived the filter: a full upstream page means GitHub has more to
	// give even when none of it matched `q`, and stopping on a short filtered
	// page would end the walk at the first unlucky page.
	next := ""
	if rawCount == page.Limit {
		next = strconv.Itoa(upstreamPage + 1)
	}
	httpx.WriteProxyList(w, page, out, next)
}

// upstreamPageFromCursor decodes the page-number cursor the proxy lists carry.
// An empty cursor is the first page. Anything else must be a positive integer
// — the token is opaque and only this server mints one, so a value that isn't
// is a tampered or stale token, not a page.
func upstreamPageFromCursor(v *httpx.Validation, cursor string) (int, bool) {
	if cursor == "" {
		return 1, true
	}
	n, err := strconv.Atoi(cursor)
	if err != nil || n < 1 {
		v.Add(httpx.ErrorItem{
			Reason:  httpx.ReasonInvalidParam,
			Message: "page_token is not a valid page token",
			Field:   "page_token",
		})
		return 1, false
	}
	return n, true
}

// Repo *tracking* selection is per-team: writes go through
// PUT /api/settings/team/{id}/repos (handleTeamReposPut), which writes
// team_github_repos and brings any newly-tracked repository into the registry.
// The old org-global POST /api/repos was removed in favor of that single,
// team-admin-gated entry point; GET /api/repos (the org-wide registry),
// PATCH /api/repos/{owner}/{repo} (base_branch), and GET
// /api/repos/{owner}/{repo}/branches remain, scoped per-caller.
//
// GET /api/repos lists the registry, which is a superset of the tracked set:
// a repository the last team untracked keeps its row (a run's worktree ledger
// or a pinned project may still name it), so an org admin can still see it and
// re-track it. Non-admins see only what their own teams track.
//
// Reads (isOrgAdmin / repoAccessAllowed) go by membership: an org admin
// sees every repo, a member only the repos their own team(s) track. The
// PATCH gate (repoMutationAccess) is narrower still, because a repository
// row is org-wide and has no team to write against: it takes an org admin or a
// team admin of a team that tracks the repo.
