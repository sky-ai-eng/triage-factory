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

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	ghclient "github.com/sky-ai-eng/triage-factory/internal/github"
	"github.com/sky-ai-eng/triage-factory/internal/server/httpx"
)

// dashboardHandler serves the personal dashboard endpoints: PR stats, the
// user's open PRs, and live per-PR status / draft toggles. It holds the
// transactional store runner and the GitHub credential resolver the live
// per-PR calls use; routes() registers its methods through the
// api()/apiMutating() middleware wrappers.
type dashboardHandler struct {
	tx         db.TxRunner
	ghResolver ghclient.Resolver
	// backfill kicks a one-shot dashboard-history backfill for (user, host),
	// fire-and-forget and marker-guarded downstream. Bound to
	// Server.kickDashboardBackfill; a no-op in local mode (per-cycle Phase 1b
	// owns history there). Nil-safe via kickBackfill (TFAC-396).
	backfill func(orgID, userID, login, host string)
}

// kickBackfill triggers the dashboard-history backfill for a resolved
// (user, host), if a backfiller is wired and the identity is bound. Cheap and
// idempotent — the marker downstream prevents re-running for an identity that's
// already been backfilled, so calling it on every dashboard read is fine.
func (dh *dashboardHandler) kickBackfill(orgID, userID, login, host string) {
	if dh.backfill != nil && login != "" && host != "" {
		dh.backfill(orgID, userID, login, host)
	}
}

// dashboardStatsWindowDays is the trailing window /api/dashboard/stats
// aggregates over when the caller names none. It is the same 30 days the store
// used to hardcode; the difference is that a caller can now say otherwise, and
// the panel's label and the query it describes come from the same place.
const dashboardStatsWindowDays = 30

// handleDashboardStats returns aggregated PR statistics from entity snapshots
// over the ?since window (RFC3339 or YYYY-MM-DD; default: the last 30 days).
func (dh *dashboardHandler) handleDashboardStats(w http.ResponseWriter, r *http.Request) {
	orgID, ok := requireOrg(w, r)
	if !ok {
		return
	}
	var v httpx.Validation
	since := time.Now().AddDate(0, 0, -dashboardStatsWindowDays)
	if raw := strings.TrimSpace(r.URL.Query().Get("since")); raw != "" {
		t, err := parseUsageTime(raw)
		if err != nil {
			v.Invalid("since", "must be RFC3339 or YYYY-MM-DD")
		} else {
			since = t
		}
	}
	if v.Flush(w, http.StatusBadRequest) {
		return
	}
	userID := ClaimsFrom(r.Context()).Subject
	var (
		host     string
		username string
		stats    *domain.DashboardStats
	)
	if err := dh.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		// The GitHub host comes from org_settings, not an integration
		// credential. App-mode orgs (and multi-mode org-PAT orgs) have no
		// per-user PAT, so the pre-TFAC-396 `creds.GitHubPAT == ""` gate hid
		// the dashboard entirely even though the poller populates snapshots via
		// the App token. The only real preconditions are a resolvable GitHub
		// host and a bound host-scoped identity — neither needs a PAT. Host
		// resolution mirrors handleGitHubIdentityPAT so the (user, host) key
		// agrees across surfaces.
		orgSet, lerr := tx.Orgs.GetSettings(r.Context(), orgID)
		if lerr != nil {
			return lerr
		}
		ghWeb, okHost := resolveGitHubHost(orgSet.GitHubBaseURL)
		if !okHost {
			return nil
		}
		host = ghWeb
		// Propagate a real lookup failure as a 5xx; only a missing row (->
		// "", nil) degrades to the empty-dashboard response below. Swallowing
		// the error would turn a DB fault into a misleading empty dashboard.
		username, lerr = tx.Users.GetGitHubLogin(r.Context(), userID, ghWeb)
		if lerr != nil {
			return lerr
		}
		if username == "" {
			return nil
		}
		var e error
		stats, e = tx.Dashboard.Stats(r.Context(), orgID, username, since)
		return e
	}); err != nil {
		internalError(w, "dashboard", err)
		return
	}
	if username == "" {
		writeJSON(w, http.StatusOK, map[string]any{})
		return
	}

	dh.kickBackfill(orgID, userID, username, host)
	writeJSON(w, http.StatusOK, stats)
}

