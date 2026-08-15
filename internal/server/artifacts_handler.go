package server

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/sky-ai-eng/triage-factory/internal/agentmeta"
	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/delegate"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	ghclient "github.com/sky-ai-eng/triage-factory/internal/github"
	"github.com/sky-ai-eng/triage-factory/pkg/websocket"
)

// artifactsHandler serves the artifact-id-addressed endpoints that back the
// GitHub-native PR preview. It replaces the local pending_prs path:
// the draft PR is a real GitHub object created at `pr create` time and recorded
// as a `pull_request` artifact, so these handlers read/edit/approve that live
// object 1:1 instead of staging a local row.
//
// ghResolver picks the right GitHub client (org App installation token → PAT)
// per repo at call time, so App-only orgs work identically to PAT orgs. Mirrors
// pendingPRsHandler's deps.
type artifactsHandler struct {
	tx         db.TxRunner
	ws         *websocket.Hub
	agentRuns  db.ConversationStore
	ghResolver ghclient.Resolver
	// spawner is a lazy delegation-spawner accessor (wired by Server.routes via a
	// closure over s.spawner) used to feed the drafting agent a <system-note> when
	// a human resolves one of its artifacts (TFAC-493).
	spawner func() *delegate.Spawner
}

// injectArtifactNote feeds the artifact's drafting run the agent-facing
// <system-note> for a just-completed resolution. Fully decoupled from the
// resolution itself: a live run is steered (the actual delivery runs on a
// detached goroutine inside the spawner, so this returns immediately and never
// blocks the response); a terminal/paused run gets nothing here and re-derives
// the same note from the artifact row into its ledger on the next resume. Never
// gates the blueprint (TFAC-379 #2). The artifact passed must carry its
// post-resolution State so the right kind-specific copy is rendered.
func (ah *artifactsHandler) injectArtifactNote(orgID string, a domain.Artifact) {
	if a.ConversationID == "" || ah.spawner == nil {
		return
	}
	sp := ah.spawner()
	if sp == nil {
		return
	}
	sp.InjectArtifactNote(orgID, a.ConversationID, a)
}

// prArtifactJSON is the wire shape the PR overlay consumes. Title/Body are the
// live values pulled from GitHub (GetPRBasic) so the editor renders the current
// PR, not a stale snapshot; the rest are the artifact's stable coordinates.
type prArtifactJSON struct {
	ID             string `json:"id"`
	ConversationID string `json:"conversation_id,omitempty"`
	Owner          string `json:"owner"`
	Repo           string `json:"repo"`
	Number         int    `json:"number"`
	HeadBranch     string `json:"head_branch"`
	BaseBranch     string `json:"base_branch"`
	Title          string `json:"title"`
	Body           string `json:"body"`
	URL            string `json:"url"`
	State          string `json:"state"`
}

// loadArtifact fetches an artifact by id and 404s if missing. It is the shared
// first step of every artifact-id-addressed route; the caller then dispatches on
// art.Kind (pull_request vs review). Returns ok=false (after writing the error
// response) so callers can `if ...; !ok { return }`.
func (ah *artifactsHandler) loadArtifact(w http.ResponseWriter, r *http.Request, orgID, userID, id string) (art *domain.Artifact, ok bool) {
	if err := ah.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		var e error
		art, e = tx.Artifacts.Get(r.Context(), orgID, id)
		return e
	}); err != nil {
		internalError(w, "artifacts", err)
		return nil, false
	}
	if art == nil {
		notFound(w, "artifact")
		return nil, false
	}
	return art, true
}

// ghForArtifact parses an artifact's owner/repo/number coordinates (PR and review
// artifacts both target owner/repo#<number>) and resolves a per-repo GitHub
// client. Writes the error response and returns ok=false on any failure.
func (ah *artifactsHandler) ghForArtifact(w http.ResponseWriter, r *http.Request, orgID string, art *domain.Artifact) (gh *ghclient.Client, owner, repo string, number int, ok bool) {
	o, rp, n, parseOK := domain.ParsePRTarget(art.Target)
	if !parseOK {
		// A PR/review artifact whose target isn't owner/repo#N can't be acted on
		// (no PR to address). Treat as a server-side data error rather than
		// guessing — the create writer always stamps a well-formed target.
		internalError(w, "artifacts", fmt.Errorf("malformed artifact target %q (artifact %s)", art.Target, art.ID))
		return nil, "", "", 0, false
	}

	// Resolve per-repo (org App installation token → PAT): App-only orgs (no PAT)
	// resolve a client here instead of 503-ing on a nil global client.
	client, err := ah.ghResolver.ClientForRepo(r.Context(), orgID, o, rp)
	if err != nil {
		if errors.Is(err, ghclient.ErrNoGitHubCredentials) {
			artifactsLog.Warn("github not configured", "org", orgID, "owner", o, "repo", rp, "error", err)
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "GitHub credentials not configured"})
			return nil, "", "", 0, false
		}
		internalError(w, "artifacts", err)
		return nil, "", "", 0, false
	}
	return client, o, rp, n, true
}

