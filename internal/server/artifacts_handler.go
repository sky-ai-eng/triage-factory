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
// per repo at call time, so App-only orgs work identically to PAT orgs. spawner
// is read through a getter so the handler always sees the current value, (re)wired
// onto the server after construction. Mirrors pendingPRsHandler's deps.
type artifactsHandler struct {
	tx         db.TxRunner
	ws         *websocket.Hub
	agentRuns  db.AgentRunStore
	ghResolver ghclient.Resolver
	spawner    func() *delegate.Spawner
}

// prArtifactJSON is the wire shape the PR overlay consumes. Title/Body are the
// live values pulled from GitHub (GetPR) so the editor renders the current PR,
// not a stale snapshot; the rest are the artifact's stable coordinates.
type prArtifactJSON struct {
	ID         string `json:"id"`
	RunID      string `json:"run_id,omitempty"`
	Owner      string `json:"owner"`
	Repo       string `json:"repo"`
	Number     int    `json:"number"`
	HeadBranch string `json:"head_branch"`
	BaseBranch string `json:"base_branch"`
	Title      string `json:"title"`
	Body       string `json:"body"`
	URL        string `json:"url"`
	State      string `json:"state"`
}

// resolvePR loads a pull_request artifact by id, validates its kind, parses its
// owner/repo/number coordinates, and resolves a per-repo GitHub client. It
// writes the appropriate error response and returns ok=false on any failure, so
// callers can `if ... ; !ok { return }`.
func (ah *artifactsHandler) resolvePR(w http.ResponseWriter, r *http.Request, orgID, userID, id string) (art *domain.Artifact, gh *ghclient.Client, owner, repo string, number int, ok bool) {
	if err := ah.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		var e error
		art, e = tx.Artifacts.Get(r.Context(), orgID, id)
		return e
	}); err != nil {
		internalError(w, "artifacts", err)
		return nil, nil, "", "", 0, false
	}
	if art == nil || art.Kind != domain.ArtifactKindPullRequest {
		notFound(w, "pull request artifact")
		return nil, nil, "", "", 0, false
	}

	o, rp, n, parseOK := domain.ParsePRTarget(art.Target)
	if !parseOK {
		// A pull_request artifact whose target isn't owner/repo#N can't be acted
		// on (no PR to address). Treat as a server-side data error rather than
		// guessing — the create writer always stamps a well-formed target.
		internalError(w, "artifacts", fmt.Errorf("malformed PR artifact target %q (artifact %s)", art.Target, art.ID))
		return nil, nil, "", "", 0, false
	}

	// Resolve per-repo (org App installation token → PAT): App-only orgs (no PAT)
	// resolve a client here instead of 503-ing on a nil global client.
	client, err := ah.ghResolver.ClientForRepo(r.Context(), orgID, o, rp)
	if err != nil {
		if errors.Is(err, ghclient.ErrNoGitHubCredentials) {
			artifactsLog.Warn("github not configured", "org", orgID, "owner", o, "repo", rp, "error", err)
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "GitHub credentials not configured"})
			return nil, nil, "", "", 0, false
		}
		internalError(w, "artifacts", err)
		return nil, nil, "", "", 0, false
	}
	return art, client, o, rp, n, true
}

