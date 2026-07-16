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
	"github.com/sky-ai-eng/triage-factory/internal/review"
)

// reviewArtifactJSON is the wire shape the review overlay consumes. The whole
// review is staged TF-side (TFAC-494): body/event and the inline comments all
// come from the artifact's details_json, applied to GitHub only by the atomic
// submit at approval. Each comment's severity is parsed back out of its body for
// the chip. ReviewID is empty until approval stamps the submitted review's id.
type reviewArtifactJSON struct {
	ID          string `json:"id"`
	RunID       string `json:"run_id,omitempty"`
	Owner       string `json:"owner"`
	Repo        string `json:"repo"`
	PRNumber    int    `json:"pr_number"`
	ReviewID    string `json:"review_id"`
	ReviewBody  string `json:"review_body"`
	ReviewEvent string `json:"review_event"`
	State       string `json:"state"`
	// URL is the posted review's GitHub deep link (#pullrequestreview-<id>),
	// stamped at approval — empty while the draft is pending (nothing exists on
	// GitHub yet). The read-only overlay renders it as "View on GitHub".
	URL      string                      `json:"url"`
	Comments []reviewArtifactCommentJSON `json:"comments"`
	// CommitsSinceFinalize is how many commits the live PR head is ahead of the
	// finalize-time head (TFAC-500) — the "N commits since this review was written"
	// indicator. nil when it can't be computed (no finalize head stored, or the
	// live head couldn't be fetched); a non-nil 0 means the PR hasn't advanced.
	CommitsSinceFinalize *int `json:"commits_since_finalize"`
}

type reviewArtifactCommentJSON struct {
	ID        string `json:"id"`
	Path      string `json:"path"`
	Line      int    `json:"line"`
	StartLine *int   `json:"start_line,omitempty"`
	Body      string `json:"body"`
	Severity  string `json:"severity,omitempty"`
	// Freshness is the per-comment staleness vs. the live PR head (TFAC-500):
	// "current", "moved", "outdated", or "unknown". See review_freshness.go.
	Freshness string `json:"freshness"`
	// MappedLine / MappedPath carry the comment's new-side position on the live
	// head when Freshness is "moved" (the anchored code relocated). Zero/empty for
	// every other verdict. MappedPath is set ONLY for a rename — a "moved" comment
	// with an absent MappedPath means "same file, line shifted to MappedLine"; the
	// consumer falls back to the comment's original path in that case.
	MappedLine int    `json:"mapped_line,omitempty"`
	MappedPath string `json:"mapped_path,omitempty"`
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
	// Surface a corrupt details_json as a 500 rather than serving a misleadingly
	// empty review (no body, no comments) the user might approve or act on.
	details, err := domain.ParseReviewArtifactDetails(art.DetailsJSON)
	if err != nil {
		internalError(w, "artifacts", fmt.Errorf("parse review artifact details (artifact %s): %w", art.ID, err))
		return
	}

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
		URL:         art.URL,
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
			// Default to "unknown"; annotateReviewFreshness upgrades it per comment
			// when the live head + compare resolve. Pre-seeding here means a GitHub
			// failure simply leaves it at "unknown" with no extra branching.
			Freshness: review.FreshnessUnknown,
		})
	}
	// Augment with per-comment freshness + commits-since-finalize against the live
	// PR head (TFAC-500). Best-effort: never errors the overlay — on any GitHub
	// failure the comments stay "unknown" and the count stays nil. Pending only:
	// a resolved review is immutable, so drift against the live head is
	// meaningless (and the GitHub reads would be wasted).
	if art.State == domain.ArtifactStateReviewPending {
		ah.annotateReviewFreshness(r.Context(), orgID, art, details, &out)
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

	if !ah.updateReviewDetailsPending(w, r, orgID, userID, art.ID, details) {
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// updateReviewDetailsPending persists mutated review-draft details through the
// state-guarded write (UpdateReviewDetailsIfPending), shared by every draft
// mutation (PATCH body/event, comment edit/delete, refresh). The guard closes
// the read-mutate-write race those handlers all have: their upfront pending
// check reads a snapshot, so an unconditional Upsert landing after a concurrent
// approve would write the stale pending state back over submitted — making the
// posted review approvable (and submittable) again. A lost race is a 409, the
// same conflict the upfront check reports. Writes the HTTP error response and
// returns false on any failure.
func (ah *artifactsHandler) updateReviewDetailsPending(w http.ResponseWriter, r *http.Request, orgID, userID, artifactID string, details domain.ReviewArtifactDetails) bool {
	var updated bool
	if err := ah.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		var e error
		updated, e = tx.Artifacts.UpdateReviewDetailsIfPending(r.Context(), orgID, artifactID, domain.MarshalReviewArtifactDetails(details))
		return e
	}); err != nil {
		internalError(w, "artifacts", err)
		return false
	}
	if !updated {
		writeJSON(w, http.StatusConflict, map[string]string{"error": "this review is no longer pending — it was just submitted or dismissed"})
		return false
	}
	return true
}

