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
	"github.com/sky-ai-eng/triage-factory/internal/server/httpx"
)

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
	pre, ok := s.pickerCredentialClass(w, r, orgID, userID)
	if !ok {
		return
	}
	class := pre.class

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
		// ...and this is where an unestablished preflight has to be paid for. The
		// readiness state is a PROMISE that looking will produce something, and
		// the preflight is what backs it; when its installations read failed we
		// could not establish that, so an org whose mirror is also empty would be
		// told to wait for a discovery that may never resolve. Serving the error
		// is the honest answer — and it costs nothing in the common case, because
		// a failed read on an org that HAS a mirror never reaches here.
		if pre.err != nil {
			reposLog.Error("cannot establish whether discovery will resolve", "org", orgID, "error", pre.err)
			internalError(w, "repos", pre.err)
			return
		}
		writeJSON(w, http.StatusOK, githubRepoListResponse{
			Items:  []githubRepoJSON{},
			Status: githubRepoStatusDiscovering,
		})
		return
	}

	rows, total, err := s.reachableRepos.ListReachableSystem(r.Context(), orgID, class, q,
		db.ListOpts{Limit: page.Limit, Offset: page.Offset, CountOnly: page.CountOnly})
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

// pickerPreflight is what the configuration check established. class is the
// credential class the org's reachable entries are keyed under; err is set when
// the check could not be COMPLETED — as opposed to completing and finding the
// org unusable, which refuses the request outright.
//
// A non-nil err is deliberately not fatal on its own. It means one thing was
// unreadable, and what that thing decides is only whether the org is usable at
// all — not what the picker serves, which is the mirror. So a request whose
// mirror answers is served, and only a request that would otherwise fall into
// the readiness state has to reckon with it: that state promises discovery will
// resolve, and an unestablished preflight cannot back the promise.
type pickerPreflight struct {
	class domain.GitHubCredentialClass
	err   error
}

// pickerCredentialClass resolves the class the org's reachable entries are keyed
// under, and refuses the request when GitHub is established to be unusable for
// the org. It writes the response on every false return.
//
// The two refusals are deliberately different, and the difference is the
// first-run dead-end this handler exists to keep diagnosable: an org whose
// credential is an App — its own or the deployment's — that is installed
// nowhere is told to install it, while everything else gets the generic "not
// connected". Reporting the first as the second reads as "add a PAT", which is
// the one thing that will not help.
//
// A third outcome — "we could not tell" — is neither of those and is reported
// through pickerPreflight.err rather than as a refusal, because the two claims
// it could be mistaken for are both wrong: "not installed" is a claim only a
// SUCCESSFUL read of zero installations can support, and a hard failure would
// blank a picker whose data is sitting in the mirror and did not need this read
// at all.
func (s *Server) pickerCredentialClass(w http.ResponseWriter, r *http.Request, orgID, userID string) (pickerPreflight, bool) {
	// The EFFECTIVE class — the org's stored class narrowed by the App-XOR-PAT
	// gate — because that is what the refresh keys its rows under. An
	// unreadable or unrecognised class fails the request: the picker's whole job
	// is to show what the org can reach, and showing the wrong credential
	// system's idea of that is worse than showing an error.
	class, err := s.reachableCredentialClass(r.Context(), orgID)
	if err != nil {
		// The class is not optional: it is the key every subsequent read uses, so
		// there is nothing to serve without it.
		reposLog.Error("resolve reachable credential class failed", "org", orgID, "error", err)
		internalError(w, "repos", err)
		return pickerPreflight{}, false
	}
	if class.AppTier() {
		// An App with at least one installation is usable by definition — the
		// installations ARE the reach, whether the App is the workspace's own or
		// the deployment's shared one. A read failure here is carried rather than
		// refused: reporting it would mean claiming "not installed", which only a
		// successful read of zero installations can support.
		insts, err := s.githubApps.ListInstallationsForOrgSystem(r.Context(), orgID)
		if err != nil {
			reposLog.Warn("list installations failed; serving the mirror if there is one", "org", orgID, "error", err)
			return pickerPreflight{class: class, err: err}, true
		}
		if len(insts) == 0 {
			reposLog.Warn("app usable but installed on zero accounts", "org", orgID, "class", class)
			writeNotConfigured(w, "the GitHub App is not installed on any account")
			return pickerPreflight{}, false
		}
		return pickerPreflight{class: class}, true
	}

	// PAT tier — the only one left, since both App classes are answered above and
	// an unknown class never resolves at all. Credentials + per-org base URL read
	// through the app pool inside WithTx so SecretStore decrypts under the user's
	// claims and org_settings_select RLS is enforced — the same shape the rest of
	// the settings surface uses.
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
		return pickerPreflight{}, false
	}
	if creds.GitHubPAT == "" || (orgSet.GitHubBaseURL == "" && creds.GitHubURL == "") {
		reposLog.Warn("github not configured, no usable app installation and no pat", "org", orgID)
		writeNotConfigured(w, "GitHub is not connected for this workspace")
		return pickerPreflight{}, false
	}
	return pickerPreflight{class: class}, true
}