// handleArtifactGet returns the PR artifact augmented with the live PR title and
// body fetched from GitHub (1:1 display). On a live-fetch failure it degrades to
// the proposed/edited snapshot stored in details_json so the overlay still renders
// (a closed/merged PR or a transient GitHub blip shouldn't blank the editor).
func (ah *artifactsHandler) handleArtifactGet(w http.ResponseWriter, r *http.Request) {
	orgID, ok := requireOrg(w, r)
	if !ok {
		return
	}
	userID := ClaimsFrom(r.Context()).Subject
	id := r.PathValue("id")

	art, ok := ah.loadArtifact(w, r, orgID, userID, id)
	if !ok {
		return
	}
	switch art.Kind {
	case domain.ArtifactKindReview:
		ah.reviewGet(w, r, orgID, art)
		return
	case domain.ArtifactKindPullRequest:
		// fall through to the PR path below
	default:
		notFound(w, "artifact")
		return
	}
	gh, owner, repo, number, ok := ah.ghForArtifact(w, r, orgID, art)
	if !ok {
		return
	}

	details, _ := domain.ParsePRArtifactDetails(art.DetailsJSON)
	title := details.Snapshot.Title
	body := details.Snapshot.Body
	if pr, err := gh.GetPRBasic(r.Context(), owner, repo, number); err != nil {
		artifactsLog.Warn("live PR fetch failed; falling back to snapshot",
			"artifact", art.ID, "owner", owner, "repo", repo, "number", number, "error", err)
	} else {
		title = pr.Title
		body = pr.Body
	}

	writeJSON(w, http.StatusOK, prArtifactJSON{
		ID:             art.ID,
		ConversationID: art.ConversationID,
		Owner:          owner,
		Repo:           repo,
		Number:         number,
		HeadBranch:     details.HeadBranch,
		BaseBranch:     details.Base,
		Title:          title,
		Body:           body,
		URL:            art.URL,
		State:          art.State,
	})
}

