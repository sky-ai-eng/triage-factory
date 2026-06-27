package server

import (
	"context"
	"fmt"
	"net/http"

	"github.com/sky-ai-eng/triage-factory/internal/agentmeta"
	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	ghclient "github.com/sky-ai-eng/triage-factory/internal/github"
)

// reviewArtifactJSON is the wire shape the review overlay consumes. The staged
// body/event come from the artifact's details_json (TF-side staging, applied to
// GitHub only at approval); the comments are read LIVE from the GitHub pending
// review, each with its severity parsed back out of the body for the chip.
type reviewArtifactJSON struct {
	ID          string                      `json:"id"`
	RunID       string                      `json:"run_id,omitempty"`
	Owner       string                      `json:"owner"`
	Repo        string                      `json:"repo"`
	PRNumber    int                         `json:"pr_number"`
	ReviewID    string                      `json:"review_id"`
	ReviewBody  string                      `json:"review_body"`
	ReviewEvent string                      `json:"review_event"`
	State       string                      `json:"state"`
	Comments    []reviewArtifactCommentJSON `json:"comments"`
}

type reviewArtifactCommentJSON struct {
	ID        string `json:"id"`
	Path      string `json:"path"`
	Line      int    `json:"line"`
	StartLine *int   `json:"start_line,omitempty"`
	Body      string `json:"body"`
	Severity  string `json:"severity,omitempty"`
}

// reviewGet returns the review artifact augmented with its LIVE GitHub
// pending-review comments. Each comment body carries the severity badge baked in
// (the GitHub-native form); ParseSeverityBadge splits it back into a chip level +
// clean body for display. Body + event are the staged values from details_json.
// A live-fetch failure degrades to staged body/event with no comments rather than
// blanking the overlay.
func (ah *artifactsHandler) reviewGet(w http.ResponseWriter, r *http.Request, orgID string, art *domain.Artifact) {
	gh, owner, repo, number, ok := ah.ghForArtifact(w, r, orgID, art)
	if !ok {
		return
	}
	details, _ := domain.ParseReviewArtifactDetails(art.DetailsJSON)

	out := reviewArtifactJSON{
		ID:          art.ID,
		RunID:       art.RunID,
		Owner:       owner,
		Repo:        repo,
		PRNumber:    number,
		ReviewID:    art.ExternalID,
		ReviewBody:  details.ReviewBody,
		ReviewEvent: details.ReviewEvent,
		State:       art.State,
		Comments:    []reviewArtifactCommentJSON{},
	}
	if _, comments, err := gh.GetPendingReview(r.Context(), owner, repo, number); err != nil {
		artifactsLog.Warn("live pending-review fetch failed; serving staged body/event with no comments",
			"artifact", art.ID, "owner", owner, "repo", repo, "number", number, "error", err)
	} else {
		for _, c := range comments {
			sev, clean := domain.ParseSeverityBadge(c.Body)
			out.Comments = append(out.Comments, reviewArtifactCommentJSON{
				ID:        c.ID,
				Path:      c.Path,
				Line:      derefInt(c.Line),
				StartLine: c.StartLine,
				Body:      clean,
				Severity:  sev,
			})
		}
	}
	writeJSON(w, http.StatusOK, out)
}