// isOrgAdmin reports whether userID is an org admin of orgID.
func (s *Server) isOrgAdmin(ctx context.Context, orgID, userID string) (bool, error) {
	return s.az.UserIsOrgAdmin(ctx, userID, orgID)
}

// The three repo gates below share a shape: the caller's org-admin answer is
// resolved ONCE per request, outside any transaction (isOrgAdmin opens its
// own), and handed in; each gate then does its team-scoped reads on the
// transaction the handler already holds. Nothing here opens a nested
// transaction — a gate called from inside a WithTx would take a second
// connection out of the pool for the length of the outer one, which on a
// list read is once per row.

// repoVisible reports whether the caller may *read* the given repo
// (TFAC-559): an org admin always may; a non-admin member may only when the
// repo is tracked by at least one of their teams. Local mode (N=1) has no
// team boundary and resolves through isAdmin, which its isOrgAdmin
// short-circuits to true.
//
// Mutations are a strictly narrower gate — see repoMutationAccess.
func repoVisible(ctx context.Context, tx db.TxStores, orgID string, isAdmin bool, row domain.Repository) (bool, error) {
	if isAdmin {
		return true, nil
	}
	return tx.TeamGitHubRepos.TracksRepoViewerScoped(ctx, orgID, row.Owner, row.Repo)
}