// handleArtifactUpdate edits the live PR's title/body 1:1 via UpdatePR and
// refreshes the artifact's details_json snapshot. Pessimistic by contract: a
// GitHub failure returns non-2xx and leaves the snapshot untouched, so the
// frontend never shows a green "saved" over a write GitHub rejected. The proposed
// snapshot (the agent's draft) is preserved — only the mutable snapshot moves.
func (ah *artifactsHandler) handleArtifactUpdate(w http.ResponseWriter, r *http.Request) {
	orgID, ok := requireOrg(w, r)
	if !ok {
		return
	}
	userID := ClaimsFrom(r.Context()).Subject
	id := r.PathValue("id")

	art, ok := ah.loadArtifact(w, r, orgID, userID, id)
	if !ok {
		return
	}
	switch art.Kind {
	case domain.ArtifactKindReview:
		ah.reviewUpdate(w, r, orgID, userID, art)
		return
	case domain.ArtifactKindPullRequest:
		// fall through to the PR path below
	default:
		notFound(w, "artifact")
		return
	}

	var req struct {
		Title *string `json:"title"`
		Body  *string `json:"body"`
	}
	if !decodeJSON(w, r, &req, "") {
		return
	}

	gh, owner, repo, number, ok := ah.ghForArtifact(w, r, orgID, art)
	if !ok {
		return
	}

	// Whole-field replace: UpdatePR rewrites both title and body, so a PATCH that
	// touches only one field must fill the other with the PR's CURRENT live value,
	// not a cached snapshot. Using the snapshot would silently revert a field the
	// human (or anyone) edited directly on GitHub since we last cached it — a lost
	// update. We only need the live read when a field is omitted; if the client
	// sent both, skip the round-trip.
	details, derr := domain.ParsePRArtifactDetails(art.DetailsJSON)
	var title, body string
	if req.Title == nil || req.Body == nil {
		live, err := gh.GetPRBasic(r.Context(), owner, repo, number)
		if err != nil {
			// We can't reconstruct the omitted field without the current PR state,
			// and guessing from a stale snapshot risks clobbering. Fail loudly.
			artifactsLog.Warn("GetPR for partial-edit baseline failed", "artifact", art.ID, "owner", owner, "repo", repo, "number", number, "error", err)
			writeJSON(w, http.StatusBadGateway, map[string]string{"error": "couldn't read the current PR to apply a partial edit" + localDetail(err)})
			return
		}
		title = live.Title
		body = live.Body
	}
	if req.Title != nil {
		title = *req.Title
	}
	if req.Body != nil {
		body = *req.Body
	}
	// Trim before the empty check: GitHub rejects whitespace-only titles at the
	// API anyway, and silently letting "   " through means the user only finds
	// out at approval. Fail fast and store the trimmed value.
	title = strings.TrimSpace(title)
	if title == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "title cannot be empty or whitespace-only"})
		return
	}

	// Live edit FIRST (pessimistic): if GitHub rejects it, surface the error and
	// don't move the snapshot. liftValidationErr inside UpdatePR turns a 422 into
	// a readable reason.
	if err := gh.UpdatePR(r.Context(), owner, repo, number, title, body); err != nil {
		artifactsLog.Warn("UpdatePR failed", "artifact", art.ID, "owner", owner, "repo", repo, "number", number, "error", err)
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "GitHub API error" + localDetail(err)})
		return
	}

	// Audit the org-credential PR edit (TFAC-483). The GitHub write already landed
	// (pessimistic); record best-effort in its own tx so it fires regardless of
	// the snapshot refresh below (which is itself best-effort and skipped when
	// details don't parse). No state transition — it's an in-place title/body edit.
	recordExternalActionBestEffort(r.Context(), ah.tx, orgID, userID,
		githubApprovalAction(art, userID, domain.ActionPREdited, "", "",
			githubCredentialFor(r.Context(), ah.ghResolver, orgID, owner, repo)))

	// Refresh the artifact's mutable snapshot to the new title/body; proposed
	// stays frozen. Best-effort-but-reported: the GitHub edit already landed, so
	// a snapshot-write failure isn't fatal to the user's edit, but we log it.
	// Skip when details didn't parse — re-marshaling a zero-value details would
	// blank the proposed (agent draft) baseline the approve diff needs.
	if derr != nil {
		artifactsLog.Warn("PR artifact details unparseable; skipping snapshot refresh after edit", "artifact", art.ID, "error", derr)
	} else {
		details.Snapshot = domain.PRArtifactSnapshot{Title: title, Body: body}
		if err := ah.upsertPRDetails(r.Context(), orgID, userID, art, details); err != nil {
			artifactsLog.Warn("refresh PR artifact snapshot failed (GitHub edit applied)", "artifact", art.ID, "error", err)
		}
	}

	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// handleArtifactDiff returns the unified diff the overlay renders. A PR artifact
// (and a legacy/comment-less review with no stored frame) renders the LIVE PR diff
// at its current head. A review artifact with a stored finalize frame renders that
// frame — FinalizedBaseSHA...FinalizedHeadSHA, the PR diff as of finalize — so
// every staged comment anchors in the frame it was written against (TFAC-500);
// drift since is conveyed by the per-comment freshness badges + the
// commits-since-finalize count, and reconciled by Refresh. Both frames port the
// 406→per-file fallback: GitHub refuses the verbatim diff media type on very large
// diffs, so reassemble from the files API rather than 502-ing the overlay.
func (ah *artifactsHandler) handleArtifactDiff(w http.ResponseWriter, r *http.Request) {
	orgID, ok := requireOrg(w, r)
	if !ok {
		return
	}
	userID := ClaimsFrom(r.Context()).Subject
	id := r.PathValue("id")

	art, ok := ah.loadArtifact(w, r, orgID, userID, id)
	if !ok {
		return
	}
	if art.Kind != domain.ArtifactKindPullRequest && art.Kind != domain.ArtifactKindReview {
		notFound(w, "artifact")
		return
	}
	gh, owner, repo, number, ok := ah.ghForArtifact(w, r, orgID, art)
	if !ok {
		return
	}

	file := r.URL.Query().Get("file")
	finBase, finHead := reviewFinalizeFrame(art)
	useFin := finBase != "" && finHead != ""

	var (
		diff           string
		truncationNote string
		err            error
	)
	switch {
	case useFin && file != "":
		// The compare endpoint has no built-in single-file filter, so narrow via the
		// files API for a single-file request in the finalize frame.
		var files []ghclient.PRFile
		if files, err = gh.GetCompareFiles(r.Context(), owner, repo, finBase, finHead); err == nil {
			if diff = ghclient.SingleFileDiff(files, file); diff == "" {
				writeJSON(w, http.StatusNotFound, map[string]string{"error": "file " + file + " is not part of this review's diff (or lies beyond the file-listing cap)"})
				return
			}
		}
	case useFin:
		if diff, err = gh.GetCompareDiff(r.Context(), owner, repo, finBase, finHead); err != nil && ghclient.IsHTTP406(err) {
			var files []ghclient.PRFile
			if files, err = gh.GetCompareFiles(r.Context(), owner, repo, finBase, finHead); err == nil {
				diff = ghclient.ReassemblePRDiff(files)
				truncationNote = diffTruncationNote(len(files))
			}
		}
	default:
		if diff, err = gh.GetPRDiff(r.Context(), owner, repo, number, file); err != nil && ghclient.IsHTTP406(err) {
			var files []ghclient.PRFile
			if files, err = gh.GetPRFiles(r.Context(), owner, repo, number); err == nil {
				if file != "" {
					if diff = ghclient.SingleFileDiff(files, file); diff == "" {
						writeJSON(w, http.StatusNotFound, map[string]string{"error": "file " + file + " is not part of this PR's diff (or lies beyond the file-listing cap)"})
						return
					}
				} else {
					diff = ghclient.ReassemblePRDiff(files)
				}
				truncationNote = diffTruncationNote(len(files))
			}
		}
	}
	if err != nil {
		artifactsLog.Error("artifact diff failed",
			"artifact", art.ID, "owner", owner, "repo", repo, "number", number, "finalize_frame", useFin, "error", err)
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "GitHub API error" + localDetail(err)})
		return
	}

	if truncationNote != "" {
		w.Header().Set("X-Diff-Truncated", truncationNote)
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Write([]byte(diff))
}

