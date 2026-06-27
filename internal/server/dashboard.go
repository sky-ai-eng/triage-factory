package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	ghclient "github.com/sky-ai-eng/triage-factory/internal/github"
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

// handleDashboardStats returns aggregated PR statistics from entity snapshots.
func (dh *dashboardHandler) handleDashboardStats(w http.ResponseWriter, r *http.Request) {
	orgID, ok := requireOrg(w, r)
	if !ok {
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
		stats, e = tx.Dashboard.Stats(r.Context(), orgID, username, 30)
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

// handleDashboardPRs returns open PRs from entity snapshots.
func (dh *dashboardHandler) handleDashboardPRs(w http.ResponseWriter, r *http.Request) {
	orgID, ok := requireOrg(w, r)
	if !ok {
		return
	}
	userID := ClaimsFrom(r.Context()).Subject
	var (
		host     string
		username string
		prs      []domain.PRSummaryRow
	)
	if err := dh.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		// Host from org_settings, not a PAT credential — see handleDashboardStats
		// for the TFAC-396 rationale. App-mode / org-PAT orgs have no PAT but do
		// have populated snapshots and a bound identity.
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
		// "", nil) degrades to the empty-dashboard response below. See
		// handleDashboardStats.
		username, lerr = tx.Users.GetGitHubLogin(r.Context(), userID, ghWeb)
		if lerr != nil {
			return lerr
		}
		if username == "" {
			return nil
		}
		var e error
		prs, e = tx.Dashboard.PRs(r.Context(), orgID, username)
		return e
	}); err != nil {
		internalError(w, "dashboard", err)
		return
	}
	if username == "" {
		writeJSON(w, http.StatusOK, []domain.PRSummaryRow{})
		return
	}
	dh.kickBackfill(orgID, userID, username, host)
	if prs == nil {
		prs = []domain.PRSummaryRow{}
	}
	writeJSON(w, http.StatusOK, prs)
}

// handleDashboardPRStatus fetches live CI/review status for a single PR.
// This stays as a live API call since it's on-demand detail, not aggregated data.
func (dh *dashboardHandler) handleDashboardPRStatus(w http.ResponseWriter, r *http.Request) {
	numberStr := r.PathValue("number")
	number, err := strconv.Atoi(numberStr)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid PR number"})
		return
	}

	repoParam := r.URL.Query().Get("repo")
	if repoParam == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "repo query parameter required (owner/repo)"})
		return
	}
	parts := strings.SplitN(repoParam, "/", 2)
	if len(parts) != 2 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "repo must be owner/repo format"})
		return
	}

	orgID, ok := requireOrg(w, r)
	if !ok {
		return
	}

	// Repo-scoped read: decide the credential tier on the whole owner/repo,
	// not just the owner. A "Selected repositories" App install mints a token
	// for any repo under parts[0] but 403s on repos outside the grant, so a
	// bare-owner resolve would skip the PAT that would have worked. ClientForRepo
	// falls through to the PAT when the App doesn't cover this repo.
	client, err := dh.ghResolver.ClientForRepo(r.Context(), orgID, parts[0], parts[1])
	if err != nil {
		if errors.Is(err, ghclient.ErrNoGitHubCredentials) {
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "GitHub not configured"})
			return
		}
		// Real DB/vault/RLS failure — internalError redacts in multi-mode + logs detail.
		internalError(w, "dashboard", err)
		return
	}
	status, err := client.GetPRStatus(r.Context(), parts[0], parts[1], number)
	if err != nil {
		internalError(w, "dashboard", err)
		return
	}

	writeJSON(w, http.StatusOK, status)
}

func (dh *dashboardHandler) handleDashboardPRDraft(w http.ResponseWriter, r *http.Request) {
	numberStr := r.PathValue("number")
	number, err := strconv.Atoi(numberStr)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid PR number"})
		return
	}

	repoParam := r.URL.Query().Get("repo")
	if repoParam == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "repo query parameter required (owner/repo)"})
		return
	}
	parts := strings.SplitN(repoParam, "/", 2)
	if len(parts) != 2 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "repo must be owner/repo"})
		return
	}

	var body struct {
		Draft bool `json:"draft"`
	}
	if !decodeJSON(w, r, &body, "") {
		return
	}

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
	client, err := dh.ghResolver.ClientForRepo(r.Context(), orgID, parts[0], parts[1])
	if err != nil {
		if errors.Is(err, ghclient.ErrNoGitHubCredentials) {
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "GitHub not configured"})
			return
		}
		internalError(w, "dashboard", err)
		return
	}

	if body.Draft {
		err = client.ConvertPRToDraft(r.Context(), parts[0], parts[1], number)
	} else {
		err = client.MarkPRReady(r.Context(), parts[0], parts[1], number)
	}
	if err != nil {
		internalError(w, "dashboard", err)
		return
	}

	// Audit the org-credential board-drag draft toggle (TFAC-483): a
	// human-authorized, org-executed GitHub write. No run (a direct dashboard
	// action, not an agent's) and no team (an org-wide PR with no team context),
	// so it surfaces in the org governance feed, not a team feed. Best-effort in
	// its own tx after the pessimistic GitHub write — never fails the toggle.
	draftAction := domain.ActionPRMarkedReady
	draftFrom, draftTo := domain.ArtifactStatePRDraft, domain.ArtifactStatePROpen
	if body.Draft {
		draftAction = domain.ActionPRConvertedToDraft
		draftFrom, draftTo = domain.ArtifactStatePROpen, domain.ArtifactStatePRDraft
	}
	recordExternalActionBestEffort(r.Context(), dh.tx, orgID, userID, domain.ExternalAction{
		Provider:    domain.ArtifactProviderGitHub,
		Action:      draftAction,
		Target:      fmt.Sprintf("%s/%s#%d", parts[0], parts[1], number),
		ExternalID:  strconv.Itoa(number),
		FromState:   draftFrom,
		ToState:     draftTo,
		ActorUserID: userID,
		Credential:  domain.CredentialGitHubApp,
	})

	// Patch the local entity snapshot to match the state we just pushed to
	// GitHub. Without this, the frontend's subsequent fetchAll() reads the
	// stale pre-mutation snapshot and the card snaps back to its old column
	// until the next poll cycle (up to several minutes later).
	//
	// TODO(SKY-193): we deliberately don't fire a synthetic pr:ready_for_review
	// / pr:converted_to_draft event here — the user's UI click is its own
	// signal and a second event would race the next poll's diff and confuse
	// the audit trail. Revisit if a user reports "my trigger didn't fire
	// when I dragged the card."
	sourceID := fmt.Sprintf("%s/%s#%d", parts[0], parts[1], number)
	if patchErr := dh.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		return patchPRSnapshotDraft(r.Context(), tx.Entities, orgID, sourceID, body.Draft)
	}); patchErr != nil {
		dashboardLog.Warn("failed to patch snapshot after draft toggle", "source_id", sourceID, "error", patchErr)
	}

	writeJSON(w, http.StatusOK, map[string]any{"draft": body.Draft})
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
	return entities.PatchSnapshot(ctx, orgID, entity.ID, string(patched))
}