// repoCanEdit resolves the per-row can_edit annotation the read routes carry:
// true for an org admin, otherwise true only when the caller administers a
// team that tracks the repo. It is the read-shaped half of repoMutationAccess
// — same predicate, no 404-vs-403 distinction to make.
func repoCanEdit(ctx context.Context, tx db.TxStores, orgID string, isAdmin bool, row domain.Repository) (bool, error) {
	if isAdmin {
		return true, nil
	}
	return tx.TeamGitHubRepos.TracksRepoViewerAdminScoped(ctx, orgID, row.Owner, row.Repo)
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
// Local mode (N=1) resolves to repoWriteAllowed via isAdmin, which its
// isOrgAdmin short-circuits to true before any store call.
func repoMutationAccess(ctx context.Context, tx db.TxStores, orgID string, isAdmin bool, row domain.Repository) (repoWriteAccess, error) {
	if isAdmin {
		return repoWriteAllowed, nil
	}
	teamAdmin, err := repoCanEdit(ctx, tx, orgID, isAdmin, row)
	if err != nil {
		return repoWriteInvisible, err
	}
	if teamAdmin {
		// A team admin of a tracking team is necessarily a member of one, so
		// the visibility read below would only confirm what we already know.
		return repoWriteAllowed, nil
	}
	tracked, err := repoVisible(ctx, tx, orgID, isAdmin, row)
	if err != nil {
		return repoWriteInvisible, err
	}
	if tracked {
		return repoWriteForbidden, nil
	}
	return repoWriteInvisible, nil
}

// repoJSON is the registry's row shape — the list rows and the single read
// alike. CanEdit mirrors the PATCH gate (repoMutationAccess) per row, so the
// page can render a read-only base branch instead of a control that 403s on
// use. The client must not re-derive it from role: org admin and team admin
// are orthogonal, and only the server knows which teams tracking a given repo
// the caller administers.
//
// ID is the registry row id and Slug is the display name, and the split is the
// whole point: the id is what a client sends back (every repo route is keyed
// on it), the slug is what a person reads. Owner and Repo stay alongside for
// the clients that need the halves — a GitHub blob URL, a per-owner group —
// and Slug rather than a client-side join keeps exactly one renderer per side,
// domain.Repository.Slug() here and this field there.
type repoJSON struct {
	ID             string  `json:"id"`
	Slug           string  `json:"slug"`
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
		ID:             row.ID,
		Slug:           row.Slug(),
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
		opts := db.ListOpts{Limit: page.Limit, Offset: page.Offset, CountOnly: page.CountOnly}
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
			canEdit[i], e = repoCanEdit(r.Context(), tx, orgID, isAdmin, row)
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

// repoAnnotation says whether a read wants can_edit resolved alongside the
// row. Named rather than a bare bool at the call sites, because it is a
// decision and not a flag: the row-shaped reads serialize can_edit, so they
// pay for it; the branch list does not carry the field at all, and it is one
// upstream call per keystroke on a type-to-filter box, so it does not pay a
// non-admin's extra EXISTS to compute something it then discards.
type repoAnnotation bool

const (
	withCanEdit repoAnnotation = true
	noCanEdit   repoAnnotation = false
)

// readRepo resolves one registry row for a read route. lookup performs the
// resolution — by id for the canonical route, by ref for the by-name one — so
// every read shares this gate and none can grow its own.
//
// Resolve first, gate second, and both failures answer the same 404: a row no
// id answers to and a row outside the caller's tracked set are indistinguishable
// on the wire, which is what the two-valued disclosure rule asks for on a read.
// (Writes are where the distinction is drawn — see repoMutationAccess.)
//
// A nil row with a nil error is that 404; the caller writes it. canEdit is
// false whenever annotate is noCanEdit — a caller that did not ask for it must
// not read it.
func (s *Server) readRepo(
	ctx context.Context, orgID, userID string, annotate repoAnnotation,
	lookup func(ctx context.Context, tx db.TxStores) (*domain.Repository, error),
) (*domain.Repository, bool, error) {
	isAdmin, err := s.isOrgAdmin(ctx, orgID, userID)
	if err != nil {
		return nil, false, err
	}
	var (
		row     *domain.Repository
		canEdit bool
	)
	err = s.tx.WithTx(ctx, orgID, userID, func(tx db.TxStores) error {
		found, e := lookup(ctx, tx)
		if e != nil || found == nil {
			return e
		}
		visible, e := repoVisible(ctx, tx, orgID, isAdmin, *found)
		if e != nil || !visible {
			return e
		}
		row = found
		if annotate == noCanEdit {
			return nil
		}
		canEdit, e = repoCanEdit(ctx, tx, orgID, isAdmin, *found)
		return e
	})
	return row, canEdit, err
}

// handleRepoGet is the canonical single read for a registry row, in the list's
// row shape.
//
// GET /api/repos/{id}
func (s *Server) handleRepoGet(w http.ResponseWriter, r *http.Request) {
	orgID, ok := s.requireOrg(w, r)
	if !ok {
		return
	}
	userID := ClaimsFrom(r.Context()).Subject
	id, ok := repoIDPath(w, r)
	if !ok {
		return
	}

	row, canEdit, err := s.readRepo(r.Context(), orgID, userID, withCanEdit,
		func(ctx context.Context, tx db.TxStores) (*domain.Repository, error) {
			return repoByID(ctx, tx.Repos.Get, orgID, id)
		})
	if err != nil {
		internalError(w, "repos", err)
		return
	}
	if row == nil {
		notFound(w, "repo")
		return
	}
	writeJSON(w, http.StatusOK, repoToJSON(*row, canEdit))
}

// handleRepoGetByName is the same read addressed by the provider's current
// name for the repository — the `GET /…/by-name/{name}` half the API rules
// give every resource with a unique name, and here the org-scoped folded
// identity index is that uniqueness.
//
// It is the ONLY repo route that accepts a slug. Everything else takes the id
// it was served, including the writes: a client that wants to mutate goes
// through the id, so a rename between the page render and the save cannot make
// the request address a different repository (or nothing at all).
//
// GET /api/repos/by-name/{owner}/{repo}
func (s *Server) handleRepoGetByName(w http.ResponseWriter, r *http.Request) {
	orgID, ok := s.requireOrg(w, r)
	if !ok {
		return
	}
	userID := ClaimsFrom(r.Context()).Subject
	owner, repo, ok := repoSlugPath(w, r)
	if !ok {
		return
	}

	row, canEdit, err := s.readRepo(r.Context(), orgID, userID, withCanEdit,
		func(ctx context.Context, tx db.TxStores) (*domain.Repository, error) {
			// Ref-keyed, not id-keyed: the path segments are the provider's
			// current NAME for the repository, and the registry id is a
			// separate handle (TFAC-834). GetByRef's (nil, nil) miss is the
			// answer this route wants anyway, since a name that resolves to
			// nothing is a 404 here.
			return tx.Repos.GetByRef(ctx, orgID, domain.RepoRef{Owner: owner, Repo: repo})
		})
	if err != nil {
		internalError(w, "repos", err)
		return
	}
	if row == nil {
		notFound(w, "repo")
		return
	}
	writeJSON(w, http.StatusOK, repoToJSON(*row, canEdit))
}

// repoIDPath reads the {id} path segment — the registry row id every repo
// route but by-name is addressed by. Empty is an invalid id rather than a
// lookup that would match nothing in particular.
//
// Nothing here tries to be helpful about a slug arriving in this position. It
// is not parsed, not sniffed for a '/', and not retried against the by-name
// lookup: a parameter that widens on fallback is the class the API rules ban,
// and the miss it produces instead is a clean 404 the caller can act on.
func repoIDPath(w http.ResponseWriter, r *http.Request) (string, bool) {
	id := r.PathValue("id")
	if id == "" {
		httpx.WriteErrors(w, http.StatusBadRequest, httpx.ErrorItem{
			Reason: httpx.ReasonInvalidID, Message: "repository id is required",
		})
		return "", false
	}
	return id, true
}

// repoByID adapts an id-keyed store lookup to the (nil, nil)-means-404 shape
// the routes want. ErrNoSuchRepository is the id-keyed miss (see its doc for
// why it is an error rather than an empty answer), and at an HTTP edge holding
// a client-supplied id it is exactly a 404 — the caller made the id up, or the
// row went away since it was served.
func repoByID(
	ctx context.Context,
	get func(ctx context.Context, orgID, id string) (*domain.Repository, error),
	orgID, id string,
) (*domain.Repository, error) {
	row, err := get(ctx, orgID, id)
	if errors.Is(err, db.ErrNoSuchRepository) {
		return nil, nil
	}
	return row, err
}

// repoSlugPath reads the two-segment {owner}/{repo} path of the by-name route.
// Empty halves are an invalid id rather than a lookup that would match nothing
// in particular.
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
//
// PATCH /api/repos/{id}
func (s *Server) handleRepoUpdate(w http.ResponseWriter, r *http.Request) {
	orgID, ok := s.requireOrg(w, r)
	if !ok {
		return
	}
	userID := ClaimsFrom(r.Context()).Subject
	id, ok := repoIDPath(w, r)
	if !ok {
		return
	}

	isAdmin, err := s.isOrgAdmin(r.Context(), orgID, userID)
	if err != nil {
		internalError(w, "repos", err)
		return
	}
	// Resolve the id to a row before the gate, in the same transaction as the
	// gate, because the gate reads team_github_repos by name and only the row
	// knows what this repository is currently called. A rename between the
	// page render and this save moves owner/repo and leaves the id alone, so
	// the write still lands on the repo the user was looking at — the failure
	// mode the slug-addressed route had, where the request 404'd on a name
	// nothing answered to any more.
	var (
		row    *domain.Repository
		access repoWriteAccess
	)
	if err := s.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		found, e := repoByID(r.Context(), tx.Repos.Get, orgID, id)
		if e != nil || found == nil {
			return e
		}
		row = found
		access, e = repoMutationAccess(r.Context(), tx, orgID, isAdmin, *found)
		return e
	}); err != nil {
		internalError(w, "repos", err)
		return
	}
	if row == nil {
		notFound(w, "repo")
		return
	}
	ref := domain.RepoRef{Owner: row.Owner, Repo: row.Repo}
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
	// Write by the id the caller addressed. The row resolved a moment ago to
	// run the gate, and UpdateBaseBranch reports ErrNoSuchRepository rather
	// than a silent zero-row UPDATE, so a repository deleted in between is a
	// 404 and not an "updated" answering for a write that did nothing.
	var updated domain.Repository
	if err := s.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		var e error
		updated, e = tx.Repos.UpdateBaseBranch(r.Context(), orgID, row.ID, branch)
		return e
	}); err != nil {
		if errors.Is(err, db.ErrNoSuchRepository) {
			notFound(w, "repo")
			return
		}
		internalError(w, "repos", err)
		return
	}

	// The resource, in the same shape the reads serve it, off the row the
	// write returned — not a status stub, and not the request body echoed
	// back. can_edit is true by construction: the switch above only reaches
	// here on repoWriteAllowed, which is the same predicate the annotation
	// reports.
	writeJSON(w, http.StatusOK, repoToJSON(updated, true))
}

