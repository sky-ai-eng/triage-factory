package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/sky-ai-eng/triage-factory/internal/auth"
	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	ghclient "github.com/sky-ai-eng/triage-factory/internal/github"
	"github.com/sky-ai-eng/triage-factory/internal/integrations"
)

// dashboardHandler serves the personal dashboard endpoints: PR stats, the
// user's open PRs, and live per-PR status / draft toggles. It holds the
// transactional store runner and the GitHub credential resolver the live
// per-PR calls use; routes() registers its methods through the
// api()/apiMutating() middleware wrappers.
type dashboardHandler struct {
	tx         db.TxRunner
	ghResolver ghclient.Resolver
}

// handleDashboardStats returns aggregated PR statistics from entity snapshots.
func (dh *dashboardHandler) handleDashboardStats(w http.ResponseWriter, r *http.Request) {
	orgID, ok := requireOrg(w, r)
	if !ok {
		return
	}
	userID := ClaimsFrom(r.Context()).Subject
	var (
		creds    auth.Credentials
		username string
		stats    *domain.DashboardStats
	)
	if err := dh.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		var lerr error
		creds, lerr = integrations.Load(r.Context(), tx.Secrets, orgID)
		if lerr != nil || creds.GitHubPAT == "" {
			return nil
		}
		// Identity is host-scoped (SKY-396): the relevant login is the
		// one bound to the org's GitHub host (creds.GitHubURL, which
		// mirrors org_settings.github_base_url).
		username, _ = tx.Users.GetGitHubLogin(r.Context(), userID, creds.GitHubURL)
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
		creds    auth.Credentials
		username string
		prs      []domain.PRSummaryRow
	)
	if err := dh.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		var lerr error
		creds, lerr = integrations.Load(r.Context(), tx.Secrets, orgID)
		if lerr != nil || creds.GitHubPAT == "" {
			return nil
		}
		// Identity is host-scoped (SKY-396): the relevant login is the
		// one bound to the org's GitHub host (creds.GitHubURL, which
		// mirrors org_settings.github_base_url).
		username, _ = tx.Users.GetGitHubLogin(r.Context(), userID, creds.GitHubURL)
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
	status, err := client.GetPRStatus(parts[0], parts[1], number)
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
		err = client.ConvertPRToDraft(parts[0], parts[1], number)
	} else {
		err = client.MarkPRReady(parts[0], parts[1], number)
	}
	if err != nil {
		internalError(w, "dashboard", err)
		return
	}

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
