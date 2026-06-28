package server

import (
	"context"
	"fmt"
	"net/http"
	"strconv"

	"github.com/sky-ai-eng/triage-factory/internal/agentmeta"
	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	ghclient "github.com/sky-ai-eng/triage-factory/internal/github"
)

// reviewArtifactJSON is the wire shape the review overlay consumes. The whole
// review is staged TF-side (TFAC-494): body/event and the inline comments all
// come from the artifact's details_json, applied to GitHub only by the atomic
// submit at approval. Each comment's severity is parsed back out of its body for
// the chip. ReviewID is empty until approval stamps the submitted review's id.
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

// reviewGet returns the review artifact with its TF-side staged comments
// (TFAC-494) — no GitHub call. Each comment body carries the severity badge baked
// in; ParseSeverityBadge splits it back into a chip level + clean body for
// display. Body + event are the staged values; the whole review is local until
// the atomic submit at approval.
func (ah *artifactsHandler) reviewGet(w http.ResponseWriter, r *http.Request, orgID string, art *domain.Artifact) {
	owner, repo, number, ok := domain.ParsePRTarget(art.Target)
	if !ok {
		internalError(w, "artifacts", fmt.Errorf("malformed artifact target %q (artifact %s)", art.Target, art.ID))
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
	for _, c := range details.StagedComments {
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

// reviewApprove creates and submits the staged review to GitHub atomically
// (SubmitReview — one POST carrying commit_id + event + body + footer + the staged
// comments[]), stamps the submitted review's id + URL onto the artifact, flips it
// pending → submitted, records the human verdict into run_memory, and runs the
// shared run/task/blueprint bookkeeping. Nothing touched GitHub before this point
// (the review was staged entirely TF-side), so concurrent runs on one PR each
// submit their own review here — GitHub allows unlimited submitted reviews per
// identity. The submit is pessimistic (non-2xx on failure, artifact untouched);
// everything after is detached best-effort.
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
	// finalized, so there's nothing to approve.
	if details.ReviewEvent == "" {
		writeJSON(w, http.StatusConflict, map[string]string{"error": "this review has not been finalized by the agent yet"})
		return
	}

	// Translate the staged comments into the SubmitReview payload. A comment with
	// no anchor (nil Line) can't be submitted to GitHub — surface it rather than
	// silently dropping it. (A stale anchor after a force-push surfaces as
	// GitHub's 422, already classified in SubmitReview.)
	submitComments := make([]ghclient.SubmitReviewComment, 0, len(details.StagedComments))
	for _, c := range details.StagedComments {
		if c.Line == nil {
			writeJSON(w, http.StatusUnprocessableEntity, map[string]string{
				"error": "a staged review comment on " + c.Path + " has no line anchor and can't be submitted — edit or delete it, then approve again",
			})
			return
		}
		submitComments = append(submitComments, ghclient.SubmitReviewComment{
			Path:      c.Path,
			Line:      *c.Line,
			StartLine: c.StartLine,
			Body:      c.Body,
		})
	}

	// Create + submit the review in one POST, pinned to the commit the agent
	// reviewed (details.HeadSHA), with the staged body + event + footer.
	body := details.ReviewBody + agentmeta.Build(ah.agentRuns, orgID, art.RunID, "review")
	reviewID, _, err := gh.SubmitReview(r.Context(), owner, repo, number, details.HeadSHA, details.ReviewEvent, body, submitComments)
	if err != nil {
		artifactsLog.Warn("SubmitReview failed",
			"artifact", art.ID, "owner", owner, "repo", repo, "number", number, "error", err)
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "GitHub API error" + localDetail(err)})
		return
	}

	cleanupCtx := context.WithoutCancel(r.Context())

	// Step 1: stamp the submitted review's id + URL onto the artifact (a submitted
	// review finally has both — a never-published draft had neither), flip it
	// pending → submitted, and compose the audit row with the flip (TFAC-483). The
	// org-App submit is a human-authorized, org-executed write — run_id is the
	// drafting run, actor is the approver. Atomic with the flip.
	submitted := *art
	submitted.State = domain.ArtifactStateReviewSubmitted
	submitted.ExternalID = strconv.Itoa(reviewID)
	submitted.URL = fmt.Sprintf("%s#pullrequestreview-%d", domain.GitHubPullURL(owner+"/"+repo, number), reviewID)
	if err := ah.tx.WithTx(cleanupCtx, orgID, userID, func(tx db.TxStores) error {
		if _, e := tx.Artifacts.Upsert(cleanupCtx, orgID, submitted); e != nil {
			return e
		}
		return tx.ExternalActions.Record(cleanupCtx, orgID,
			githubApprovalAction(&submitted, userID, domain.ActionReviewSubmitted, domain.ArtifactStateReviewPending, domain.ArtifactStateReviewSubmitted))
	}); err != nil {
		artifactsLog.Warn("flip review artifact to submitted + record action failed", "artifact", art.ID, "error", err)
	}

	// Step 2: human verdict capture — diff the agent's proposed draft against the
	// human-edited final (body, event, the staged comments) into
	// run_memory.human_content.
	if art.RunID != "" {
		humanContent := FormatHumanFeedback(buildReviewHumanFeedbackInput(details, details.StagedComments))
		if err := ah.tx.WithTx(cleanupCtx, orgID, userID, func(tx db.TxStores) error {
			return tx.TaskMemory.UpdateRunMemoryHumanContent(cleanupCtx, orgID, art.RunID, humanContent)
		}); err != nil {
			artifactsLog.Warn("failed to record human verdict", "run", art.RunID, "error", err)
		}
	}

	// Step 3: terminal-on-last task closure. Approval is a decoupled sidecar — it
	// never flips run status or resumes/terminates a blueprint. The only lifecycle
	// effect is closing the task when this was the LAST unresolved artifact on an
	// already-terminal blueprint; otherwise a no-op.
	ah.closeTaskIfTerminalAndResolved(cleanupCtx, orgID, userID, art.RunID)

	writeJSON(w, http.StatusOK, map[string]any{
		"review_id": submitted.ExternalID,
		"url":       submitted.URL,
		"event":     details.ReviewEvent,
		"state":     domain.ArtifactStateReviewSubmitted,
	})
}