// reviewUpdate stages a new review body and/or event into the artifact's
// details_json. TF-side only: a pending review's body + event are not
// live-editable on GitHub (the event *is* the submit), so they stage here and
// apply atomically at approval. No GitHub call.
func (ah *artifactsHandler) reviewUpdate(w http.ResponseWriter, r *http.Request, orgID, userID string, art *domain.Artifact) {
	// Only a still-pending review can be staged. A PATCH on a submitted/dismissed
	// artifact would write body/event that can never be applied (approve guards
	// state), so reject it as a conflict rather than return a misleading 200 on an
	// artifact the user can no longer act on. Mirrors the reviewApprove guard.
	if art.State != domain.ArtifactStateReviewPending {
		writeJSON(w, http.StatusConflict, map[string]string{"error": "this review is no longer pending (state: " + art.State + ")"})
		return
	}

	var req struct {
		ReviewBody  *string `json:"review_body"`
		ReviewEvent *string `json:"review_event"`
	}
	if !decodeJSON(w, r, &req, "") {
		return
	}

	details, err := domain.ParseReviewArtifactDetails(art.DetailsJSON)
	if err != nil {
		internalError(w, "artifacts", fmt.Errorf("parse review artifact details: %w", err))
		return
	}
	if req.ReviewBody != nil {
		details.ReviewBody = *req.ReviewBody
	}
	if req.ReviewEvent != nil {
		// Validate early (400) rather than deferring to approval-time GitHub
		// rejection. review_event also doubles as the ready sentinel, so an empty
		// or bogus value would silently un-park the run as well as fail the submit.
		if !validReviewEvent(*req.ReviewEvent) {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "review_event must be one of APPROVE, COMMENT, REQUEST_CHANGES"})
			return
		}
		details.ReviewEvent = *req.ReviewEvent
	}

	next := *art
	next.DetailsJSON = domain.MarshalReviewArtifactDetails(details)
	if err := ah.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		_, e := tx.Artifacts.Upsert(r.Context(), orgID, next)
		return e
	}); err != nil {
		internalError(w, "artifacts", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// reviewApprove submits the staged pending review to GitHub
// (SubmitExistingReview, applying the staged body + event + the agentmeta
// footer), flips the artifact pending → submitted, records the human verdict into
// run_memory, and runs the shared run/task/blueprint bookkeeping. The GitHub
// submit is pessimistic (non-2xx on failure, artifact untouched); everything
// after is detached best-effort.
func (ah *artifactsHandler) reviewApprove(w http.ResponseWriter, r *http.Request, orgID, userID string, art *domain.Artifact) {
	gh, owner, repo, number, ok := ah.ghForArtifact(w, r, orgID, art)
	if !ok {
		return
	}

	// Only a still-pending review can be approved. A stale/double click on an
	// already-submitted (or dismissed) artifact would otherwise re-submit.
	if art.State != domain.ArtifactStateReviewPending {
		writeJSON(w, http.StatusConflict, map[string]string{"error": "this review is no longer pending approval (state: " + art.State + ")"})
		return
	}

	details, err := domain.ParseReviewArtifactDetails(art.DetailsJSON)
	if err != nil {
		internalError(w, "artifacts", fmt.Errorf("parse review artifact details: %w", err))
		return
	}
	// The ready sentinel must be set — the agent finalized the review via
	// submit-review. A pending artifact without it was started but never
	// submitted, so there's nothing to approve.
	if details.ReviewEvent == "" {
		writeJSON(w, http.StatusConflict, map[string]string{"error": "this review has not been finalized by the agent yet"})
		return
	}

	// Capture the final (human-edited) inline comments for the verdict diff BEFORE
	// submitting — once submitted the review leaves the PENDING state and
	// GetPendingReview returns nothing. A read failure just drops the comment diff.
	// If the live pending review's id doesn't match the one we recorded, the
	// original was replaced out-of-band: submitting art.ExternalID would diff the
	// verdict against a different review (or fail at GitHub), so 409 before any
	// mutation and let the user restart the preview. (An empty id means no live
	// pending review — the stale-submit failure surfaces from GitHub below.)
	var finalComments []ghclient.PendingReviewComment
	if pendingID, comments, gerr := gh.GetPendingReview(r.Context(), owner, repo, number); gerr != nil {
		artifactsLog.Warn("pre-approve pending-review re-read failed; verdict comment diff skipped",
			"artifact", art.ID, "error", gerr)
	} else if pendingID != "" && pendingID != art.ExternalID {
		artifactsLog.Warn("live pending review id differs from the recorded one; refusing to approve",
			"artifact", art.ID, "recorded", art.ExternalID, "live", pendingID)
		writeJSON(w, http.StatusConflict, map[string]string{"error": "the pending review on this PR no longer matches the one prepared here — return it to the queue and start a fresh review"})
		return
	} else {
		finalComments = comments
	}

	// Submit the pending review to GitHub with the staged body + event + footer.
	body := details.ReviewBody + agentmeta.Build(ah.agentRuns, orgID, art.RunID, "review")
	if err := gh.SubmitExistingReview(r.Context(), owner, repo, art.ExternalID, details.ReviewEvent, body); err != nil {
		artifactsLog.Warn("SubmitExistingReview failed",
			"artifact", art.ID, "owner", owner, "repo", repo, "number", number, "review", art.ExternalID, "error", err)
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "GitHub API error: " + err.Error()})
		return
	}

	cleanupCtx := context.WithoutCancel(r.Context())

	// Step 1: flip the artifact pending → submitted, and compose the audit row
	// with the flip (TFAC-483). The org-App SubmitExistingReview is a
	// human-authorized, org-executed write — run_id is the drafting run, actor is
	// the approver. Atomic with the flip.
	submitted := *art
	submitted.State = domain.ArtifactStateReviewSubmitted
	if err := ah.tx.WithTx(cleanupCtx, orgID, userID, func(tx db.TxStores) error {
		if _, e := tx.Artifacts.Upsert(cleanupCtx, orgID, submitted); e != nil {
			return e
		}
		return tx.ExternalActions.Record(cleanupCtx, orgID,
			githubApprovalAction(art, userID, domain.ActionReviewSubmitted, domain.ArtifactStateReviewPending, domain.ArtifactStateReviewSubmitted))
	}); err != nil {
		artifactsLog.Warn("flip review artifact to submitted + record action failed", "artifact", art.ID, "error", err)
	}

	// Step 2: human verdict capture — diff the agent's proposed draft against the
	// human-edited final (body, event, comments) into run_memory.human_content.
	if art.RunID != "" {
		humanContent := FormatHumanFeedback(buildReviewHumanFeedbackInput(details, finalComments))
		if err := ah.tx.WithTx(cleanupCtx, orgID, userID, func(tx db.TxStores) error {
			return tx.TaskMemory.UpdateRunMemoryHumanContent(cleanupCtx, orgID, art.RunID, humanContent)
		}); err != nil {
			artifactsLog.Warn("failed to record human verdict", "run", art.RunID, "error", err)
		}
	}

	// Step 3: shared run/task/blueprint bookkeeping.
	ah.finishApprovedRun(cleanupCtx, orgID, userID, art.RunID)

	writeJSON(w, http.StatusOK, map[string]any{
		"review_id": art.ExternalID,
		"event":     details.ReviewEvent,
		"state":     domain.ArtifactStateReviewSubmitted,
	})
}