// handleDashboardPRs returns one page of the caller's PRs from entity
// snapshots, newest polled first.
//
// "Mine" is a union of two legs: pull requests my GitHub login authored, and
// pull requests a run I commissioned opened under the bot's login
// (entities.commissioned_by_user_id, stamped at the exec recording funnel).
// The second leg is why an unbound GitHub identity no longer short-circuits
// the read: a person who has never bound a login can still have asked for work
// that produced pull requests, and hiding those behind an unrelated missing
// binding would answer the wrong question. The response is still the standard
// list envelope in every case — an empty page with total 0, not a bare `[]`
// and not a 404. The dashboard is a personal view, so "you have no PRs here
// yet" and "your GitHub identity isn't bound" are the same empty list to a
// client; the panel decides what to say about it from /api/me, which is where
// identity state actually lives.
//
// POST /api/dashboard/prs/list
func (dh *dashboardHandler) handleDashboardPRs(w http.ResponseWriter, r *http.Request) {
	orgID, ok := requireOrg(w, r)
	if !ok {
		return
	}
	var req prListRequest
	if !httpx.DecodeJSONStrict(w, r, &req) {
		return
	}
	var v httpx.Validation
	// The identity legs are the route's subject, not a filter — the body
	// cannot name them — so the fingerprint is over the states filter and the
	// scope that says which population this token walks.
	states := canonicalPRStates(&v, req.States)
	page := httpx.ResolvePage(&v, req.PageRequest, prListFingerprint("dashboard-prs", "", states), 0)
	if v.Flush(w, http.StatusBadRequest) {
		return
	}
	userID := ClaimsFrom(r.Context()).Subject
	var (
		host     string
		username string
		prs      []domain.PRSummaryRow
		total    int
	)
	if err := dh.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		// Host from org_settings, not a PAT credential — see handleDashboardStats
		// for the TFAC-396 rationale. App-mode / org-PAT orgs have no PAT but do
		// have populated snapshots and a bound identity.
		orgSet, lerr := tx.Orgs.GetSettings(r.Context(), orgID)
		if lerr != nil {
			return lerr
		}
		// An unresolvable host and an unbound identity both leave username
		// empty, which drops the author leg and leaves the commissioned one.
		// Propagate a real lookup failure as a 5xx: only a missing row (-> "",
		// nil) is an answer. See handleDashboardStats.
		if ghWeb, okHost := resolveGitHubHost(orgSet.GitHubBaseURL); okHost {
			host = ghWeb
			username, lerr = tx.Users.GetGitHubLogin(r.Context(), userID, ghWeb)
			if lerr != nil {
				return lerr
			}
		}
		var e error
		prs, total, e = tx.Dashboard.PRs(r.Context(), orgID,
			db.PRViewer{Login: username, UserID: userID},
			db.PRListFilter{States: states},
			db.ListOpts{Limit: page.Limit, Offset: page.Offset, CountOnly: page.CountOnly})
		return e
	}); err != nil {
		internalError(w, "dashboard", err)
		return
	}
	// The backfill is about a GitHub identity's history, so it still needs a
	// bound one; the list above does not.
	dh.kickBackfill(orgID, userID, username, host)
	httpx.WriteList(w, page, prs, total)
}