// reviewApprove creates and submits the staged review to GitHub atomically
// (SubmitReview — one POST carrying commit_id + event + body + footer + the staged
// comments[]), stamps the submitted review's id + URL onto the artifact, records
// the human verdict into run_memory, and runs the shared run/task/blueprint
// bookkeeping. Nothing touched GitHub before this point (the review was staged
// entirely TF-side), so concurrent runs on one PR each submit their own review
// here — GitHub allows unlimited submitted reviews per identity.
//
// The pending → submitted flip is a CAS CLAIM taken BEFORE the GitHub write, so
// the submit is at-most-once: of two racing approves (two tabs, two pods) exactly
// one wins the claim and reaches GitHub; the loser 409s. The claim also shuts the
// door on every state-guarded mutation for the duration. A GitHub failure
// releases the claim (submitted → pending) so retry works; the release running
// AFTER the failed write means a crash mid-flight strands the artifact as
// submitted-without-URL — deliberately preferred over the inverse order, where a
// crash between a successful post and the flip leaves the review re-submittable.
//
// The content submitted is read back AFTER the claim, in the claim's own tx —
// never the snapshot the handler was dispatched with. The claim is the
// linearization point for the draft: every guarded mutation requires
// state=pending, so an edit racing the approve click either commits before the
// claim (and is included in the re-read) or loses to it (409); submitting the
// pre-claim snapshot would post content the user had already edited away.
func (ah *artifactsHandler) reviewApprove(w http.ResponseWriter, r *http.Request, orgID, userID string, art *domain.Artifact) {
	gh, owner, repo, number, ok := ah.ghForArtifact(w, r, orgID, art)
	if !ok {
		return
	}

	// Fast path: a stale/double click on an already-resolved artifact gets a
	// state-specific 409 without claim churn. Advisory only — the CAS below is
	// the real guard.
	if art.State != domain.ArtifactStateReviewPending {
		writeJSON(w, http.StatusConflict, map[string]string{"error": "this review is no longer pending approval (state: " + art.State + ")"})
		return
	}

	// Claim the review BEFORE reading the content to submit: CAS pending →
	// submitted, plus a re-read of the now-frozen row in the same tx (a read
	// failure rolls the claim back with it, so no claim can outlive this tx
	// un-observed). Exactly one caller wins; a loser (concurrent approve from
	// another tab/pod, or a just-landed dismiss) 409s without touching GitHub,
	// which is what makes the submit at-most-once. The claim carries no
	// coordinates yet — GitHub hasn't minted them.
	var claimed bool
	var fresh *domain.Artifact
	if err := ah.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		var e error
		claimed, e = tx.Artifacts.TransitionReviewState(r.Context(), orgID, art.ID,
			domain.ArtifactStateReviewPending, domain.ArtifactStateReviewSubmitted, "", "", "")
		if e != nil || !claimed {
			return e
		}
		fresh, e = tx.Artifacts.Get(r.Context(), orgID, art.ID)
		return e
	}); err != nil {
		internalError(w, "artifacts", err)
		return
	}
	if !claimed {
		writeJSON(w, http.StatusConflict, map[string]string{"error": "this review is no longer pending approval — it was just submitted or dismissed"})
		return
	}
	if fresh == nil {
		// The row vanished between the claim UPDATE and the re-read in the same
		// tx — not a state anything in the system produces.
		internalError(w, "artifacts", fmt.Errorf("review artifact %s unreadable immediately after claim", art.ID))
		return
	}

	// releaseClaim undoes the pending→submitted claim on any post-claim failure
	// (validation or the GitHub write), so the user can fix and retry. Detached
	// from the request context: a client disconnect mustn't strand the claim.
	releaseClaim := func() {
		releaseCtx := context.WithoutCancel(r.Context())
		var released bool
		if rerr := ah.tx.WithTx(releaseCtx, orgID, userID, func(tx db.TxStores) error {
			var e error
			released, e = tx.Artifacts.TransitionReviewState(releaseCtx, orgID, art.ID,
				domain.ArtifactStateReviewSubmitted, domain.ArtifactStateReviewPending, "", "", "")
			return e
		}); rerr != nil || !released {
			artifactsLog.Error("failed to release review submit claim — review stuck as submitted without a posted review",
				"artifact", art.ID, "released", released, "error", rerr)
		}
	}

	details, err := domain.ParseReviewArtifactDetails(fresh.DetailsJSON)
	if err != nil {
		releaseClaim()
		internalError(w, "artifacts", fmt.Errorf("parse review artifact details: %w", err))
		return
	}
	// The ready sentinel must be set — the agent finalized the review via
	// finalize-review. A pending artifact without it was started but never
	// finalized (or an agent-side reset raced the click), so there's nothing to
	// approve.
	if details.ReviewEvent == "" {
		releaseClaim()
		writeJSON(w, http.StatusConflict, map[string]string{"error": "this review has not been finalized by the agent yet"})
		return
	}

	// Translate the staged comments into the SubmitReview payload, and derive the
	// commit_id to pin the review to. GitHub reads each comment's line in the frame
	// of a commit; the atomic submit carries ONE commit_id for the whole review.
	// finalize-review already reconciled every comment to the PR's single current
	// head (TFAC-499) — auto-remapping pure shifts and failing on anything outdated
	// — so by the time a review is approvable its comments all share one CommitSHA;
	// the pin simply follows it. A comment-less review (approve / body-only) has no
	// inline anchor, so it falls back to the start-review head (details.HeadSHA).
	submitComments := make([]ghclient.SubmitReviewComment, 0, len(details.StagedComments))
	commitID := details.HeadSHA
	for _, c := range details.StagedComments {
		if c.Line == nil {
			releaseClaim()
			writeJSON(w, http.StatusUnprocessableEntity, map[string]string{
				"error": "a staged review comment on " + c.Path + " has no line anchor and can't be submitted — edit or delete it, then approve again",
			})
			return
		}
		if c.CommitSHA != "" {
			commitID = c.CommitSHA
		}
		submitComments = append(submitComments, ghclient.SubmitReviewComment{
			Path:      c.Path,
			Line:      *c.Line,
			StartLine: c.StartLine,
			Body:      c.Body,
		})
	}

	// Create + submit the review in one POST, pinned to commitID (the commit its
	// inline comments were validated against), with the staged body + event +
	// footer. SubmitReview downgrades APPROVE→COMMENT when auto-merge is enabled on
	// the PR and returns the event GitHub actually recorded — capture it so the
	// persisted artifact, the verdict diff, and the response all reflect what
	// landed, not what was requested.
	body := details.ReviewBody + agentmeta.Build(ah.agentRuns, orgID, fresh.RunID, "review")
	reviewID, submittedEvent, err := gh.SubmitReview(r.Context(), owner, repo, number, commitID, details.ReviewEvent, body, submitComments)
	if err != nil {
		artifactsLog.Warn("SubmitReview failed",
			"artifact", art.ID, "owner", owner, "repo", repo, "number", number, "error", err)
		// Release the claim so the user can retry — nothing reached GitHub.
		releaseClaim()
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "GitHub API error" + localDetail(err)})
		return
	}
	// Record the event GitHub actually applied (post auto-merge downgrade) so a
	// later reader — and the proposed-vs-final diff below — sees the truth.
	details.ReviewEvent = submittedEvent

	cleanupCtx := context.WithoutCancel(r.Context())

	// Step 1: stamp the submitted review's id + URL onto the claimed artifact (a
	// submitted review finally has both — a never-published draft had neither) and
	// the actual submitted event into details, and compose the audit row with the
	// stamp (TFAC-483). The org-App submit is a human-authorized, org-executed
	// write — run_id is the drafting run, actor is the approver. The state is
	// already submitted (the claim), so this is a submitted→submitted CAS carrying
	// the coordinates; retried once, and a double failure is loud — until the
	// stamp lands the row shows submitted with no URL, which is why this must not
	// stay a silent Warn.
	submitted := *fresh
	submitted.State = domain.ArtifactStateReviewSubmitted
	submitted.ExternalID = strconv.Itoa(reviewID)
	// Anchor the "View on GitHub" deep link to the org's own GitHub host —
	// github.com, a GHES host, or a GHEC data-residency host — not a hardcoded
	// github.com. A review artifact carries no GitHub-minted html_url (unlike a PR
	// artifact), so the URL is composed here; BaseURLFor is the same host
	// resolution the client that just submitted the review used, so the link and
	// the submit agree on the server. A resolve failure degrades to the public
	// host rather than failing the stamp after the review is already live.
	//
	// On cleanupCtx, not r.Context(): this runs after the review is live on
	// GitHub, so it must not turn on request liveness. A client disconnect
	// between the submit and here would cancel r.Context(), fail the resolve, and
	// silently downgrade the stamped link to github.com — reintroducing this very
	// bug in a GHES/GHEC org through a narrow race. The detached context makes the
	// host resolution unconditional, like the rest of this post-write block.
	webBase, baseErr := ah.ghResolver.BaseURLFor(cleanupCtx, orgID)
	if baseErr != nil {
		artifactsLog.Warn("resolve github base for review URL failed; using default host",
			"artifact", art.ID, "org", orgID, "error", baseErr)
		webBase = ""
	}
	submitted.URL = fmt.Sprintf("%s#pullrequestreview-%d", domain.GitHubPullURLBase(webBase, owner+"/"+repo, number), reviewID)
	submitted.DetailsJSON = domain.MarshalReviewArtifactDetails(details)
	stamp := func() error {
		return ah.tx.WithTx(cleanupCtx, orgID, userID, func(tx db.TxStores) error {
			stamped, e := tx.Artifacts.TransitionReviewState(cleanupCtx, orgID, art.ID,
				domain.ArtifactStateReviewSubmitted, domain.ArtifactStateReviewSubmitted,
				submitted.ExternalID, submitted.URL, submitted.DetailsJSON)
			if e != nil {
				return e
			}
			// A zero-row CAS means the claimed row is gone or no longer submitted —
			// nothing here can move it post-claim, so this is a real anomaly. Error
			// out (rolling back the audit row: it composes with the stamp,
			// both-or-neither) so the retry + loud-failure path below sees it
			// instead of a silent fake success.
			if !stamped {
				return fmt.Errorf("stamp CAS matched no row: artifact %s is missing or no longer submitted", art.ID)
			}
			return tx.ExternalActions.Record(cleanupCtx, orgID,
				githubApprovalAction(&submitted, userID, domain.ActionReviewSubmitted, domain.ArtifactStateReviewPending, domain.ArtifactStateReviewSubmitted))
		})
	}
	if err := stamp(); err != nil {
		artifactsLog.Warn("stamp submitted review onto artifact failed; retrying once", "artifact", art.ID, "error", err)
		if err := stamp(); err != nil {
			artifactsLog.Error("stamp submitted review onto artifact failed twice — artifact shows submitted without the posted review's id/URL",
				"artifact", art.ID, "review", submitted.ExternalID, "url", submitted.URL, "error", err)
		}
	}

	// Step 2: human verdict capture — diff the agent's proposed draft against the
	// human-edited final (body, event, the staged comments — all from the
	// post-claim re-read, so a last-moment edit shows up in the diff) into
	// run_memory.human_content.
	if fresh.RunID != "" {
		humanContent := FormatHumanFeedback(buildReviewHumanFeedbackInput(details, details.StagedComments))
		if err := ah.tx.WithTx(cleanupCtx, orgID, userID, func(tx db.TxStores) error {
			return tx.TaskMemory.UpdateRunMemoryHumanContent(cleanupCtx, orgID, fresh.RunID, humanContent)
		}); err != nil {
			artifactsLog.Warn("failed to record human verdict", "run", fresh.RunID, "error", err)
		}
	}

	// Step 3: terminal-on-last task closure. Approval is a decoupled sidecar — it
	// never flips run status or resumes/terminates a blueprint. The only lifecycle
	// effect is closing the task when this was the LAST unresolved artifact on an
	// already-terminal blueprint; otherwise a no-op.
	ah.closeTaskIfTerminalAndResolved(cleanupCtx, orgID, userID, fresh.RunID)

	// Tell the drafting agent its review was submitted (live or via the ledger).
	// submitted keeps art.ID — the review handle the agent spoke to start-review —
	// so the note references the id it already knows.
	ah.injectArtifactNote(orgID, submitted)

	writeJSON(w, http.StatusOK, map[string]any{
		"review_id": submitted.ExternalID,
		"url":       submitted.URL,
		"event":     submittedEvent,
		"state":     domain.ArtifactStateReviewSubmitted,
	})
}