// handleArtifactCommentUpdate edits one inline comment on the live GitHub pending
// review (PUT /api/artifacts/{id}/comments/{commentId}). Review-only. Pessimistic:
// a GitHub failure returns non-2xx. The severity badge is re-baked onto the
// human's edited (clean) body — severity isn't editable, just preserved — by
// recovering it from the comment's current GitHub body.
func (ah *artifactsHandler) handleArtifactCommentUpdate(w http.ResponseWriter, r *http.Request) {
	orgID, ok := requireOrg(w, r)
	if !ok {
		return
	}
	userID := ClaimsFrom(r.Context()).Subject
	id := r.PathValue("id")
	commentID := r.PathValue("commentId")

	var req struct {
		Body string `json:"body"`
	}
	if !decodeJSON(w, r, &req, "") {
		return
	}
	if req.Body == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "body is required"})
		return
	}

	art, ok := ah.loadArtifact(w, r, orgID, userID, id)
	if !ok {
		return
	}
	if art.Kind != domain.ArtifactKindReview {
		notFound(w, "review artifact")
		return
	}
	gh, owner, repo, number, ok := ah.ghForArtifact(w, r, orgID, art)
	if !ok {
		return
	}

	// Read this artifact's pending review (scoped to its owner/repo/number) and
	// require the comment to belong to it before editing. The GraphQL update keys
	// solely on the comment node id, so without this a commentId from another
	// pending review the credential can see would be editable through this
	// artifact-scoped route. The same fetch recovers the comment's current severity
	// (baked into its GitHub body) to re-bake the badge onto the human's edit.
	pendingID, comments, err := gh.GetPendingReview(r.Context(), owner, repo, number)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "GitHub API error: " + err.Error()})
		return
	}
	// If the live pending review isn't the one the artifact records, it was
	// replaced out-of-band — the comment-membership check below would pass against
	// the wrong review. 409 so the edit can't land on a review this artifact no
	// longer represents.
	if pendingID != "" && pendingID != art.ExternalID {
		writeJSON(w, http.StatusConflict, map[string]string{"error": "the pending review on this PR no longer matches the one prepared here — return it to the queue and start a fresh review"})
		return
	}
	severity, found := "", false
	for _, c := range comments {
		if c.ID == commentID {
			severity, _ = domain.ParseSeverityBadge(c.Body)
			found = true
			break
		}
	}
	if !found {
		notFound(w, "review comment")
		return
	}
	_, clean := domain.ParseSeverityBadge(req.Body)
	if err := gh.UpdatePendingReviewComment(r.Context(), commentID, domain.SeverityBadgeMarkdown(severity)+clean); err != nil {
		artifactsLog.Warn("UpdatePendingReviewComment failed", "artifact", art.ID, "comment", commentID, "error", err)
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "GitHub API error: " + err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// handleArtifactCommentDelete removes one inline comment from the live GitHub
// pending review (DELETE /api/artifacts/{id}/comments/{commentId}). Review-only,
// pessimistic.
func (ah *artifactsHandler) handleArtifactCommentDelete(w http.ResponseWriter, r *http.Request) {
	orgID, ok := requireOrg(w, r)
	if !ok {
		return
	}
	userID := ClaimsFrom(r.Context()).Subject
	id := r.PathValue("id")
	commentID := r.PathValue("commentId")

	art, ok := ah.loadArtifact(w, r, orgID, userID, id)
	if !ok {
		return
	}
	if art.Kind != domain.ArtifactKindReview {
		notFound(w, "review artifact")
		return
	}
	gh, owner, repo, number, ok := ah.ghForArtifact(w, r, orgID, art)
	if !ok {
		return
	}
	// DeletePendingReviewComment keys solely off the comment node id, so require the
	// comment to belong to THIS artifact's pending review first — otherwise a
	// commentId from another pending review the credential can see would be
	// deletable through this artifact-scoped route.
	pendingID, comments, err := gh.GetPendingReview(r.Context(), owner, repo, number)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "GitHub API error: " + err.Error()})
		return
	}
	// A live pending review that isn't the recorded one was replaced out-of-band;
	// 409 rather than delete against a review this artifact no longer represents.
	if pendingID != "" && pendingID != art.ExternalID {
		writeJSON(w, http.StatusConflict, map[string]string{"error": "the pending review on this PR no longer matches the one prepared here — return it to the queue and start a fresh review"})
		return
	}
	found := false
	for _, c := range comments {
		if c.ID == commentID {
			found = true
			break
		}
	}
	if !found {
		notFound(w, "review comment")
		return
	}
	if err := gh.DeletePendingReviewComment(r.Context(), commentID); err != nil {
		artifactsLog.Warn("DeletePendingReviewComment failed", "artifact", art.ID, "comment", commentID, "error", err)
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "GitHub API error: " + err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// buildReviewHumanFeedbackInput diffs the agent's proposed review draft (snapshot
// in details_json) against the human-edited final (staged body/event + the live
// comments captured at approval), producing the input FormatHumanFeedback renders
// into run_memory.human_content. Comments are joined by their GitHub node id;
// severity badges are stripped from both sides so the diff shows clean prose.
func buildReviewHumanFeedbackInput(details domain.ReviewArtifactDetails, finalComments []ghclient.PendingReviewComment) HumanFeedbackInput {
	type cmt struct {
		path string
		line int
		body string
	}
	proposed := make(map[string]cmt, len(details.Proposed.Comments))
	proposedOrder := make([]string, 0, len(details.Proposed.Comments))
	for _, c := range details.Proposed.Comments {
		if c.ID == "" {
			continue
		}
		_, clean := domain.ParseSeverityBadge(c.Body)
		if _, seen := proposed[c.ID]; !seen {
			proposedOrder = append(proposedOrder, c.ID)
		}
		proposed[c.ID] = cmt{path: c.Path, line: derefInt(c.Line), body: clean}
	}
	final := make(map[string]cmt, len(finalComments))
	finalOrder := make([]string, 0, len(finalComments))
	for _, c := range finalComments {
		if c.ID == "" {
			continue
		}
		_, clean := domain.ParseSeverityBadge(c.Body)
		if _, seen := final[c.ID]; !seen {
			finalOrder = append(finalOrder, c.ID)
		}
		final[c.ID] = cmt{path: c.Path, line: derefInt(c.Line), body: clean}
	}

	entries := make([]ReviewCommentDiffEntry, 0, len(proposedOrder)+len(finalOrder))
	for _, id := range proposedOrder {
		p := proposed[id]
		f, ok := final[id]
		switch {
		case !ok:
			entries = append(entries, ReviewCommentDiffEntry{Path: p.path, Line: p.line, Status: CommentDiffRemoved, Original: p.body})
		case p.body != f.body:
			entries = append(entries, ReviewCommentDiffEntry{Path: p.path, Line: p.line, Status: CommentDiffEdited, Original: p.body, Final: f.body})
		default:
			entries = append(entries, ReviewCommentDiffEntry{Path: p.path, Line: p.line, Status: CommentDiffUnchanged, Original: p.body, Final: f.body})
		}
	}
	// Comments in the final set but not the agent's draft were added by the human
	// (out of scope today, but handled so the diff stays honest if it happens).
	for _, id := range finalOrder {
		if _, ok := proposed[id]; ok {
			continue
		}
		f := final[id]
		entries = append(entries, ReviewCommentDiffEntry{Path: f.path, Line: f.line, Status: CommentDiffAdded, Final: f.body})
	}

	proposedBody := details.Proposed.Body
	proposedEvent := details.Proposed.Event
	return HumanFeedbackInput{
		OriginalBody:  &proposedBody,
		FinalBody:     details.ReviewBody,
		OriginalEvent: &proposedEvent,
		FinalEvent:    details.ReviewEvent,
		Comments:      entries,
	}
}

// derefInt returns the pointed-to int, or 0 for a nil pointer (a GitHub comment
// with no anchor on the current diff). The 0 only feeds display/grouping, never a
// GitHub write.
func derefInt(p *int) int {
	if p == nil {
		return 0
	}
	return *p
}

// validReviewEvent reports whether e is a GitHub PullRequestReviewEvent the
// approve path can submit. Empty is intentionally invalid: review_event doubles
// as the ready sentinel, so clearing it would un-park the run.
func validReviewEvent(e string) bool {
	switch e {
	case "APPROVE", "COMMENT", "REQUEST_CHANGES":
		return true
	default:
		return false
	}
}