// reviewFinalizeFrame returns the stored finalize-time base+head SHAs for a review
// artifact (TFAC-500), or ("","") for a PR artifact, a malformed/legacy review
// with no stored frame, or a comment-less review (which records no frame). Both
// must be present for the caller to render the finalize frame.
func reviewFinalizeFrame(art *domain.Artifact) (base, head string) {
	if art.Kind != domain.ArtifactKindReview {
		return "", ""
	}
	d, err := domain.ParseReviewArtifactDetails(art.DetailsJSON)
	if err != nil {
		return "", ""
	}
	return d.FinalizedBaseSHA, d.FinalizedHeadSHA
}

// diffTruncationNote is the X-Diff-Truncated banner shown when a diff was
// reassembled from the per-file patches API (GitHub refused the verbatim diff as
// too large), noting the file-listing cap when hit.
func diffTruncationNote(fileCount int) string {
	note := "diff too large to fetch in full from GitHub; showing per-file patches reassembled from the files API (binary and oversized files may be summarized rather than shown)"
	if fileCount >= ghclient.MaxPRFiles {
		note += fmt.Sprintf("; only the first %d files are listed", ghclient.MaxPRFiles)
	}
	return note
}

// handleArtifactApprove promotes the draft PR to ready-for-review: it appends the
// agentmeta footer to the body (UpdatePR), marks the PR ready (MarkPRReady),
// flips the artifact to state=open, and captures the human verdict into
// conversation_memory. Approval is a decoupled sidecar — it does NOT flip run status or
// resume/terminate a blueprint; the only lifecycle effect is the shared
// terminal-on-last task-closure check (closeTaskIfTerminalAndResolved), which
// closes the task iff this was the last unresolved artifact on a cleanly-completed
// run.
//
// The content promoted is read LIVE from GitHub (never the cached snapshot), so a
// stale or malformed snapshot can neither revert a direct GitHub edit nor write an
// empty body that wipes the PR. The footer is appended idempotently (existing one
// stripped first), so a retry after a partial failure leaves exactly one. The two
// GitHub mutations come first and are pessimistic (non-2xx on failure); everything
// after is detached best-effort bookkeeping — the PR is already ready, so a client
// disconnect must not strand the run/task half-flipped.
func (ah *artifactsHandler) handleArtifactApprove(w http.ResponseWriter, r *http.Request) {
	orgID, ok := requireOrg(w, r)
	if !ok {
		return
	}
	userID := ClaimsFrom(r.Context()).Subject
	id := r.PathValue("id")

	art, ok := ah.loadArtifact(w, r, orgID, userID, id)
	if !ok {
		return
	}
	switch art.Kind {
	case domain.ArtifactKindReview:
		ah.reviewApprove(w, r, orgID, userID, art)
		return
	case domain.ArtifactKindPullRequest:
		// fall through to the PR path below
	default:
		notFound(w, "artifact")
		return
	}
	gh, owner, repo, number, ok := ah.ghForArtifact(w, r, orgID, art)
	if !ok {
		return
	}

	// Approval only makes sense on a draft awaiting it. A stale/double "Open PR"
	// click on an already-open or closed artifact would otherwise re-run the
	// GitHub mutations — a spurious footer rewrite (new timestamp/cost) and a
	// no-op MarkPRReady — so reject it as a conflict. The state transition is
	// gated here rather than in resolvePR, which the read paths (GET/diff) share
	// and must keep serving non-draft PRs.
	if art.State != domain.ArtifactStatePRDraft {
		writeJSON(w, http.StatusConflict, map[string]string{"error": "this PR is no longer a draft awaiting approval (state: " + art.State + ")"})
		return
	}

	// Parse the artifact details for the proposed (agent-draft) baseline the
	// human-verdict memory diffs against. A parse failure is non-fatal: we still
	// promote the PR from its LIVE content (below) and only skip the verdict diff.
	details, derr := domain.ParsePRArtifactDetails(art.DetailsJSON)
	if derr != nil {
		artifactsLog.Warn("PR artifact details unparseable; promoting from live PR and skipping the verdict diff", "artifact", art.ID, "error", derr)
	}

	// Source the promoted content from the LIVE PR, never the cached snapshot: a
	// stale snapshot would revert a direct-on-GitHub edit, and a malformed one
	// would yield empty title/body that UpdatePR would write over the PR. GetPR
	// failure is fatal here — we can't safely promote what we can't read.
	live, err := gh.GetPRBasic(r.Context(), owner, repo, number)
	if err != nil {
		artifactsLog.Warn("approve GetPR failed", "artifact", art.ID, "owner", owner, "repo", repo, "number", number, "error", err)
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "couldn't read the PR to promote it" + localDetail(err)})
		return
	}
	finalTitle := live.Title
	finalBody := live.Body

	// Append the footer idempotently: strip any footer a prior (partially failed)
	// approve already added before re-appending, so a retry can't stack footers.
	footeredBody := agentmeta.StripFooter(finalBody) + agentmeta.Build(ah.agentRuns, orgID, art.ConversationID, "PR")
	if err := gh.UpdatePR(r.Context(), owner, repo, number, finalTitle, footeredBody); err != nil {
		artifactsLog.Warn("approve UpdatePR (footer) failed", "artifact", art.ID, "owner", owner, "repo", repo, "number", number, "error", err)
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "GitHub API error" + localDetail(err)})
		return
	}
	if err := gh.MarkPRReady(r.Context(), owner, repo, number); err != nil {
		artifactsLog.Warn("MarkPRReady failed", "artifact", art.ID, "owner", owner, "repo", repo, "number", number, "error", err)
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "GitHub API error" + localDetail(err)})
		return
	}

	// Post-success bookkeeping runs detached from r.Context(): the PR is already
	// ready on GitHub, so a client disconnect mustn't leave the run/task in a
	// half-cleaned state. Each step is best-effort + logged, and each gets its own
	// tx so one failure doesn't roll back the others.
	cleanupCtx := context.WithoutCancel(r.Context())

	// Step 1: flip the artifact to open and refresh its snapshot to the promoted
	// (pre-footer) content. proposed stays frozen. When details didn't parse we
	// still flip the state but leave the (malformed) details rather than blanking
	// them.
	//
	// The credential is classified before the tx opens — the classification can
	// reach GitHub, and the tx the row composes into must not wait on a network
	// round trip.
	credential := githubCredentialFor(cleanupCtx, ah.ghResolver, orgID, owner, repo)
	openArt := *art
	openArt.State = domain.ArtifactStatePROpen
	if derr == nil {
		details.Snapshot = domain.PRArtifactSnapshot{Title: finalTitle, Body: finalBody}
		openArt.DetailsJSON = domain.MarshalPRArtifactDetails(details)
	}
	// Compose the audit row with the flip (TFAC-483): the org-App MarkPRReady is a
	// human-authorized, org-executed write — run_id is the drafting run, actor is
	// the approver. Recording inside the flip tx keeps the audit row and the
	// artifact state atomic (both land or neither).
	if err := ah.tx.WithTx(cleanupCtx, orgID, userID, func(tx db.TxStores) error {
		if _, e := tx.Artifacts.Upsert(cleanupCtx, orgID, openArt); e != nil {
			return e
		}
		return tx.ExternalActions.Record(cleanupCtx, orgID,
			githubApprovalAction(art, userID, domain.ActionPRMarkedReady, domain.ArtifactStatePRDraft, domain.ArtifactStatePROpen, credential))
	}); err != nil {
		artifactsLog.Warn("flip PR artifact to open + record action failed", "artifact", art.ID, "error", err)
	}

	// Step 2: human verdict capture — only when we recovered the agent's proposed
	// draft (details parsed); without it there's no honest baseline to diff the
	// human's final against, so we skip rather than fabricate a "was empty" diff.
	if art.ConversationID != "" && derr == nil {
		humanContent := formatPRHumanFeedback(details.Proposed.Title, details.Proposed.Body, finalTitle, finalBody)
		if err := ah.tx.WithTx(cleanupCtx, orgID, userID, func(tx db.TxStores) error {
			return tx.TaskMemory.UpdateRunMemoryHumanContent(cleanupCtx, orgID, art.ConversationID, humanContent)
		}); err != nil {
			artifactsLog.Warn("failed to record human verdict", "run", art.ConversationID, "error", err)
		}
	}

	// Step 3: terminal-on-last task closure. Approval is a decoupled sidecar — it
	// never flips run status or resumes/terminates a blueprint. The only lifecycle
	// effect is closing the task when this was the LAST unresolved artifact on an
	// already-terminal blueprint (§3); otherwise a no-op.
	ah.closeTaskIfTerminalAndResolved(cleanupCtx, orgID, userID, art.ConversationID)

	// Step 4: tell the drafting agent its PR was approved — live if the run is
	// warm, else via its ledger on resume. Decoupled from the resolution above.
	// Note the one carve-out to the ledger's "never miss" property: if the
	// best-effort flip in step 1 failed (logged above), the artifact row stays at
	// state=draft, so a terminal-run resume can't re-derive this note — the live
	// steer is then the only delivery. That double-failure (flip failed AND no warm
	// process) drops the note, which is acceptable here: the GitHub PR is already
	// open (the authoritative fact), and the un-flipped draft has bigger problems
	// than the note (it also blocks terminal-on-last closure until reconciliation).
	ah.injectArtifactNote(orgID, openArt)

	writeJSON(w, http.StatusOK, map[string]any{
		"number":   number,
		"html_url": art.URL,
		"state":    domain.ArtifactStatePROpen,
	})
}