// maxBranchPageSize is the branch list's declared page ceiling. It replaces
// the hardcoded 30 the route used to apply silently: a caller now asks for the
// window it wants and learns when it asked for too much, instead of receiving
// a truncated answer that looks complete. It is lower than the shared
// MaxPageSize because every page is an upstream GitHub call, and the picker
// this feeds is a type-to-filter box, not a browse.
const maxBranchPageSize = 100

// branchListRequest is the body of POST /api/repos/{id}/branches/list.
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
// POST /api/repos/{id}/branches/list
func (s *Server) handleRepoBranches(w http.ResponseWriter, r *http.Request) {
	id, ok := repoIDPath(w, r)
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

	// The upstream call is name-shaped — GitHub has no idea what a registry id
	// is — so the id resolves to a row here and the row supplies the name. That
	// is the general shape of this flip: TF ids on the wire, provider
	// coordinates on the provider hop, resolved once at the boundary between.
	row, _, err := s.readRepo(r.Context(), orgID, userID, noCanEdit,
		func(ctx context.Context, tx db.TxStores) (*domain.Repository, error) {
			return repoByID(ctx, tx.Repos.Get, orgID, id)
		})
	if err != nil {
		internalError(w, "repos", err)
		return
	}
	if row == nil {
		notFound(w, "repo")
		return
	}
	owner, repo := row.Owner, row.Repo

	// Resolve per-repo (org App installation token → PAT) so App-only orgs
	// (no PAT) list branches through the installation client instead of 400-ing
	// on a nil global client. A selective App install that doesn't cover this
	// repo falls through to the PAT.
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
// PUT /api/teams/{team_id}/github-repos (handleTeamReposPut), which writes
// team_github_repos and brings any newly-tracked repository into the registry.
// That save stays name-shaped, and deliberately: it is the ingest edge, where
// tracking a repository is what mints its registry row, so its input names
// things that may not have a row to be addressed by yet.
//
// The old org-global POST /api/repos was removed in favor of that single,
// team-admin-gated entry point; the registry list, the single read, the
// base_branch PATCH and the branch list remain, scoped per-caller and all
// addressed by the registry row id.
//
// GET /api/repos lists the registry, which is a superset of the tracked set:
// a repository the last team untracked keeps its row (a conversation's
// worktree ledger or a pinned project may still name it), so an org admin can
// still see it and re-track it. Non-admins see only what their own teams
// track.
//
// Reads (isOrgAdmin / repoVisible) go by membership: an org admin
// sees every repo, a member only the repos their own team(s) track. The
// PATCH gate (repoMutationAccess) is narrower still, because a repository
// row is org-wide and has no team to write against: it takes an org admin or a
// team admin of a team that tracks the repo.