// reviewDismiss resolves a pending review artifact by abandoning it: it flips the
// artifact pending → dismissed, then runs the terminal-on-last task-closure check.
// The per-item counterpart to reviewApprove; like approve it never touches the
// run's lifecycle. No GitHub call and NO external-actions audit row — the review
// is staged entirely TF-side (TFAC-494), so a dismiss is a purely local state
// change, not an org-credential write (external_actions records only writes). The
// flip is the whole resolution. An already-submitted / already-dismissed review
// is terminal → 409. A pending review is dismissable whether or not it reached the
// ready sentinel (a started-but-unfinalized review is still abandonable).
func (ah *artifactsHandler) reviewDismiss(w http.ResponseWriter, r *http.Request, orgID, userID string, art *domain.Artifact) {
	if art.State != domain.ArtifactStateReviewPending {
		writeJSON(w, http.StatusConflict, map[string]string{"error": "this review is no longer pending (state: " + art.State + ")"})
		return
	}

	// Flip pending → dismissed, pessimistic — the flip is the resolution. A CAS,
	// not an upsert: a dismiss racing an approve must lose cleanly (409) rather
	// than overwrite a just-submitted review as dismissed. The proposed snapshot
	// is preserved on the row; abandonment retires the staged draft, not the
	// record of what the agent wrote. No audit row: there is no org-credential
	// write to record (the draft never reached GitHub).
	dismissed := *art
	dismissed.State = domain.ArtifactStateReviewDismissed
	var flipped bool
	if err := ah.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		var e error
		flipped, e = tx.Artifacts.TransitionReviewState(r.Context(), orgID, art.ID,
			domain.ArtifactStateReviewPending, domain.ArtifactStateReviewDismissed, "", "", "")
		return e
	}); err != nil {
		internalError(w, "artifacts", err)
		return
	}
	if !flipped {
		writeJSON(w, http.StatusConflict, map[string]string{"error": "this review is no longer pending — it was just submitted or dismissed"})
		return
	}

	// The dismiss is fully resolved by the flip above (no GitHub object). Run the
	// terminal-on-last task-closure check detached so a client disconnect can't
	// strand it now that the artifact is resolved.
	cleanupCtx := context.WithoutCancel(r.Context())
	ah.closeTaskIfTerminalAndResolved(cleanupCtx, orgID, userID, art.RunID)
	// Tell the drafting agent its review was dismissed (live or via the ledger).
	ah.injectArtifactNote(orgID, dismissed)

	writeJSON(w, http.StatusOK, map[string]any{
		"review_id": art.ExternalID,
		"state":     domain.ArtifactStateReviewDismissed,
	})
}