// handleArtifactDismiss resolves ONE artifact as a decoupled sidecar operation,
// the per-item counterpart to approve: it abandons the GitHub object and flips
// the artifact's state, never touching the run's lifecycle. A draft PR is closed
// on GitHub (best-effort) and flipped to closed; a pending review has its GitHub
// pending review deleted and is flipped to dismissed. Branches are never touched.
// An already-terminal artifact (a non-draft PR / non-pending review) is a 409 —
// there is nothing left to resolve. After the flip it runs the shared
// terminal-on-last task-closure check.
func (ah *artifactsHandler) handleArtifactDismiss(w http.ResponseWriter, r *http.Request) {
	orgID, ok := requireOrg(w, r)
	if !ok {
		return
	}
	userID := ClaimsFrom(r.Context()).Subject
	id := r.PathValue("id")

	art, ok := ah.loadArtifact(w, r, orgID, userID, id)
	if !ok {
		return
	}
	switch art.Kind {
	case domain.ArtifactKindReview:
		ah.reviewDismiss(w, r, orgID, userID, art)
		return
	case domain.ArtifactKindPullRequest:
		// fall through to the PR path below
	default:
		notFound(w, "artifact")
		return
	}

	// Only a draft PR can be dismissed. An already-open / merged / closed PR is
	// terminal — there's no draft to abandon — so 409 rather than re-closing a PR
	// the user already shipped or a prior dismiss already retired.
	if art.State != domain.ArtifactStatePRDraft {
		writeJSON(w, http.StatusConflict, map[string]string{"error": "this PR is no longer a draft awaiting resolution (state: " + art.State + ")"})
		return
	}

	// Flip the artifact to closed + audit, atomic and pessimistic (the flip is the
	// resolution, so a DB failure is a real error the client must see). The GitHub
	// close + the terminal-on-last check run detached afterwards so a client
	// disconnect can't strand them once the artifact is resolved.
	//
	// The credential is classified before the tx: the GitHub close runs after it,
	// but the row records the credential that close is made under, and a probe
	// inside the tx would hold it open across a round trip.
	credential := githubCredentialForArtifact(r.Context(), ah.ghResolver, orgID, art)
	closed := *art
	closed.State = domain.ArtifactStatePRClosed
	if err := ah.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		if _, e := tx.Artifacts.Upsert(r.Context(), orgID, closed); e != nil {
			return e
		}
		return tx.ExternalActions.Record(r.Context(), orgID,
			githubApprovalAction(art, userID, domain.ActionPRClosed, domain.ArtifactStatePRDraft, domain.ArtifactStatePRClosed, credential))
	}); err != nil {
		internalError(w, "artifacts", err)
		return
	}

	cleanupCtx := context.WithoutCancel(r.Context())
	// Close the draft PR on GitHub (best-effort). The artifact is already closed; a
	// GitHub hiccup leaves the PR for reconciliation to retire later. Branch kept.
	closeDraftPRBestEffort(cleanupCtx, ah.ghResolver, orgID, art)
	ah.closeTaskIfTerminalAndResolved(cleanupCtx, orgID, userID, art.ConversationID)
	// Tell the drafting agent its draft PR was dismissed (live or via the ledger).
	ah.injectArtifactNote(orgID, closed)

	writeJSON(w, http.StatusOK, map[string]any{
		"number":   artifactPRNumber(art),
		"html_url": art.URL,
		"state":    domain.ArtifactStatePRClosed,
	})
}