// dashboardPRRef strictly parses the {owner}/{repo}/{number} triple the live
// per-PR routes address a pull request by, reporting every fault at once.
//
// All three segments are path, not query. A pull request is identified by all
// three — a bare number names nothing — so the address should carry all three,
// and a caller that omits one should fail to route rather than reach a handler
// that then has to invent the missing-parameter error itself. The ?repo= half
// this replaced also let "owner/" and "/repo" through as a two-element split.
func dashboardPRRef(w http.ResponseWriter, r *http.Request) (owner, repo string, number int, ok bool) {
	var v httpx.Validation
	owner, repo = r.PathValue("owner"), r.PathValue("repo")
	if owner == "" || repo == "" {
		// Unreachable through the mux (an empty segment doesn't match the
		// pattern), but the handler is called directly in tests and the
		// contract shouldn't depend on the router to hold.
		v.Add(httpx.ErrorItem{Reason: httpx.ReasonInvalidID, Message: "owner and repo are required"})
	}
	number, err := strconv.Atoi(r.PathValue("number"))
	if err != nil || number <= 0 {
		v.Add(httpx.ErrorItem{Reason: httpx.ReasonInvalidID, Message: "PR number must be a positive integer"})
	}
	if v.Flush(w, http.StatusBadRequest) {
		return "", "", 0, false
	}
	return owner, repo, number, true
}

// handleDashboardPRStatus fetches live CI/review status for a single PR.
// This stays as a live API call since it's on-demand detail, not aggregated data.
func (dh *dashboardHandler) handleDashboardPRStatus(w http.ResponseWriter, r *http.Request) {
	owner, repo, number, ok := dashboardPRRef(w, r)
	if !ok {
		return
	}

	orgID, ok := requireOrg(w, r)
	if !ok {
		return
	}

	// Repo-scoped read: decide the credential tier on the whole owner/repo,
	// not just the owner. A "Selected repositories" App install mints a token
	// for any repo under owner but 403s on repos outside the grant, so a
	// bare-owner resolve would skip the PAT that would have worked. ClientForRepo
	// falls through to the PAT when the App doesn't cover this repo.
	client, err := dh.ghResolver.ClientForRepo(r.Context(), orgID, owner, repo)
	if err != nil {
		if errors.Is(err, ghclient.ErrNoGitHubCredentials) {
			writeNotConfigured(w, "GitHub not configured")
			return
		}
		// Real DB/vault/RLS failure — internalError redacts in multi-mode + logs detail.
		internalError(w, "dashboard", err)
		return
	}
	status, err := client.GetPRStatus(r.Context(), owner, repo, number)
	if err != nil {
		internalError(w, "dashboard", err)
		return
	}

	writeJSON(w, http.StatusOK, status)
}