// reviewDismiss resolves a pending review artifact by abandoning it: it flips the
// artifact pending → dismissed, records the audit row, then runs the
// terminal-on-last task-closure check. The per-item counterpart to reviewApprove;
// like approve it never touches the run's lifecycle. No GitHub call — the review
// is staged entirely TF-side (TFAC-494), so there is no pending review object to
// delete; the flip is the whole resolution. An already-submitted /
// already-dismissed review is terminal → 409. A pending review is dismissable
// whether or not it reached the ready sentinel (a started-but-unfinalized review
// is still abandonable).
func (ah *artifactsHandler) reviewDismiss(w http.ResponseWriter, r *http.Request, orgID, userID string, art *domain.Artifact) {
	if art.State != domain.ArtifactStateReviewPending {
		writeJSON(w, http.StatusConflict, map[string]string{"error": "this review is no longer pending (state: " + art.State + ")"})
		return
	}

	// Flip pending → dismissed + audit, atomic and pessimistic — the flip is the
	// resolution. The proposed snapshot is preserved for the audit ledger;
	// abandonment retires the staged draft, not the record of what the agent wrote.
	dismissed := *art
	dismissed.State = domain.ArtifactStateReviewDismissed
	if err := ah.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		if _, e := tx.Artifacts.Upsert(r.Context(), orgID, dismissed); e != nil {
			return e
		}
		return tx.ExternalActions.Record(r.Context(), orgID,
			githubApprovalAction(art, userID, domain.ActionReviewDismissed, domain.ArtifactStateReviewPending, domain.ArtifactStateReviewDismissed))
	}); err != nil {
		internalError(w, "artifacts", err)
		return
	}

	// The dismiss is fully resolved by the flip above (no GitHub object). Run the
	// terminal-on-last task-closure check detached so a client disconnect can't
	// strand it now that the artifact is resolved.
	cleanupCtx := context.WithoutCancel(r.Context())
	ah.closeTaskIfTerminalAndResolved(cleanupCtx, orgID, userID, art.RunID)

	writeJSON(w, http.StatusOK, map[string]any{
		"review_id": art.ExternalID,
		"state":     domain.ArtifactStateReviewDismissed,
	})
}

// handleArtifactCommentUpdate edits one staged inline comment on the review draft
// (PUT /api/artifacts/{id}/comments/{commentId}). Review-only, no GitHub call —
// the whole review is staged TF-side until approval (TFAC-494). The severity
// badge is re-baked onto the human's edited (clean) body — severity isn't
// editable, just preserved — by recovering it from the staged comment's body.
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
	// Only a still-pending review can be edited: once submitted, its comments are
	// public and immutable through this path.
	if art.State != domain.ArtifactStateReviewPending {
		writeJSON(w, http.StatusConflict, map[string]string{"error": "this review is no longer pending (state: " + art.State + ")"})
		return
	}
	details, err := domain.ParseReviewArtifactDetails(art.DetailsJSON)
	if err != nil {
		internalError(w, "artifacts", fmt.Errorf("parse review artifact details: %w", err))
		return
	}
	// Find the staged comment by its local id and re-bake its current severity onto
	// the human's clean edit.
	idx := -1
	for i, c := range details.StagedComments {
		if c.ID == commentID {
			idx = i
			break
		}
	}
	if idx < 0 {
		notFound(w, "review comment")
		return
	}
	severity, _ := domain.ParseSeverityBadge(details.StagedComments[idx].Body)
	_, clean := domain.ParseSeverityBadge(req.Body)
	details.StagedComments[idx].Body = domain.SeverityBadgeMarkdown(severity) + clean

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

// handleArtifactCommentDelete removes one staged inline comment from the review
// draft (DELETE /api/artifacts/{id}/comments/{commentId}). Review-only, no GitHub
// call — the staged set is local until approval (TFAC-494).
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
	if art.State != domain.ArtifactStateReviewPending {
		writeJSON(w, http.StatusConflict, map[string]string{"error": "this review is no longer pending (state: " + art.State + ")"})
		return
	}
	details, err := domain.ParseReviewArtifactDetails(art.DetailsJSON)
	if err != nil {
		internalError(w, "artifacts", fmt.Errorf("parse review artifact details: %w", err))
		return
	}
	idx := -1
	for i, c := range details.StagedComments {
		if c.ID == commentID {
			idx = i
			break
		}
	}
	if idx < 0 {
		notFound(w, "review comment")
		return
	}
	details.StagedComments = append(details.StagedComments[:idx], details.StagedComments[idx+1:]...)

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

// buildReviewHumanFeedbackInput diffs the agent's proposed review draft (snapshot
// in details_json) against the human-edited final (staged body/event + the staged
// comments at approval), producing the input FormatHumanFeedback renders into
// run_memory.human_content. Comments are joined by their stable TF-local id;
// severity badges are stripped from both sides so the diff shows clean prose.
func buildReviewHumanFeedbackInput(details domain.ReviewArtifactDetails, finalComments []domain.ReviewArtifactComment) HumanFeedbackInput {
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