// artifactPRNumber parses the PR number from an artifact's target for the dismiss
// response. Returns 0 on a malformed target — the field is informational (the
// artifact is already resolved), so it never blocks the success response.
func artifactPRNumber(art *domain.Artifact) int {
	_, _, n, _ := domain.ParsePRTarget(art.Target)
	return n
}

// closeTaskIfTerminalAndResolved is the shared terminal-on-last task-closure
// check, run after any per-artifact resolve (approve or dismiss). It re-reads the
// artifact set governing the run and closes the task IFF the controlling run
// reached a CLEAN completion AND no unresolved artifact remains. Otherwise it
// no-ops:
//
//   - a LIVE run keeps running — resolving an artifact never closes its task; the
//     run's own eventual termination (terminateBlueprint / standalone completion)
//     re-checks this and closes the task then.
//   - a cleanly-completed run with other unresolved artifacts stays in the approval
//     column; the last resolution is what closes it.
//   - an ABORTED / FAILED / CANCELLED run leaves the task open for human attention
//     regardless of artifact state — mirroring terminateBlueprint (blueprint.go)
//     and processCompletion (run.go), which close the task only on a clean finish.
//     Resolving a stray draft PR on an aborted blueprint must not override the
//     "needs a human" disposition.
//
// No accept/dismiss distinction — the task closes on the last resolution
// regardless of whether anything was accepted (epic decision #3). This is the
// ONLY lifecycle effect of a resolve: it never flips conversations.status or
// resumes/terminates a blueprint.
//
// Governing signals: only a blueprint run is a task run, so only it can close a
// task — clean completion is its blueprint run reaching status=completed. A
// non-blueprint run (origin <> 'blueprint' — a future interactive/ad-hoc run) is
// task-less and is a no-op here. The "anything still unresolved?" check is scoped
// to the whole TASK (all its runs), matching teardownTaskArtifacts, so a stranded
// artifact from a prior attempt blocks closure.
//
// Detached + best-effort: the caller already mutated the GitHub object and flipped
// the artifact, so a failure here must not unwind that. Fails CLOSED on the close
// direction — any read error leaves the task open (recoverable; the next resolve
// or run termination re-checks) rather than closing a task that may still hold
// unresolved work.
func (ah *artifactsHandler) closeTaskIfTerminalAndResolved(ctx context.Context, orgID, userID, runID string) {
	if runID == "" {
		return
	}
	var (
		taskID        string
		closeEligible bool
		unresolved    bool
	)
	if err := ah.tx.WithTx(ctx, orgID, userID, func(tx db.TxStores) error {
		// Step 1: only a blueprint run is a task run, so only it can drive
		// terminal-on-last task closure. A non-blueprint run (origin <> 'blueprint'
		// — a future interactive/ad-hoc run) is task-less by construction (NULL
		// task_id), so there is nothing to close: leave taskID empty and no-op below.
		br, _, bpErr := tx.Blueprints.GetRunForRun(ctx, orgID, runID)
		if bpErr != nil {
			return fmt.Errorf("blueprint lookup: %w", bpErr)
		}
		if br == nil {
			return nil
		}
		// A clean blueprint finalization is status=completed; an abort/fail/cancel
		// maps to a non-completed terminal and leaves the task open for a human.
		taskID = br.TaskID
		closeEligible = br.Status == domain.BlueprintRunStatusCompleted
		if !closeEligible {
			return nil // no need to scan artifacts if we can't close anyway
		}
		// Step 2: unresolved check scoped to the TASK (all its runs), matching
		// teardownTaskArtifacts — not just the current blueprint's step runs. A
		// stranded artifact from a prior attempt (e.g. a teardown that partially
		// failed on a network blip) must block closure rather than be silently
		// skipped, so we never close the task with an unresolved draft PR / pending
		// review sitting on GitHub.
		runs, e := tx.Conversations.ListForTask(ctx, orgID, taskID)
		if e != nil {
			return fmt.Errorf("list runs for task: %w", e)
		}
		has, e := runsHaveUnresolvedArtifacts(ctx, tx, orgID, runs)
		if e != nil {
			return e
		}
		unresolved = has
		return nil
	}); err != nil {
		artifactsLog.Warn("terminal-on-last close check failed; leaving task open (fail closed)", "run", runID, "error", err)
		return
	}
	if taskID == "" || !closeEligible || unresolved {
		return
	}

	// Close in its own tx. Tasks.Close is idempotent (UPDATE ... WHERE status NOT IN
	// ('done','dismissed')), so two concurrent resolves that both pass the gate
	// above race harmlessly — the second close is a 0-row no-op (worst case a
	// duplicate task_updated broadcast), not an error.
	if err := ah.tx.WithTx(ctx, orgID, userID, func(tx db.TxStores) error {
		return tx.Tasks.Close(ctx, orgID, taskID, "run_completed", "")
	}); err != nil {
		artifactsLog.Warn("terminal-on-last task close failed", "task", taskID, "run", runID, "error", err)
		return
	}
	// Move the card to Done on peer boards without a refetch.
	ah.ws.Broadcast(websocket.Event{
		Type:  "task_updated",
		OrgID: orgID,
		Data:  map[string]any{"task_id": taskID, "status": "done"},
	})
}

