package server

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/sky-ai-eng/triage-factory/internal/agentmeta"
	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	ghclient "github.com/sky-ai-eng/triage-factory/internal/github"
	"github.com/sky-ai-eng/triage-factory/internal/review"
	"github.com/sky-ai-eng/triage-factory/internal/server/httpx"
)

// reviewArtifactDetailsJSON is the `details` payload for a review artifact.
// The whole review is staged TF-side: body/event and the inline
// comments all come from the artifact's details_json, applied to GitHub only by
// the atomic submit at approval. Each comment's severity is parsed back out of
// its body for the chip. The artifact's own identity — id, state, and the
// posted review's external_id + URL, both stamped at approval — rides the
// shared envelope around this, not a second copy in here.
type reviewArtifactDetailsJSON struct {
	Owner       string                      `json:"owner"`
	Repo        string                      `json:"repo"`
	PRNumber    int                         `json:"pr_number"`
	ReviewBody  string                      `json:"review_body"`
	ReviewEvent string                      `json:"review_event"`
	Comments    []reviewArtifactCommentJSON `json:"comments"`
	// CommitsSinceFinalize is how many commits the live PR head is ahead of the
	// finalize-time head — the "N commits since this review was written"
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

// reviewArtifactDetails composes the review `details` payload from the
// artifact's TF-side staged draft — no GitHub call for the content
// itself. Each comment body carries the severity badge baked in;
// ParseSeverityBadge splits it back into a chip level + clean body for display.
// Body + event are the staged values; the whole review is local until the
// atomic submit at approval.
func (ah *artifactsHandler) reviewArtifactDetails(w http.ResponseWriter, r *http.Request, orgID string, art *domain.Artifact) (reviewArtifactDetailsJSON, bool) {
	owner, repo, number, ok := domain.ParsePRTarget(art.Target)
	if !ok {
		internalError(w, "artifacts", fmt.Errorf("malformed artifact target %q (artifact %s)", art.Target, art.ID))
		return reviewArtifactDetailsJSON{}, false
	}
	// Surface a corrupt details_json as a 500 rather than serving a misleadingly
	// empty review (no body, no comments) the user might approve or act on.
	details, err := domain.ParseReviewArtifactDetails(art.DetailsJSON)
	if err != nil {
		internalError(w, "artifacts", fmt.Errorf("parse review artifact details (artifact %s): %w", art.ID, err))
		return reviewArtifactDetailsJSON{}, false
	}

	out := reviewArtifactDetailsJSON{
		Owner:       owner,
		Repo:        repo,
		PRNumber:    number,
		ReviewBody:  details.ReviewBody,
		ReviewEvent: details.ReviewEvent,
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
	return out, true
}

// patchReviewRequest is the body of PATCH /api/artifacts/{id}/review. The
// fields carry no review_ prefix: the sub-resource in the path already says
// which artifact half they belong to.
//
// state is the review's own lifecycle position, and the only value it accepts
// is "dismissed". Abandoning a staged review reaches nothing outside this
// process — the draft never left it — so it is an honest field write here
// rather than a verb, which is why /dismiss is now pull-request-only.
type patchReviewRequest struct {
	Body  *string `json:"body"`
	Event *string `json:"event"`
	State *string `json:"state"`
}

// handleArtifactReviewUpdate stages a new review body and/or event into the
// artifact's details_json, or dismisses the staged draft. TF-side only: a
// pending review's body + event are not live-editable on GitHub (the event
// *is* the submit), so they stage here and apply atomically at approval. No
// GitHub call on any arm.
//
// PATCH /api/artifacts/{id}/review
func (ah *artifactsHandler) handleArtifactReviewUpdate(w http.ResponseWriter, r *http.Request) {
	orgID, ok := requireOrg(w, r)
	if !ok {
		return
	}
	userID := ClaimsFrom(r.Context()).Subject
	id, ok := artifactIDOr404(w, r)
	if !ok {
		return
	}
	art, ok := ah.loadArtifact(w, r, orgID, userID, id)
	if !ok {
		return
	}
	if !requireArtifactKind(w, art, domain.ArtifactKindReview, "a review") {
		return
	}
	// Only a still-pending review can be staged or dismissed. A PATCH on a
	// submitted/dismissed artifact would write body/event that can never be
	// applied (approve guards state), so reject it as a conflict rather than
	// return a misleading 200 on an artifact the user can no longer act on.
	// Mirrors the reviewApprove guard.
	if art.State != domain.ArtifactStateReviewPending {
		writeReviewNotPending(w, art.State)
		return
	}

	var req patchReviewRequest
	if !httpx.DecodeJSONStrict(w, r, &req) {
		return
	}

	var v httpx.Validation
	if req.Body == nil && req.Event == nil && req.State == nil {
		httpx.WriteErrors(w, http.StatusBadRequest, httpx.ErrorItem{
			Reason:  httpx.ReasonMissingField,
			Message: "no fields to update: provide body, event, or state",
		})
		return
	}
	if req.Body != nil && strings.TrimSpace(*req.Body) == "" {
		// The summary body is what the review says. A whitespace-only one is as
		// empty as "" and would post a blank review at approval, so it is
		// refused here and the trimmed value is what gets stored.
		v.Invalid("body", "body cannot be empty or whitespace-only")
	}
	if req.Event != nil && !validReviewEvent(*req.Event) {
		// Validate here rather than deferring to approval-time GitHub
		// rejection. The event also doubles as the ready sentinel, so an empty
		// or bogus value would silently un-park the conversation as well as
		// fail the submit.
		v.Invalid("event", "event must be one of APPROVE, COMMENT, REQUEST_CHANGES")
	}
	if req.State != nil {
		if *req.State != domain.ArtifactStateReviewDismissed {
			v.Invalid("state", `state accepts only "dismissed"; submitting a review is POST …/approve`)
		} else if req.Body != nil || req.Event != nil {
			// Dismissal retires the draft; staging edits into the same request
			// asks for two different outcomes on one row. Say so rather than
			// pick an order.
			v.Invalid("state", "state cannot be combined with body or event — dismiss the review on its own")
		}
	}
	if v.Flush(w, http.StatusBadRequest) {
		return
	}

	if req.State != nil {
		ah.reviewDismiss(w, r, orgID, userID, art)
		return
	}

	details, err := domain.ParseReviewArtifactDetails(art.DetailsJSON)
	if err != nil {
		internalError(w, "artifacts", fmt.Errorf("parse review artifact details: %w", err))
		return
	}
	if req.Body != nil {
		details.ReviewBody = strings.TrimSpace(*req.Body)
	}
	if req.Event != nil {
		details.ReviewEvent = *req.Event
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
		conflict(w, "this review is no longer pending — it was just submitted or dismissed")
		return false
	}
	return true
}

// writeReviewNotPending answers a review-artifact mutation whose row has
// already settled: 409 ALREADY_TERMINAL naming the state it settled into. The
// CAS losers below are a different class — a race the caller resolves by
// re-reading — and keep the generic CONFLICT.
func writeReviewNotPending(w http.ResponseWriter, state string) {
	httpx.WriteErrors(w, http.StatusConflict, httpx.ErrorItem{
		Reason:  httpx.ReasonAlreadyTerminal,
		Message: "this review is no longer pending (state: " + state + ")",
	})
}

// reviewApprove creates and submits the staged review to GitHub atomically
// (SubmitReview — one POST carrying commit_id + event + body + footer + the staged
// comments[]), stamps the submitted review's id + URL onto the artifact, records
// the human verdict into conversation_memory, and runs the shared
// conversation/task/blueprint bookkeeping. Nothing touched GitHub before this
// point (the review was staged entirely TF-side), so concurrent runs on one PR
// each submit their own review here — GitHub allows unlimited submitted reviews
// per identity.
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
		writeReviewNotPending(w, art.State)
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
		conflict(w, "this review is no longer pending approval — it was just submitted or dismissed")
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
		conflict(w, "this review has not been finalized by the agent yet")
		return
	}

	// Publish through the shared submit (internal/review) — the same
	// function the auto-post posture calls from the agent's finalize choke point,
	// so the commit pin, the comment payload, and the composed deep link can't
	// drift between a human-approved and an auto-posted review.
	//
	// cleanupCtx is detached from the request: the post-write block below must not
	// turn on request liveness once the review is live on GitHub. That includes
	// the review URL's host resolution (WebBase, called only after the submit
	// lands) — a client disconnect between the submit and the stamp would
	// otherwise cancel the resolve and silently downgrade a GHES/GHEC org's link
	// to the deployment default.
	cleanupCtx := context.WithoutCancel(r.Context())
	res, err := review.SubmitStaged(r.Context(), gh, review.SubmitInput{
		Owner:   owner,
		Repo:    repo,
		Number:  number,
		Details: details,
		Footer:  agentmeta.Build(ah.conversations, orgID, fresh.ConversationID, "review"),
		WebBase: func() string {
			base, baseErr := ah.ghResolver.BaseURLFor(cleanupCtx, orgID)
			if baseErr != nil {
				artifactsLog.Warn("resolve github base for review URL failed; using default host",
					"artifact", art.ID, "org", orgID, "error", baseErr)
				return ""
			}
			return base
		},
	})
	if err != nil {
		var unanchored *review.UnanchoredCommentError
		if errors.As(err, &unanchored) {
			// Raised before the GitHub call — nothing was published, so this is a
			// content problem the user fixes in the overlay.
			releaseClaim()
			httpx.WriteErrors(w, http.StatusUnprocessableEntity, httpx.ErrorItem{
				Reason:  httpx.ReasonInvalidField,
				Message: unanchored.Error() + " — edit or delete it, then approve again",
			})
			return
		}
		var divergent *review.DivergentAnchorsError
		if errors.As(err, &divergent) {
			// Also pre-GitHub. Reported as its own 422 rather than folded into the
			// 502 below, because "GitHub API error" would point the user at an
			// outage for a draft that never left this process — and Refresh, which
			// re-pins every surviving comment to the live head, is the fix.
			artifactsLog.Error("staged review comments disagree on their anchor commit",
				"artifact", art.ID, "owner", owner, "repo", repo, "number", number, "error", err)
			releaseClaim()
			httpx.WriteErrors(w, http.StatusUnprocessableEntity, httpx.ErrorItem{
				Reason:  httpx.ReasonInvalidField,
				Message: divergent.Error() + " — refresh the review, then approve again",
			})
			return
		}
		artifactsLog.Warn("SubmitReview failed",
			"artifact", art.ID, "owner", owner, "repo", repo, "number", number, "error", err)
		// Release the claim so the user can retry — nothing reached GitHub.
		releaseClaim()
		writeUpstreamGitHub(w, "GitHub API error", err)
		return
	}
	submittedEvent := res.Event
	// Record the event we submitted so a later reader — and the
	// proposed-vs-final diff below — sees what was sent.
	details.ReviewEvent = submittedEvent

	// Step 1: stamp the submitted review's id + URL onto the claimed artifact (a
	// submitted review finally has both — a never-published draft had neither) and
	// the actual submitted event into details, and compose the audit row with the
	// stamp (TFAC-483). The org-App submit is a human-authorized, org-executed
	// write — conversation_id is the drafting conversation, actor is the approver. The state is
	// already submitted (the claim), so this is a submitted→submitted CAS carrying
	// the coordinates; retried once, and a double failure is loud — until the
	// stamp lands the row shows submitted with no URL, which is why this must not
	// stay a silent Warn.
	//
	// The credential is classified outside the stamp tx, against the same repo
	// the submit just went to, so the row names the credential that posted it.
	credential := githubCredentialFor(cleanupCtx, ah.ghResolver, orgID, owner, repo)
	submitted := *fresh
	submitted.State = domain.ArtifactStateReviewSubmitted
	submitted.ExternalID = strconv.Itoa(res.ReviewID)
	submitted.URL = res.URL
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
				githubApprovalAction(&submitted, userID, domain.ActionReviewSubmitted, domain.ArtifactStateReviewPending, domain.ArtifactStateReviewSubmitted, credential))
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
	// conversation_memory.human_content.
	if fresh.ConversationID != "" {
		humanContent := FormatHumanFeedback(buildReviewHumanFeedbackInput(details, details.StagedComments))
		if err := ah.tx.WithTx(cleanupCtx, orgID, userID, func(tx db.TxStores) error {
			_, err := tx.TaskMemory.UpdateConversationMemoryHumanContent(cleanupCtx, orgID, fresh.ConversationID, humanContent)
			return err
		}); err != nil {
			artifactsLog.Warn("failed to record human verdict", "conversation", fresh.ConversationID, "error", err)
		}
	}

	// Step 3: terminal-on-last task closure. Approval is a decoupled sidecar — it
	// never flips conversation status or resumes/terminates a blueprint. The
	// only lifecycle effect is closing the task when this was the LAST
	// unresolved artifact on an already-terminal blueprint; otherwise a no-op.
	ah.pingConversationsResolved(orgID)
	ah.closeTaskIfTerminalAndResolved(cleanupCtx, orgID, userID, fresh.ConversationID)

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
// conversation's lifecycle. No GitHub call and NO external-actions audit row — the review
// is staged entirely TF-side, so a dismiss is a purely local state
// change, not an org-credential write (external_actions records only writes). The
// flip is the whole resolution. A pending review is dismissable whether or not it
// reached the ready sentinel (a started-but-unfinalized review is still
// abandonable).
//
// It is the state="dismissed" arm of the review PATCH, which has already
// established the kind and the pending state; a terminal row never reaches
// here.
func (ah *artifactsHandler) reviewDismiss(w http.ResponseWriter, r *http.Request, orgID, userID string, art *domain.Artifact) {
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
		conflict(w, "this review is no longer pending — it was just submitted or dismissed")
		return
	}

	// The dismiss is fully resolved by the flip above (no GitHub object). Run the
	// terminal-on-last task-closure check detached so a client disconnect can't
	// strand it now that the artifact is resolved.
	cleanupCtx := context.WithoutCancel(r.Context())
	ah.pingConversationsResolved(orgID)
	ah.closeTaskIfTerminalAndResolved(cleanupCtx, orgID, userID, art.ConversationID)
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
	id, ok := artifactIDOr404(w, r)
	if !ok {
		return
	}
	art, ok := ah.loadArtifact(w, r, orgID, userID, id)
	if !ok {
		return
	}
	if !requireArtifactKind(w, art, domain.ArtifactKindReview, "a review") {
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
		writeReviewNotPending(w, art.State)
		return
	}
	details, err := domain.ParseReviewArtifactDetails(art.DetailsJSON)
	if err != nil {
		internalError(w, "artifacts", fmt.Errorf("parse review artifact details (artifact %s): %w", art.ID, err))
		return
	}
	if details.ReviewEvent == "" {
		conflict(w, "this review has not been finalized by the agent yet")
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
		writeUpstreamGitHub(w, "couldn't fetch the PR's current head from GitHub", err)
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
			writeUpstreamGitHub(w, "couldn't reconcile a comment against the current head", o.MapErr)
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

// handleArtifactCommentUpdate edits one staged inline comment on the review
// draft. Review-only, no GitHub call — the whole review is staged TF-side until
// approval. The comment's severity is fixed at creation and is
// re-baked onto the human's edited body from the stored comment, so an edit
// preserves the chip without being able to move it.
//
// A body arriving with a severity badge of its own is refused rather than
// silently stripped and re-baked: the caller asked for a severity the route
// will not honour, and answering 200 to that reads as "accepted" for a value
// that was thrown away. The overlay never sends one — it edits the clean body
// the read hands it.
//
// PATCH /api/artifacts/{id}/comments/{commentId}
func (ah *artifactsHandler) handleArtifactCommentUpdate(w http.ResponseWriter, r *http.Request) {
	orgID, ok := requireOrg(w, r)
	if !ok {
		return
	}
	userID := ClaimsFrom(r.Context()).Subject
	id, ok := artifactIDOr404(w, r)
	if !ok {
		return
	}
	commentID := r.PathValue("commentId")

	// The resource answers before the payload does, as on every kind-scoped
	// write here: which artifact this is decides whether the route applies at
	// all.
	art, ok := ah.loadArtifact(w, r, orgID, userID, id)
	if !ok {
		return
	}
	if !requireArtifactKind(w, art, domain.ArtifactKindReview, "a review") {
		return
	}
	// Only a still-pending review can be edited: once submitted, its comments are
	// public and immutable through this path.
	if art.State != domain.ArtifactStateReviewPending {
		writeReviewNotPending(w, art.State)
		return
	}

	var req struct {
		Body string `json:"body"`
	}
	if !httpx.DecodeJSONStrict(w, r, &req) {
		return
	}
	if strings.TrimSpace(req.Body) == "" {
		// Trim before the emptiness check and store what was validated: a
		// whitespace-only comment body is as empty as "" and would otherwise
		// post a blank comment to GitHub.
		httpx.WriteErrors(w, http.StatusBadRequest, httpx.ErrorItem{
			Reason: httpx.ReasonMissingField, Message: "body is required", Field: "body",
		})
		return
	}
	req.Body = strings.TrimSpace(req.Body)
	if sev, _ := domain.ParseSeverityBadge(req.Body); sev != "" {
		httpx.WriteErrors(w, http.StatusUnprocessableEntity, httpx.ErrorItem{
			Reason:  httpx.ReasonInvalidField,
			Message: "severity is set at creation and cannot be changed by editing the body",
			Field:   "body",
		})
		return
	}
	details, err := domain.ParseReviewArtifactDetails(art.DetailsJSON)
	if err != nil {
		internalError(w, "artifacts", fmt.Errorf("parse review artifact details: %w", err))
		return
	}
	// Find the staged comment by its local id and re-bake its current severity onto
	// the human's edit.
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
	details.StagedComments[idx].Body = domain.SeverityBadgeMarkdown(severity) + req.Body

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
	id, ok := artifactIDOr404(w, r)
	if !ok {
		return
	}
	commentID := r.PathValue("commentId")

	art, ok := ah.loadArtifact(w, r, orgID, userID, id)
	if !ok {
		return
	}
	if !requireArtifactKind(w, art, domain.ArtifactKindReview, "a review") {
		return
	}
	if art.State != domain.ArtifactStateReviewPending {
		writeReviewNotPending(w, art.State)
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
// conversation_memory.human_content. Comments are joined by their stable TF-local id;
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
// as the ready sentinel, so clearing it would un-park the conversation.
func validReviewEvent(e string) bool {
	switch e {
	case "APPROVE", "COMMENT", "REQUEST_CHANGES":
		return true
	default:
		return false
	}
}