// handleReviewRefresh re-reconciles a finalized review draft to the PR's current
// head (POST /api/artifacts/{id}/review/refresh, TFAC-500) — the human-driven
// counterpart to the agent's finalize gate, sharing the same forward-map
// (internal/review). Review-only; dispatches to reviewRefresh.
func (ah *artifactsHandler) handleReviewRefresh(w http.ResponseWriter, r *http.Request) {
	orgID, ok := requireOrg(w, r)
	if !ok {
		return
	}
	userID := ClaimsFrom(r.Context()).Subject
	art, ok := ah.loadArtifact(w, r, orgID, userID, r.PathValue("id"))
	if !ok {
		return
	}
	if art.Kind != domain.ArtifactKindReview {
		notFound(w, "review artifact")
		return
	}
	ah.reviewRefresh(w, r, orgID, userID, art)
}

// reviewRefresh advances a finalized review draft from the head it was written
// against to the PR's live head (TFAC-500): it forward-maps every staged comment,
// remaps + re-anchors survivors to the live head, DROPS the outdated ones (the
// human confirmed the count first), and re-pins the finalize frame (base+head) to
// the live PR so the overlay's diff and the comments move forward together. The
// surviving comments all share the live head afterward, so the atomic submit stays
// coherent — and the GC-422 risk shrinks (the pin follows a fresher commit).
//
// Pessimistic and atomic: a transient compare failure aborts before anything is
// persisted (never half-refresh against an unverified head). Refresh is optional —
// a review submits fine without it (GitHub renders post-finalize drift as
// "outdated" once submitted); this just clears that drift up front.
func (ah *artifactsHandler) reviewRefresh(w http.ResponseWriter, r *http.Request, orgID, userID string, art *domain.Artifact) {
	// Only a finalized, still-pending review can be refreshed: a submitted/dismissed
	// one is terminal, and an unfinalized draft is still the agent's to shape.
	if art.State != domain.ArtifactStateReviewPending {
		writeJSON(w, http.StatusConflict, map[string]string{"error": "this review is no longer pending (state: " + art.State + ")"})
		return
	}
	details, err := domain.ParseReviewArtifactDetails(art.DetailsJSON)
	if err != nil {
		internalError(w, "artifacts", fmt.Errorf("parse review artifact details (artifact %s): %w", art.ID, err))
		return
	}
	if details.ReviewEvent == "" {
		writeJSON(w, http.StatusConflict, map[string]string{"error": "this review has not been finalized by the agent yet"})
		return
	}

	gh, owner, repo, number, ok := ah.ghForArtifact(w, r, orgID, art)
	if !ok {
		return
	}
	pr, err := gh.GetPRBasic(r.Context(), owner, repo, number)
	if err != nil || pr.HeadSHA == "" {
		artifactsLog.Warn("review refresh: live PR head unavailable",
			"artifact", art.ID, "owner", owner, "repo", repo, "number", number, "error", err)
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "couldn't fetch the PR's current head from GitHub" + localDetail(err)})
		return
	}
	liveHead := pr.HeadSHA

	finalizeHead := details.FinalizedHeadSHA
	if finalizeHead == "" {
		finalizeHead = details.HeadSHA
	}

	lineMapFor := func(anchor string) (ghclient.LineMap, error) {
		cmp, e := gh.CompareCommits(r.Context(), owner, repo, anchor, liveHead)
		if e != nil {
			return ghclient.LineMap{}, e
		}
		return ghclient.ParseLineMapFromPatches(cmp.Files), nil
	}

	outcomes := review.ReconcileToHead(details.StagedComments, liveHead, finalizeHead, lineMapFor)
	survivors := make([]domain.ReviewArtifactComment, 0, len(outcomes))
	moved, dropped := 0, 0
	for _, o := range outcomes {
		// A transient compare failure aborts the whole refresh — better to leave the
		// review on its known-good finalize frame than to half-advance it against a
		// head we couldn't fully verify.
		if o.MapErr != nil {
			artifactsLog.Warn("review refresh: reconcile failed", "artifact", art.ID, "head", liveHead, "error", o.MapErr)
			writeJSON(w, http.StatusBadGateway, map[string]string{"error": "couldn't reconcile a comment against the current head" + localDetail(o.MapErr)})
			return
		}
		if o.Outdated() {
			dropped++ // the human confirmed dropping outdated comments before refreshing
			continue
		}
		if o.Status == ghclient.LineMapMoved {
			moved++
		}
		survivors = append(survivors, o.Comment)
	}

	// Re-pin the staged set + the finalize frame to the live PR. Survivors all share
	// liveHead now, so the submit stays atomic; the overlay re-renders the new frame.
	details.StagedComments = survivors
	details.FinalizedHeadSHA = liveHead
	details.FinalizedBaseSHA = pr.BaseSHA

	if !ah.updateReviewDetailsPending(w, r, orgID, userID, art.ID, details) {
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"moved":   moved,
		"dropped": dropped,
		"head":    liveHead,
		"state":   domain.ArtifactStateReviewPending,
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

	if !ah.updateReviewDetailsPending(w, r, orgID, userID, art.ID, details) {
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

	if !ah.updateReviewDetailsPending(w, r, orgID, userID, art.ID, details) {
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