// runsHaveUnresolvedArtifacts reports whether any of the given runs still holds an
// unresolved artifact (a draft PR or a ready review — domain.HasUnresolvedArtifacts),
// reading each run's artifacts under the caller's tx. One query per run, bounded
// by a blueprint's step count. Shared by the terminal-on-last check so the
// "blueprint still has unresolved work" definition lives next to its single use.
func runsHaveUnresolvedArtifacts(ctx context.Context, tx db.TxStores, orgID string, runs []domain.Conversation) (bool, error) {
	for i := range runs {
		arts, err := tx.Artifacts.ListByRun(ctx, orgID, runs[i].ID)
		if err != nil {
			return false, fmt.Errorf("artifacts.ListByRun(%s): %w", runs[i].ID, err)
		}
		if domain.HasUnresolvedArtifacts(arts) {
			return true, nil
		}
	}
	return false, nil
}

// upsertPRDetails writes the artifact back with new details_json, inside a
// claims-set tx (app pool / RLS). State and coordinates are carried unchanged
// from the existing row; only DetailsJSON moves.
func (ah *artifactsHandler) upsertPRDetails(ctx context.Context, orgID, userID string, art *domain.Artifact, details domain.PRArtifactDetails) error {
	next := *art
	next.DetailsJSON = domain.MarshalPRArtifactDetails(details)
	return ah.tx.WithTx(ctx, orgID, userID, func(tx db.TxStores) error {
		_, e := tx.Artifacts.Upsert(ctx, orgID, next)
		return e
	})
}