// handleArtifactGet returns the PR artifact augmented with the live PR title and
// body fetched from GitHub (1:1 display). On a GetPR failure it degrades to the
// proposed/edited snapshot stored in details_json so the overlay still renders
// (a closed/merged PR or a transient GitHub blip shouldn't blank the editor).
func (ah *artifactsHandler) handleArtifactGet(w http.ResponseWriter, r *http.Request) {
	orgID, ok := requireOrg(w, r)
	if !ok {
		return
	}
	userID := ClaimsFrom(r.Context()).Subject
	id := r.PathValue("id")

	art, gh, owner, repo, number, ok := ah.resolvePR(w, r, orgID, userID, id)
	if !ok {
		return
	}

	details, _ := domain.ParsePRArtifactDetails(art.DetailsJSON)
	title := details.Snapshot.Title
	body := details.Snapshot.Body
	if pr, err := gh.GetPR(owner, repo, number, false); err != nil {
		artifactsLog.Warn("live PR fetch failed; falling back to snapshot",
			"artifact", art.ID, "owner", owner, "repo", repo, "number", number, "error", err)
	} else {
		title = pr.Title
		body = pr.Body
	}

	writeJSON(w, http.StatusOK, prArtifactJSON{
		ID:         art.ID,
		RunID:      art.RunID,
		Owner:      owner,
		Repo:       repo,
		Number:     number,
		HeadBranch: details.HeadBranch,
		BaseBranch: details.Base,
		Title:      title,
		Body:       body,
		URL:        art.URL,
		State:      art.State,
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

	var req struct {
		Title *string `json:"title"`
		Body  *string `json:"body"`
	}
	if !decodeJSON(w, r, &req, "") {
		return
	}

	art, gh, owner, repo, number, ok := ah.resolvePR(w, r, orgID, userID, id)
	if !ok {
		return
	}

	// Whole-field replace: UpdatePR sends both title and body, so a PATCH that
	// touches only one field fills the other from the current snapshot (TF's
	// record, kept current by every prior edit). The snapshot is the baseline,
	// matching the old pending_prs row-as-baseline semantics.
	details, _ := domain.ParsePRArtifactDetails(art.DetailsJSON)
	title := details.Snapshot.Title
	body := details.Snapshot.Body
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
	if err := gh.UpdatePR(owner, repo, number, title, body); err != nil {
		artifactsLog.Warn("UpdatePR failed", "artifact", art.ID, "owner", owner, "repo", repo, "number", number, "error", err)
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "GitHub API error: " + err.Error()})
		return
	}

	// Refresh the artifact's mutable snapshot to the new title/body; proposed
	// stays frozen. Best-effort-but-reported: the GitHub edit already landed, so
	// a snapshot-write failure isn't fatal to the user's edit, but we log it.
	details.Snapshot = domain.PRArtifactSnapshot{Title: title, Body: body}
	if err := ah.upsertPRDetails(r.Context(), orgID, userID, art, details); err != nil {
		artifactsLog.Warn("refresh PR artifact snapshot failed (GitHub edit applied)", "artifact", art.ID, "error", err)
	}

	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// handleArtifactDiff returns the PR's unified diff from GitHub (the PR exists
// now, so there's no bare-clone path). Ports the review-diff 406→per-file
// fallback: GitHub refuses the verbatim diff media type on very large diffs, so
// reassemble from the files API rather than 502-ing the overlay.
func (ah *artifactsHandler) handleArtifactDiff(w http.ResponseWriter, r *http.Request) {
	orgID, ok := requireOrg(w, r)
	if !ok {
		return
	}
	userID := ClaimsFrom(r.Context()).Subject
	id := r.PathValue("id")

	art, gh, owner, repo, number, ok := ah.resolvePR(w, r, orgID, userID, id)
	if !ok {
		return
	}

	file := r.URL.Query().Get("file")
	diff, err := gh.GetPRDiff(owner, repo, number, file)
	truncationNote := ""
	if err != nil {
		if !ghclient.IsHTTP406(err) {
			artifactsLog.Error("PR diff failed",
				"artifact", art.ID, "owner", owner, "repo", repo, "number", number, "error", err)
			writeJSON(w, http.StatusBadGateway, map[string]string{"error": "GitHub API error: " + err.Error()})
			return
		}
		files, filesErr := gh.GetPRFiles(owner, repo, number)
		if filesErr != nil {
			artifactsLog.Error("PR diff fallback failed",
				"artifact", art.ID, "owner", owner, "repo", repo, "number", number, "error", filesErr)
			writeJSON(w, http.StatusBadGateway, map[string]string{"error": "GitHub API error: " + filesErr.Error()})
			return
		}
		if file != "" {
			diff = ghclient.SingleFileDiff(files, file)
			if diff == "" {
				writeJSON(w, http.StatusNotFound, map[string]string{"error": "file " + file + " is not part of this PR's diff (or lies beyond the file-listing cap)"})
				return
			}
		} else {
			diff = ghclient.ReassemblePRDiff(files)
		}
		truncationNote = "diff too large to fetch in full from GitHub; showing per-file patches reassembled from the files API (binary and oversized files may be summarized rather than shown)"
		if len(files) >= ghclient.MaxPRFiles {
			truncationNote += fmt.Sprintf("; only the first %d files are listed", ghclient.MaxPRFiles)
		}
	}

	if truncationNote != "" {
		w.Header().Set("X-Diff-Truncated", truncationNote)
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Write([]byte(diff))
}

// handleArtifactApprove promotes the draft PR to ready-for-review: it appends the
// agentmeta footer to the body (UpdatePR), marks the PR ready (MarkPRReady),
// flips the artifact to state=open, captures the human verdict into run_memory,
// and runs the run-completion / task-close / blueprint-resume bookkeeping — the
// same post-approval flow the old pending_prs submit handler ran, minus the
// CreatePR (the PR already exists).
//
// The two GitHub mutations come first and are pessimistic (non-2xx on failure):
// the footer base is the proposed/edited snapshot WITHOUT a footer, so a retry
// after a partial failure re-appends exactly one footer (idempotent). Everything
// after the GitHub success is detached best-effort bookkeeping — the PR is
// already ready, so a client disconnect must not strand the run/task half-flipped.
func (ah *artifactsHandler) handleArtifactApprove(w http.ResponseWriter, r *http.Request) {
	orgID, ok := requireOrg(w, r)
	if !ok {
		return
	}
	userID := ClaimsFrom(r.Context()).Subject
	id := r.PathValue("id")

	art, gh, owner, repo, number, ok := ah.resolvePR(w, r, orgID, userID, id)
	if !ok {
		return
	}

	details, _ := domain.ParsePRArtifactDetails(art.DetailsJSON)
	finalTitle := details.Snapshot.Title
	finalBody := details.Snapshot.Body

	// Build the final body with footer from the pre-footer snapshot, so a retry
	// after a partial GitHub failure can't double-append.
	footeredBody := finalBody + agentmeta.Build(ah.agentRuns, orgID, art.RunID, "PR")
	if err := gh.UpdatePR(owner, repo, number, finalTitle, footeredBody); err != nil {
		artifactsLog.Warn("approve UpdatePR (footer) failed", "artifact", art.ID, "owner", owner, "repo", repo, "number", number, "error", err)
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "GitHub API error: " + err.Error()})
		return
	}
	if err := gh.MarkPRReady(owner, repo, number); err != nil {
		artifactsLog.Warn("MarkPRReady failed", "artifact", art.ID, "owner", owner, "repo", repo, "number", number, "error", err)
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "GitHub API error: " + err.Error()})
		return
	}

	// Post-success bookkeeping runs detached from r.Context(): the PR is already
	// ready on GitHub, so a client disconnect mustn't leave the run/task in a
	// half-cleaned state. Each step is best-effort + logged (ported from the old
	// handlePendingPRSubmit cleanup), and each gets its own tx so one failure
	// doesn't roll back the others.
	cleanupCtx := context.WithoutCancel(r.Context())

	// Step 1: flip the artifact to open. proposed + snapshot are preserved
	// (details unchanged); only state moves.
	openArt := *art
	openArt.State = domain.ArtifactStatePROpen
	if err := ah.tx.WithTx(cleanupCtx, orgID, userID, func(tx db.TxStores) error {
		_, e := tx.Artifacts.Upsert(cleanupCtx, orgID, openArt)
		return e
	}); err != nil {
		artifactsLog.Warn("flip PR artifact to open failed", "artifact", art.ID, "error", err)
	}

	// Step 2: human verdict capture. Mirrors the submit-time human-verdict block
	// so the next retry of this ticket sees what the human changed vs the agent's draft.
	if art.RunID != "" {
		humanContent := formatPRHumanFeedback(details.Proposed.Title, details.Proposed.Body, finalTitle, finalBody)
		if err := ah.tx.WithTx(cleanupCtx, orgID, userID, func(tx db.TxStores) error {
			return tx.TaskMemory.UpdateRunMemoryHumanContent(cleanupCtx, orgID, art.RunID, humanContent)
		}); err != nil {
			artifactsLog.Warn("failed to record human verdict", "run", art.RunID, "error", err)
		}
	}

	// Step 3: run/task/blueprint bookkeeping — flip the run out of the approval
	// queue, then either resume the blueprint (which closes the task) or close the
	// standalone task. Ported from the old pending-PR submit handler.
	var blueprintRun *domain.BlueprintRun
	if art.RunID != "" {
		if err := ah.tx.WithTx(cleanupCtx, orgID, userID, func(tx db.TxStores) error {
			if _, err := tx.AgentRuns.MarkCompletedIfPendingApproval(cleanupCtx, orgID, art.RunID); err != nil {
				return fmt.Errorf("mark run completed: %w", err)
			}
			// Skip the blanket task-mark-done for blueprint steps;
			// terminateBlueprint owns task closure so the blueprint_runs row
			// finalizes first. A blueprint lookup error leaves the task open for
			// human follow-up rather than racing terminateBlueprint.
			cr, _, blueprintLookupErr := tx.Blueprints.GetRunForRun(cleanupCtx, orgID, art.RunID)
			if blueprintLookupErr != nil {
				artifactsLog.Warn("blueprint lookup failed, skipping task closure", "run", art.RunID, "error", blueprintLookupErr)
				return nil
			}
			blueprintRun = cr
			if cr != nil {
				return nil
			}
			// Stand-alone run: resolve the run's task_id and flip the task to done.
			run, runErr := tx.AgentRuns.Get(cleanupCtx, orgID, art.RunID)
			if runErr != nil || run == nil {
				artifactsLog.Warn("read run for task closure failed", "run", art.RunID, "error", runErr)
				return nil
			}
			if err := tx.Tasks.Close(cleanupCtx, orgID, run.TaskID, "run_completed", ""); err != nil {
				artifactsLog.Warn("failed to close task for run", "run", art.RunID, "error", err)
			}
			return nil
		}); err != nil {
			artifactsLog.Warn("post-approve run bookkeeping failed", "run", art.RunID, "error", err)
		}

		ah.ws.Broadcast(websocket.Event{
			Type:  "agent_run_update",
			OrgID: orgID,
			RunID: art.RunID,
			Data:  map[string]string{"status": "completed"},
		})
		spawner := ah.spawner()
		if blueprintRun != nil && spawner != nil {
			spawner.ResumeBlueprintAfterApproval(orgID, art.RunID, userID)
		} else if spawner != nil {
			// Standalone run: approval is its terminal, so drop the durable
			// workspace snapshot taken when it parked in pending_approval.
			spawner.DiscardWorkspaceSnapshot(orgID, art.RunID)
		}
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"number":   number,
		"html_url": art.URL,
		"state":    domain.ArtifactStatePROpen,
	})
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
// run_memory.human_content when a draft PR is approved. Port of the deleted
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