func (dh *dashboardHandler) handleDashboardPRDraft(w http.ResponseWriter, r *http.Request) {
	owner, repo, number, ok := dashboardPRRef(w, r)
	if !ok {
		return
	}

	// Draft is a pointer so an absent field is distinguishable from an
	// explicit false: with a plain bool, {} would zero-value into "not a
	// draft" and silently mark the PR ready.
	var body struct {
		Draft *bool `json:"draft"`
	}
	if !httpx.DecodeJSONStrict(w, r, &body) {
		return
	}
	if body.Draft == nil {
		httpx.WriteErrors(w, http.StatusBadRequest, httpx.ErrorItem{
			Reason: httpx.ReasonMissingField, Message: "draft is required", Field: "draft",
		})
		return
	}
	draft := *body.Draft

	// requireOrg must run BEFORE the GitHub mutation below — a 409 after
	// the external draft flip would have already changed the PR on
	// GitHub while reporting failure to the client, with the local
	// snapshot patch never reached. Gate org access first; mutate
	// external + local state only once the request is authorized.
	orgID, ok := requireOrg(w, r)
	if !ok {
		return
	}
	userID := ClaimsFrom(r.Context()).Subject

	// Repo-scoped mutation: resolve on the whole owner/repo so a selective App
	// install that doesn't cover this repo falls through to the PAT instead of
	// minting a token that 403s on the draft toggle (see handleDashboardPRStatus).
	client, err := dh.ghResolver.ClientForRepo(r.Context(), orgID, owner, repo)
	if err != nil {
		if errors.Is(err, ghclient.ErrNoGitHubCredentials) {
			writeNotConfigured(w, "GitHub not configured")
			return
		}
		internalError(w, "dashboard", err)
		return
	}

	if draft {
		err = client.ConvertPRToDraft(r.Context(), owner, repo, number)
	} else {
		err = client.MarkPRReady(r.Context(), owner, repo, number)
	}
	if err != nil {
		internalError(w, "dashboard", err)
		return
	}

	// Audit the org-credential board-drag draft toggle (TFAC-483): a
	// human-authorized, org-executed GitHub write. No conversation (a direct
	// dashboard action, not an agent's) and no team (an org-wide PR with no
	// team context), so it surfaces in the org governance feed, not a team
	// feed. Best-effort in its own tx after the pessimistic GitHub write —
	// never fails the toggle.
	draftAction := domain.ActionPRMarkedReady
	draftFrom, draftTo := domain.ArtifactStatePRDraft, domain.ArtifactStatePROpen
	if draft {
		draftAction = domain.ActionPRConvertedToDraft
		draftFrom, draftTo = domain.ArtifactStatePROpen, domain.ArtifactStatePRDraft
	}
	recordExternalActionBestEffort(r.Context(), dh.tx, orgID, userID, domain.ExternalAction{
		Provider:    domain.ArtifactProviderGitHub,
		Action:      draftAction,
		Target:      fmt.Sprintf("%s/%s#%d", owner, repo, number),
		ExternalID:  strconv.Itoa(number),
		URL:         domain.GitHubPullURL(owner+"/"+repo, number),
		FromState:   draftFrom,
		ToState:     draftTo,
		ActorUserID: userID,
		Credential:  githubCredentialFor(r.Context(), dh.ghResolver, orgID, owner, repo),
	})

	// Patch the local entity snapshot to match the state we just pushed to
	// GitHub. Without this, the frontend's subsequent fetchAll() reads the
	// stale pre-mutation snapshot and the card snaps back to its old column
	// until the next poll cycle (up to several minutes later).
	//
	// TODO: we deliberately don't fire a synthetic pr:ready_for_review
	// / pr:converted_to_draft event here — the user's UI click is its own
	// signal and a second event would race the next poll's diff and confuse
	// the audit trail. Revisit if a user reports "my trigger didn't fire
	// when I dragged the card."
	sourceID := fmt.Sprintf("%s/%s#%d", owner, repo, number)
	if patchErr := dh.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		return patchPRSnapshotDraft(r.Context(), tx.Entities, orgID, sourceID, draft)
	}); patchErr != nil {
		dashboardLog.Warn("failed to patch snapshot after draft toggle", "source_id", sourceID, "error", patchErr)
	}

	writeJSON(w, http.StatusOK, map[string]any{"draft": draft})
}

// patchPRSnapshotDraft flips the is_draft field on an entity's PR snapshot
// in place after a successful external mutation. Best-effort: returns nil
// silently if the entity hasn't been discovered yet (e.g. user mutated
// before the first poll) — the poller will populate it eventually.
// Race window: a concurrent in-flight poll can overwrite our patch with
// its pre-mutation snapshot. Acceptable for beta — the next poll corrects
// it, and the window is small. PatchSnapshot intentionally does NOT bump
// last_polled_at so the next poll still refreshes the row.
func patchPRSnapshotDraft(ctx context.Context, entities db.EntityStore, orgID, sourceID string, draft bool) error {
	entity, err := entities.GetBySource(ctx, orgID, "github", sourceID)
	if err != nil {
		return err
	}
	if entity == nil {
		return nil
	}
	snapshotJSON := strings.TrimSpace(entity.SnapshotJSON)
	if snapshotJSON == "" || snapshotJSON == "{}" {
		return nil
	}
	var snap domain.PRSnapshot
	if err := json.Unmarshal([]byte(snapshotJSON), &snap); err != nil {
		return err
	}
	snap.IsDraft = draft
	patched, err := json.Marshal(snap)
	if err != nil {
		return err
	}
	_, e := entities.PatchSnapshot(ctx, orgID, entity.ID, string(patched))
	return e
}