// formatPRHumanFeedback builds the markdown block written to
// conversation_memory.human_content when a draft PR is approved. Port of the deleted
// FormatHumanFeedbackPR (pending_pr_diff.go) onto the artifact's snapshot model:
// the agent's first draft is the proposed snapshot (always present in
// details_json, so no nil-vs-empty pointer dance), and the final values are the
// snapshot at approval. Reuses writeBlockquote from review_diff.go.
//
// No leading "## Human feedback (post-run)" heading: db.materializeMemory
// prepends it when joining agent_content + human_content, so baking it in here
// would double the heading. Mirrors FormatHumanFeedback.
func formatPRHumanFeedback(proposedTitle, proposedBody, finalTitle, finalBody string) string {
	var b strings.Builder

	titleChanged := proposedTitle != finalTitle
	bodyChanged := proposedBody != finalBody

	if titleChanged || bodyChanged {
		b.WriteString("**Outcome:** Human submitted the PR with edits.\n\n")
	} else {
		b.WriteString("**Outcome:** Human submitted the PR as drafted.\n\n")
	}

	if titleChanged {
		fmt.Fprintf(&b, "**Title:** edited\n- Was: %s\n- Now: %s\n\n", proposedTitle, finalTitle)
	}

	if bodyChanged {
		b.WriteString("**Body:** edited\n\n")
		b.WriteString("Originally drafted as:\n\n")
		writeBlockquote(&b, proposedBody)
		b.WriteString("\nFinal:\n\n")
		writeBlockquote(&b, finalBody)
	}

	return b.String()
}
