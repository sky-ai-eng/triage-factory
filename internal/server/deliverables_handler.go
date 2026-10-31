package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/sky-ai-eng/triage-factory/internal/agentmeta"
	"github.com/sky-ai-eng/triage-factory/internal/conversationevent"
	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/delegate"
	"github.com/sky-ai-eng/triage-factory/internal/deliverableteardown"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	ghclient "github.com/sky-ai-eng/triage-factory/internal/github"
	"github.com/sky-ai-eng/triage-factory/internal/reconcile"
	"github.com/sky-ai-eng/triage-factory/internal/server/httpx"
	"github.com/sky-ai-eng/triage-factory/pkg/websocket"
)

// deliverablesHandler serves the deliverable-id-addressed endpoints that back the
// GitHub-native PR preview. It replaces the local pending_prs path:
// the draft PR is a real GitHub object created at `pr create` time and recorded
// as a `pull_request` deliverable, so these handlers read/edit/approve that live
// object 1:1 instead of staging a local row.
//
// ghResolver picks the right GitHub client (org App installation token → PAT)
// per repo at call time, so App-only orgs work identically to PAT orgs. Mirrors
// dashboardHandler's deps.
type deliverablesHandler struct {
	tx         db.TxRunner
	ws         *websocket.Hub
	ghResolver ghclient.Resolver
	// spawner is a lazy delegation-spawner accessor (wired by Server.routes via a
	// closure over s.spawner) used to feed the drafting agent a <system-note> when
	// a human resolves one of its deliverables.
	spawner func() *delegate.Spawner
	// reconciler is the Tier-2 deliverable reconciler (nil where the deployment
	// runs none), read lazily like spawner. The resolve verbs use it when the
	// live PR turns out to have been resolved on GitHub already, so the row
	// catches up before the caller is told to look again.
	reconciler func() *reconcile.Reconciler
	// publicURL is the deployment's externally-visible base ("" until the
	// deploy config lands, or when none is configured), read lazily because
	// the deploy config is wired after routes are registered. It is what the
	// disclosure footer on a human-approved review links the run from.
	publicURL func() string
}

// footerFor is the disclosure footer for content a conversation authored — the
// same shape the agenthost appends when the run itself publishes, so a review
// posted by a human's approval and one auto-posted at finalize disclose
// identically. Empty with no conversation: nothing to disclose.
func (ah *deliverablesHandler) footerFor(kind, orgID, conversationID string) string {
	if conversationID == "" {
		return ""
	}
	return agentmeta.Build(kind, agentmeta.PublishedRunURL(ah.publicURL(), orgID, conversationID))
}

// deliverableIDOr404 guards the {id} path value on every deliverable-addressed route
// — deliverables.id is a uuid column on Postgres. See uuidPathOr404.
func deliverableIDOr404(w http.ResponseWriter, r *http.Request) (string, bool) {
	return uuidPathOr404(w, r, "id", "deliverable")
}

// writeUpstreamGitHub answers a GitHub call that failed: 502 with a static,
// author-written prefix and the raw detail appended only in local mode (the
// LocalDetail seam). The caller logs the error itself — this only shapes the
// response.
func writeUpstreamGitHub(w http.ResponseWriter, msg string, err error) {
	httpx.WriteErrors(w, http.StatusBadGateway, httpx.ErrorItem{
		Reason:  httpx.ReasonUpstreamUnavailable,
		Message: msg + httpx.LocalDetail(err),
	})
}

// injectDeliverableNote feeds the deliverable's drafting conversation the agent-
// facing <system-note> for a just-completed resolution. Fully decoupled from
// the resolution itself: a live conversation is steered (the actual delivery
// runs on a detached goroutine inside the spawner, so this returns immediately
// and never blocks the response); a terminal/paused conversation gets nothing
// here and re-derives the same note from the deliverable row into its ledger on
// the next resume. Never gates the blueprint (TFAC-379 #2). The deliverable
// passed must carry its post-resolution State so the right kind-specific copy
// is rendered.
func (ah *deliverablesHandler) injectDeliverableNote(orgID string, a domain.Deliverable) {
	if a.ConversationID == "" || ah.spawner == nil {
		return
	}
	sp := ah.spawner()
	if sp == nil {
		return
	}
	sp.InjectDeliverableNote(orgID, a.ConversationID, a)
}

// prDeliverableDetailsJSON is the `details` payload for a pull_request deliverable.
// Title/Body are the live values pulled from GitHub (GetPRBasic) so the editor
// renders the current PR, not a stale snapshot; owner/repo/number are the
// deliverable's target parsed once here rather than by every client.
type prDeliverableDetailsJSON struct {
	Owner      string `json:"owner"`
	Repo       string `json:"repo"`
	Number     int    `json:"number"`
	HeadBranch string `json:"head_branch"`
	BaseBranch string `json:"base_branch"`
	Title      string `json:"title"`
	Body       string `json:"body"`
}

// composedDeliverableDetails marshals a kind's composed `details` payload into
// the read shape's embedded-JSON slot. The payloads are plain structs, so a
// marshal failure is a programming error rather than a runtime condition —
// it surfaces as a 500 rather than a deliverable quietly served with null
// details.
func composedDeliverableDetails(w http.ResponseWriter, art *domain.Deliverable, details any) (json.RawMessage, bool) {
	raw, err := json.Marshal(details)
	if err != nil {
		internalError(w, "deliverables", fmt.Errorf("marshal %s details (deliverable %s): %w", art.Kind, art.ID, err))
		return nil, false
	}
	return raw, true
}

// requireDeliverableKind refuses a kind-scoped write against a deliverable of some
// other kind: 409, because the row exists and it is its kind that says no. A
// 404 here would conflate "no such deliverable" with "that operation does not
// apply to this one", and a 200 over an ignored body would claim a write
// nobody made.
//
// Every caller runs it before resolving an upstream client, so a wrong-kind
// write cannot reach GitHub.
func requireDeliverableKind(w http.ResponseWriter, art *domain.Deliverable, kind, article string) bool {
	if art.Kind == kind {
		return true
	}
	conflict(w, "deliverable is not "+article+" (kind: "+art.Kind+")")
	return false
}

// loadDeliverable fetches a deliverable by id and 404s if missing. It is the shared
// first step of every deliverable-id-addressed route; the caller then dispatches on
// art.Kind (pull_request vs review). Returns ok=false (after writing the error
// response) so callers can `if ...; !ok { return }`.
func (ah *deliverablesHandler) loadDeliverable(w http.ResponseWriter, r *http.Request, orgID, userID, id string) (art *domain.Deliverable, ok bool) {
	if err := ah.tx.WithReadTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		var e error
		art, e = tx.Deliverables.Get(r.Context(), orgID, id)
		return e
	}); err != nil {
		internalError(w, "deliverables", err)
		return nil, false
	}
	if art == nil {
		notFound(w, "deliverable")
		return nil, false
	}
	return art, true
}

// deliverablePullRequest resolves the pull request a PR or review deliverable is
// about under the caller's claims: the repository registry row its entity
// references, read by id, and the number the entity's key carries
// (db.ResolveDeliverablePullRequest).
func (ah *deliverablesHandler) deliverablePullRequest(ctx context.Context, orgID string, art *domain.Deliverable) (db.DeliverablePullRequest, error) {
	var pr db.DeliverablePullRequest
	err := ah.tx.WithReadTx(ctx, orgID, ClaimsFrom(ctx).Subject, func(tx db.TxStores) error {
		var e error
		pr, e = db.ResolveDeliverablePullRequest(ctx, orgID, *art, tx.Entities.Get, tx.Repos.Get)
		return e
	})
	return pr, err
}

// retiredRepositoryMessage is the refusal for acting on a pull request whose
// repository's registry row is retired.
const retiredRepositoryMessage = "this pull request's repository is no longer on GitHub under the name Triage Factory holds for it; it can only be acted on from GitHub"

// ghForDeliverable resolves the pull request a PR or review deliverable is about
// (deliverablePullRequest) and a per-repo GitHub client for its repository's
// current name. Writes the error response and returns ok=false on any failure.
//
// The client talks to the org's GitHub host now, where the same owner/repo#N
// is a different pull request than one recorded on another host, so a
// deliverable from another host is refused with a 409 (deliverableOnCurrentHost). So
// is one whose repository's registry row is retired: GitHub serves its name as
// another repository now, and a request under it would reach that one's pull
// request of the same number.
func (ah *deliverablesHandler) ghForDeliverable(w http.ResponseWriter, r *http.Request, orgID string, art *domain.Deliverable) (gh *ghclient.Client, pr db.DeliverablePullRequest, ok bool) {
	pr, err := ah.deliverablePullRequest(r.Context(), orgID, art)
	if err != nil {
		// A deliverable that names no pull request can't be acted on. Treat it
		// as a server-side data error rather than guessing — the writers always
		// link a pull request's deliverable to its entity.
		internalError(w, "deliverables", err)
		return nil, db.DeliverablePullRequest{}, false
	}
	onHost, host, err := ah.deliverableOnCurrentHost(r.Context(), orgID, art)
	if err != nil {
		internalError(w, "deliverables", err)
		return nil, db.DeliverablePullRequest{}, false
	}
	if !onHost {
		httpx.WriteErrors(w, http.StatusConflict, httpx.ErrorItem{
			Reason:  httpx.ReasonConflict,
			Message: fmt.Sprintf("this pull request is on %s, and the organization's GitHub is %s; it can only be acted on from an organization on its own GitHub", art.Scope, hostOrNotConfigured(host)),
		})
		return nil, db.DeliverablePullRequest{}, false
	}
	if pr.Retired {
		httpx.WriteErrors(w, http.StatusConflict, httpx.ErrorItem{Reason: httpx.ReasonConflict, Message: retiredRepositoryMessage})
		return nil, db.DeliverablePullRequest{}, false
	}

	// Resolve per-repo (org App installation token → PAT): App-only orgs (no PAT)
	// resolve a client here instead of 503-ing on a nil global client.
	client, err := ah.ghResolver.ClientForRepo(r.Context(), orgID, pr.Owner, pr.Repo)
	if err != nil {
		if errors.Is(err, ghclient.ErrNoGitHubCredentials) {
			deliverablesLog.Warn("github not configured", "org", orgID, "owner", pr.Owner, "repo", pr.Repo, "error", err)
			writeNotConfigured(w, "GitHub is not connected for this organization")
			return nil, db.DeliverablePullRequest{}, false
		}
		internalError(w, "deliverables", err)
		return nil, db.DeliverablePullRequest{}, false
	}
	return client, pr, true
}

// handleDeliverableGet serves one deliverable of ANY kind, in the same shape the
// conversation-scoped list serves it (deliverableJSON): the kind-independent
// envelope, with `kind` as the explicit discriminator over a kind-shaped
// `details` payload. Only the two kinds with a composed representation — a
// pull request read live from GitHub, a review assembled from its staged draft
// — do work here; every other kind hands back the details_json the row already
// stores.
//
// Serving every kind is what keeps this route from answering not-found for a
// deliverable that plainly exists: nonexistence and "no representation for that
// kind" are different answers, and a branch or comment deliverable reads the same
// way here as it does in the conversation-scoped list.
//
// GET /api/deliverables/{id}
func (ah *deliverablesHandler) handleDeliverableGet(w http.ResponseWriter, r *http.Request) {
	orgID, ok := requireOrg(w, r)
	if !ok {
		return
	}
	userID := ClaimsFrom(r.Context()).Subject
	id, ok := deliverableIDOr404(w, r)
	if !ok {
		return
	}

	art, ok := ah.loadDeliverable(w, r, orgID, userID, id)
	if !ok {
		return
	}
	out := deliverableEnvelope(*art)
	switch art.Kind {
	case domain.DeliverableKindPullRequest:
		details, ok := ah.prDeliverableDetails(w, r, orgID, art)
		if !ok {
			return
		}
		if out.Details, ok = composedDeliverableDetails(w, art, details); !ok {
			return
		}
	case domain.DeliverableKindReview:
		details, ok := ah.reviewDeliverableDetails(w, r, orgID, art)
		if !ok {
			return
		}
		if out.Details, ok = composedDeliverableDetails(w, art, details); !ok {
			return
		}
	default:
		out.Details = storedDeliverableDetails(*art)
	}
	writeJSON(w, http.StatusOK, out)
}

// prDeliverableDetails composes the pull_request `details` payload: the live PR
// title and body fetched from GitHub (1:1 display), over the deliverable's stored
// coordinates. On a live-fetch failure it degrades to the proposed/edited
// snapshot in details_json so the overlay still renders — a closed/merged PR or
// a transient GitHub blip shouldn't blank the editor.
//
// The coordinates are the pull request's repository as its registry row names
// it now, and the number its entity's key carries (deliverablePullRequest).
//
// A deliverable recorded on another GitHub host serves its snapshot with no live
// fetch: the org's client would read a different pull request under the same
// coordinates. So does one whose repository's registry row is retired, whose
// name GitHub now serves as another repository.
func (ah *deliverablesHandler) prDeliverableDetails(w http.ResponseWriter, r *http.Request, orgID string, art *domain.Deliverable) (prDeliverableDetailsJSON, bool) {
	resolved, err := ah.deliverablePullRequest(r.Context(), orgID, art)
	if err != nil {
		internalError(w, "deliverables", err)
		return prDeliverableDetailsJSON{}, false
	}
	owner, repo, number := resolved.Owner, resolved.Repo, resolved.Number
	onHost, _, err := ah.deliverableOnCurrentHost(r.Context(), orgID, art)
	if err != nil {
		internalError(w, "deliverables", err)
		return prDeliverableDetailsJSON{}, false
	}

	// A details parse failure is deliberately swallowed on this read: the
	// fields it would supply are the fallback for a failed live fetch, so a
	// corrupt row degrades to empty title/body rather than failing an overlay
	// that could still render the live PR.
	details, _ := domain.ParsePRDeliverableDetails(art.DetailsJSON)
	title := details.Snapshot.Title
	body := details.Snapshot.Body
	if !onHost || resolved.Retired {
		return prDeliverableDetailsJSON{
			Owner: owner, Repo: repo, Number: number,
			HeadBranch: details.HeadBranch, BaseBranch: details.Base,
			Title: title, Body: body,
		}, true
	}
	gh, _, ok := ah.ghForDeliverable(w, r, orgID, art)
	if !ok {
		return prDeliverableDetailsJSON{}, false
	}
	if pr, err := gh.GetPRBasic(r.Context(), owner, repo, number); err != nil {
		deliverablesLog.Warn("live PR fetch failed; falling back to snapshot",
			"deliverable", art.ID, "owner", owner, "repo", repo, "number", number, "error", err)
	} else {
		title = pr.Title
		body = pr.Body
	}

	return prDeliverableDetailsJSON{
		Owner:      owner,
		Repo:       repo,
		Number:     number,
		HeadBranch: details.HeadBranch,
		BaseBranch: details.Base,
		Title:      title,
		Body:       body,
	}, true
}

// handleDeliverablePRUpdate edits the live PR's title/body 1:1 via UpdatePR and
// refreshes the deliverable's details_json snapshot. Pessimistic by contract: a
// GitHub failure returns non-2xx and leaves the snapshot untouched, so the
// frontend never shows a green "saved" over a write GitHub rejected. The proposed
// snapshot (the agent's draft) is preserved — only the mutable snapshot moves.
//
// Two refusals come before the GitHub client is even resolved, and that
// ordering is the point of the route: a deliverable of another kind is a 409,
// and a body naming neither field is a 400. Between them, no request that
// describes no edit can reach UpdatePR — which would otherwise rewrite the PR
// with its own current content and record an audit row for it.
//
// PATCH /api/deliverables/{id}/pr
func (ah *deliverablesHandler) handleDeliverablePRUpdate(w http.ResponseWriter, r *http.Request) {
	orgID, ok := requireOrg(w, r)
	if !ok {
		return
	}
	userID := ClaimsFrom(r.Context()).Subject
	id, ok := deliverableIDOr404(w, r)
	if !ok {
		return
	}

	art, ok := ah.loadDeliverable(w, r, orgID, userID, id)
	if !ok {
		return
	}
	if !requireDeliverableKind(w, art, domain.DeliverableKindPullRequest, "a pull request") {
		return
	}

	var req struct {
		Title *string `json:"title"`
		Body  *string `json:"body"`
	}
	if !httpx.DecodeJSONStrict(w, r, &req) {
		return
	}
	if req.Title == nil && req.Body == nil {
		httpx.WriteErrors(w, http.StatusBadRequest, httpx.ErrorItem{
			Reason:  httpx.ReasonMissingField,
			Message: "no fields to update: provide title, body, or both",
		})
		return
	}

	gh, prRef, ok := ah.ghForDeliverable(w, r, orgID, art)
	if !ok {
		return
	}
	owner, repo, number := prRef.Owner, prRef.Repo, prRef.Number

	// Whole-field replace: UpdatePR rewrites both title and body, so a PATCH that
	// touches only one field must fill the other with the PR's CURRENT live value,
	// not a cached snapshot. Using the snapshot would silently revert a field the
	// human (or anyone) edited directly on GitHub since we last cached it — a lost
	// update. We only need the live read when a field is omitted; if the client
	// sent both, skip the round-trip.
	details, derr := domain.ParsePRDeliverableDetails(art.DetailsJSON)
	var title, body string
	if req.Title == nil || req.Body == nil {
		live, err := gh.GetPRBasic(r.Context(), owner, repo, number)
		if err != nil {
			// We can't reconstruct the omitted field without the current PR state,
			// and guessing from a stale snapshot risks clobbering. Fail loudly.
			deliverablesLog.Warn("GetPR for partial-edit baseline failed", "deliverable", art.ID, "owner", owner, "repo", repo, "number", number, "error", err)
			writeUpstreamGitHub(w, "couldn't read the current PR to apply a partial edit", err)
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
		httpx.WriteErrors(w, http.StatusBadRequest, httpx.ErrorItem{
			Reason: httpx.ReasonInvalidField, Message: "title cannot be empty or whitespace-only", Field: "title",
		})
		return
	}

	// Live edit FIRST (pessimistic): if GitHub rejects it, surface the error and
	// don't move the snapshot. liftValidationErr inside UpdatePR turns a 422 into
	// a readable reason.
	if err := gh.UpdatePR(r.Context(), owner, repo, number, title, body); err != nil {
		deliverablesLog.Warn("UpdatePR failed", "deliverable", art.ID, "owner", owner, "repo", repo, "number", number, "error", err)
		writeUpstreamGitHub(w, "GitHub API error", err)
		return
	}

	// Audit the org-credential PR edit (TFAC-483). The GitHub write already landed
	// (pessimistic); record best-effort in its own tx so it fires regardless of
	// the snapshot refresh below (which is itself best-effort and skipped when
	// details don't parse). No state transition — it's an in-place title/body edit.
	recordExternalActionBestEffort(r.Context(), ah.tx, orgID, userID,
		personDeliverableAction(art, userID, domain.ActionPREdited, "", "",
			ghclient.CredentialForRepo(r.Context(), ah.ghResolver, orgID, owner, repo)))

	// Refresh the deliverable's mutable snapshot to the new title/body; proposed
	// stays frozen. Best-effort-but-reported: the GitHub edit already landed, so
	// a snapshot-write failure isn't fatal to the user's edit, but we log it.
	// Skip when details didn't parse — re-marshaling a zero-value details would
	// blank the proposed (agent draft) baseline the approve diff needs.
	if derr != nil {
		deliverablesLog.Warn("PR deliverable details unparseable; skipping snapshot refresh after edit", "deliverable", art.ID, "error", derr)
	} else {
		details.Snapshot = domain.PRDeliverableSnapshot{Title: title, Body: body}
		if err := ah.upsertPRDetails(r.Context(), orgID, userID, art, details); err != nil {
			deliverablesLog.Warn("refresh PR deliverable snapshot failed (GitHub edit applied)", "deliverable", art.ID, "error", err)
		}
	}

	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// handleDeliverableDiff returns the unified diff the overlay renders. A PR deliverable
// (and a legacy/comment-less review with no stored frame) renders the LIVE PR diff
// at its current head. A review deliverable with a stored finalize frame renders that
// frame — FinalizedBaseSHA...FinalizedHeadSHA, the PR diff as of finalize — so
// every staged comment anchors in the frame it was written against (TFAC-500);
// drift since is conveyed by the per-comment freshness badges + the
// commits-since-finalize count, and reconciled by Refresh. Both frames port the
// 406→per-file fallback: GitHub refuses the verbatim diff media type on very large
// diffs, so reassemble from the files API rather than 502-ing the overlay.
func (ah *deliverablesHandler) handleDeliverableDiff(w http.ResponseWriter, r *http.Request) {
	orgID, ok := requireOrg(w, r)
	if !ok {
		return
	}
	userID := ClaimsFrom(r.Context()).Subject
	id, ok := deliverableIDOr404(w, r)
	if !ok {
		return
	}

	art, ok := ah.loadDeliverable(w, r, orgID, userID, id)
	if !ok {
		return
	}
	if art.Kind != domain.DeliverableKindPullRequest && art.Kind != domain.DeliverableKindReview {
		notFound(w, "deliverable")
		return
	}
	gh, prRef, ok := ah.ghForDeliverable(w, r, orgID, art)
	if !ok {
		return
	}
	owner, repo, number := prRef.Owner, prRef.Repo, prRef.Number

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
				httpx.WriteErrors(w, http.StatusNotFound, httpx.ErrorItem{
					Reason:  httpx.ReasonNotFound,
					Message: "file " + file + " is not part of this review's diff (or lies beyond the file-listing cap)",
					Field:   "file",
				})
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
						httpx.WriteErrors(w, http.StatusNotFound, httpx.ErrorItem{
							Reason:  httpx.ReasonNotFound,
							Message: "file " + file + " is not part of this PR's diff (or lies beyond the file-listing cap)",
							Field:   "file",
						})
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
		deliverablesLog.Error("deliverable diff failed",
			"deliverable", art.ID, "owner", owner, "repo", repo, "number", number, "finalize_frame", useFin, "error", err)
		writeUpstreamGitHub(w, "GitHub API error", err)
		return
	}

	if truncationNote != "" {
		w.Header().Set("X-Diff-Truncated", truncationNote)
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Write([]byte(diff))
}

// reviewFinalizeFrame returns the stored finalize-time base+head SHAs for a review
// deliverable (TFAC-500), or ("","") for a PR deliverable, a malformed/legacy review
// with no stored frame, or a comment-less review (which records no frame). Both
// must be present for the caller to render the finalize frame.
func reviewFinalizeFrame(art *domain.Deliverable) (base, head string) {
	if art.Kind != domain.DeliverableKindReview {
		return "", ""
	}
	d, err := domain.ParseReviewDeliverableDetails(art.DetailsJSON)
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

// handleDeliverableApprove promotes the draft PR to ready-for-review: it marks the
// PR ready (MarkPRReady) and flips the deliverable to state=open. Approval is a
// decoupled sidecar — it
// does NOT flip conversation status or resume/terminate a blueprint; the only
// lifecycle effect is the shared terminal-on-last task-closure check
// (closeTaskIfTerminalAndResolved), which closes the task iff this was the last
// unresolved deliverable on a cleanly-completed blueprint run.
//
// The PR's title and body are left exactly as they are: the disclosure footer
// is already on the body from creation, and a human may be editing the
// description on GitHub at this very moment, so approval writes nothing it
// would have to read first. The content the bookkeeping records is read LIVE
// from GitHub (never the cached snapshot), so a stale or malformed snapshot
// can neither misreport a direct GitHub edit nor blank the recorded body. The
// one GitHub mutation comes first and is pessimistic (non-2xx on failure);
// everything after is detached best-effort bookkeeping — the PR is already
// ready, so a client disconnect must not strand the conversation/task
// half-flipped.
// liveDraftOr409 reads the PR from GitHub and confirms it is still the open
// draft the deliverable row says it is. Both resolve verbs run it before writing:
// a draft the human is looking at can have been closed, merged, or marked
// ready on GitHub since the overlay loaded, and acting on the stale row would
// promote a closed PR (a GitHub error dressed as a 502) or delete the branch
// under a PR somebody merged. When the live state has moved on, the row is
// reconciled right here rather than left for the next pass, and the caller is
// answered 409 ALREADY_TERMINAL naming what GitHub holds — the same fault a
// stale click on an already-resolved row gets, because that is what it is. A
// read failure is a 502: nothing can be safely promoted or deleted unread.
func (ah *deliverablesHandler) liveDraftOr409(w http.ResponseWriter, r *http.Request, gh *ghclient.Client, orgID, userID string, art *domain.Deliverable, owner, repo string, number int) (*ghclient.PRView, bool) {
	live, err := gh.GetPRBasic(r.Context(), owner, repo, number)
	if err != nil {
		deliverablesLog.Warn("live PR read before resolve failed", "deliverable", art.ID, "owner", owner, "repo", repo, "number", number, "error", err)
		writeUpstreamGitHub(w, "couldn't read the PR on GitHub", err)
		return nil, false
	}
	stillDraft := live.Draft && !live.Merged && strings.EqualFold(live.State, "open")
	if stillDraft {
		return live, true
	}
	ah.reconcileDeliverableOutOfBand(r, orgID, userID, art)
	httpx.WriteErrors(w, http.StatusConflict, httpx.ErrorItem{
		Reason:  httpx.ReasonAlreadyTerminal,
		Message: "this PR was already resolved on GitHub outside Triage Factory (" + liveStateWord(live) + "); its record has been refreshed",
	})
	return nil, false
}

// liveStateWord names a live PR's state for a human: merged, closed, or open
// for review.
func liveStateWord(live *ghclient.PRView) string {
	switch {
	case live.Merged:
		return "merged"
	case strings.EqualFold(live.State, "closed"):
		return "closed"
	default:
		return "marked ready for review"
	}
}

// reconcileDeliverableOutOfBand runs the Tier-2 reconcile over the deliverable's
// conversation — the same working set the refresh route drives — so a row a
// resolve verb found stale on GitHub catches up now: state, terminal-on-last
// task closure, and the deliverable_updated broadcast that makes the overlay
// refetch. Best-effort and detached from the request: the 409 the
// caller is about to send is correct whether or not this lands, and the next
// reconcile pass repairs a miss.
func (ah *deliverablesHandler) reconcileDeliverableOutOfBand(r *http.Request, orgID, userID string, art *domain.Deliverable) {
	if ah.reconciler == nil {
		return
	}
	rc := ah.reconciler()
	if rc == nil {
		return
	}
	ctx := context.WithoutCancel(r.Context())
	arts := []domain.Deliverable{*art}
	if art.ConversationID != "" {
		if err := ah.tx.WithReadTx(ctx, orgID, userID, func(tx db.TxStores) error {
			all, _, e := tx.Deliverables.ListByConversation(ctx, orgID, art.ConversationID, db.Unwindowed)
			if e != nil {
				return e
			}
			arts = arts[:0]
			for _, a := range all {
				if domain.IsReconcilableNonTerminal(a.Kind, a.State) {
					arts = append(arts, a)
				}
			}
			return nil
		}); err != nil {
			deliverablesLog.Warn("list conversation deliverables for out-of-band reconcile failed; reconciling the one row", "deliverable", art.ID, "error", err)
			arts = []domain.Deliverable{*art}
		}
	}
	if _, err := rc.Reconcile(ctx, orgID, arts); err != nil {
		deliverablesLog.Warn("out-of-band reconcile failed", "deliverable", art.ID, "error", err)
	}
}

func (ah *deliverablesHandler) handleDeliverableApprove(w http.ResponseWriter, r *http.Request) {
	orgID, ok := requireOrg(w, r)
	if !ok {
		return
	}
	userID := ClaimsFrom(r.Context()).Subject
	id, ok := deliverableIDOr404(w, r)
	if !ok {
		return
	}

	art, ok := ah.loadDeliverable(w, r, orgID, userID, id)
	if !ok {
		return
	}
	switch art.Kind {
	case domain.DeliverableKindReview:
		ah.reviewApprove(w, r, orgID, userID, art)
		return
	case domain.DeliverableKindPullRequest:
		// fall through to the PR path below
	default:
		// The row exists; its kind has nothing to submit. 409, not 404 — a
		// branch deliverable is not a missing deliverable.
		conflict(w, "deliverables of kind "+art.Kind+" cannot be approved")
		return
	}
	gh, prRef, ok := ah.ghForDeliverable(w, r, orgID, art)
	if !ok {
		return
	}
	owner, repo, number := prRef.Owner, prRef.Repo, prRef.Number

	// Approval only makes sense on a draft awaiting it. A stale/double "Open PR"
	// click on an already-open or closed deliverable would otherwise re-run a
	// no-op MarkPRReady and record a second approval, so reject it as a
	// conflict. The state transition is gated here rather than in
	// ghForDeliverable, which the read paths (GET/diff) share and must keep
	// serving non-draft PRs.
	if art.State != domain.DeliverableStatePRDraft {
		httpx.WriteErrors(w, http.StatusConflict, httpx.ErrorItem{
			Reason:  httpx.ReasonAlreadyTerminal,
			Message: "this PR is no longer a draft awaiting approval (state: " + art.State + ")",
		})
		return
	}

	// Parse the deliverable details so the flip below can carry the row's own
	// snapshot and resolution forward. A parse failure is non-fatal: the PR is
	// still promoted, and the flip writes what it can from the live read.
	details, derr := domain.ParsePRDeliverableDetails(art.DetailsJSON)
	if derr != nil {
		deliverablesLog.Warn("PR deliverable details unparseable; promoting from the live PR", "deliverable", art.ID, "error", derr)
	}

	// Read the content being promoted from the LIVE PR, never the cached
	// snapshot: a stale snapshot would misreport a direct-on-GitHub edit, and a
	// malformed one would record an empty title/body. The same read confirms
	// the PR is still a draft to promote at all.
	live, ok := ah.liveDraftOr409(w, r, gh, orgID, userID, art, owner, repo, number)
	if !ok {
		return
	}
	finalTitle := live.Title
	finalBody := live.Body

	if err := gh.MarkPRReady(r.Context(), owner, repo, number); err != nil {
		deliverablesLog.Warn("MarkPRReady failed", "deliverable", art.ID, "owner", owner, "repo", repo, "number", number, "error", err)
		writeUpstreamGitHub(w, "GitHub API error", err)
		return
	}

	// Post-success bookkeeping runs detached from r.Context(): the PR is
	// already ready on GitHub, so a client disconnect mustn't leave the
	// conversation/task in a half-cleaned state. Each step is best-effort +
	// logged, and each gets its own tx so one failure doesn't roll back the
	// others.
	cleanupCtx := context.WithoutCancel(r.Context())

	// Step 1: flip the deliverable to open and refresh its snapshot to the promoted
	// content. proposed stays frozen. When details didn't parse we still flip
	// the state but leave the (malformed) details rather than blanking them.
	//
	// The credential is classified before the tx opens — the classification can
	// reach GitHub, and the tx the row composes into must not wait on a network
	// round trip.
	credential := ghclient.CredentialForRepo(cleanupCtx, ah.ghResolver, orgID, owner, repo)
	openArt := *art
	openArt.State = domain.DeliverableStatePROpen
	if derr == nil {
		details.Snapshot = domain.PRDeliverableSnapshot{Title: finalTitle, Body: finalBody}
		details.Resolution = domain.PRResolutionApproved
		openArt.DetailsJSON = domain.MarshalPRDeliverableDetails(details)
	}
	// Compose the audit row with the flip (TFAC-483): the org-App MarkPRReady is a
	// human-authorized, org-executed write — conversation_id is the drafting conversation, actor is
	// the approver. Recording inside the flip tx keeps the audit row and the
	// deliverable state atomic (both land or neither).
	if err := ah.tx.WithTx(cleanupCtx, orgID, userID, func(tx db.TxStores) error {
		if _, e := tx.Deliverables.Upsert(cleanupCtx, orgID, openArt); e != nil {
			return e
		}
		return tx.ExternalActions.Record(cleanupCtx, orgID,
			personDeliverableAction(art, userID, domain.ActionPRMarkedReady, domain.DeliverableStatePRDraft, domain.DeliverableStatePROpen, credential))
	}); err != nil {
		deliverablesLog.Warn("flip PR deliverable to open + record action failed", "deliverable", art.ID, "error", err)
	}

	// Step 2: terminal-on-last task closure. Approval is a decoupled sidecar — it
	// never flips conversation status or resumes/terminates a blueprint. The
	// only lifecycle effect is closing the task when this was the LAST
	// unresolved deliverable on an already-terminal blueprint (§3); otherwise a
	// no-op.
	ah.pingConversationsResolved(orgID)
	ah.closeTaskIfTerminalAndResolved(cleanupCtx, orgID, userID, art.ConversationID)

	// Step 3: tell the drafting agent its PR was approved — live if the
	// conversation is warm, else via its ledger on resume. Decoupled from
	// the resolution above. Note the one carve-out to the ledger's "never
	// miss" property: if the best-effort flip in step 1 failed (logged
	// above), the deliverable row stays at state=draft, so a terminal
	// conversation's resume can't re-derive this note — the live steer is
	// then the only delivery. That double-failure (flip failed AND no warm
	// process) drops the note, which is acceptable here: the GitHub PR is
	// already open (the authoritative fact), and the un-flipped draft has
	// bigger problems than the note (it also blocks terminal-on-last
	// closure until reconciliation).
	ah.injectDeliverableNote(orgID, openArt)

	writeJSON(w, http.StatusOK, map[string]any{
		"number":   number,
		"html_url": art.URL,
		"state":    domain.DeliverableStatePROpen,
	})
}

// handleDeliverableDismiss resolves ONE draft pull request as a decoupled sidecar
// operation, the per-item counterpart to approve: it abandons the GitHub object
// and flips the deliverable's state, never touching the conversation's lifecycle.
// The draft PR is closed on GitHub (best-effort) and flipped to closed;
// branches are never touched. An already-terminal deliverable (a non-draft PR) is
// a 409 — there is nothing left to resolve. After the flip it runs the shared
// terminal-on-last task-closure check.
//
// POST /api/deliverables/{id}/dismiss
func (ah *deliverablesHandler) handleDeliverableDismiss(w http.ResponseWriter, r *http.Request) {
	orgID, ok := requireOrg(w, r)
	if !ok {
		return
	}
	userID := ClaimsFrom(r.Context()).Subject
	id, ok := deliverableIDOr404(w, r)
	if !ok {
		return
	}

	art, ok := ah.loadDeliverable(w, r, orgID, userID, id)
	if !ok {
		return
	}
	// Pull requests only. A review's abandonment reaches nothing outside this
	// process — it is a local state flip — so it is a field write on the review
	// sub-resource (PATCH …/review {state:"dismissed"}), not a verb. This verb
	// exists because closing a PR is a real write to GitHub.
	if !requireDeliverableKind(w, art, domain.DeliverableKindPullRequest, "a pull request") {
		return
	}

	// Only a draft PR can be dismissed. An already-open / merged / closed PR is
	// terminal — there's no draft to abandon — so 409 rather than re-closing a PR
	// the user already shipped or a prior dismiss already retired.
	if art.State != domain.DeliverableStatePRDraft {
		httpx.WriteErrors(w, http.StatusConflict, httpx.ErrorItem{
			Reason:  httpx.ReasonAlreadyTerminal,
			Message: "this PR is no longer a draft awaiting resolution (state: " + art.State + ")",
		})
		return
	}

	// Flip the deliverable to closed + audit, atomic and pessimistic (the flip is the
	// resolution, so a DB failure is a real error the client must see). The GitHub
	// close + the terminal-on-last check run detached afterwards so a client
	// disconnect can't strand them once the deliverable is resolved.
	//
	// The credential is classified before the tx: the GitHub close runs after it,
	// but the row records the credential that close is made under, and a probe
	// inside the tx would hold it open across a round trip.
	//
	// The pull request is resolved first, by the registry row its entity
	// references. One that names no pull request is still dismissed: the flip
	// is the resolution, and there is simply nothing on GitHub to close.
	var prRef *db.DeliverablePullRequest
	switch resolved, err := ah.deliverablePullRequest(r.Context(), orgID, art); {
	case err == nil:
		prRef = &resolved
	case errors.Is(err, db.ErrDeliverableNamesNoPullRequest):
		deliverablesLog.Warn("dismiss: deliverable names no pull request; dismissing without a GitHub close", "deliverable", art.ID, "error", err)
	default:
		internalError(w, "deliverables", err)
		return
	}
	credential := githubCredentialForPullRequest(r.Context(), ah.ghResolver, orgID, prRef)
	closed := *art
	closed.State = domain.DeliverableStatePRClosed
	closed.DetailsJSON = domain.StampPRResolution(art.DetailsJSON, domain.PRResolutionDismissed)
	if err := ah.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		if _, e := tx.Deliverables.Upsert(r.Context(), orgID, closed); e != nil {
			return e
		}
		return tx.ExternalActions.Record(r.Context(), orgID,
			personDeliverableAction(art, userID, domain.ActionPRClosed, domain.DeliverableStatePRDraft, domain.DeliverableStatePRClosed, credential))
	}); err != nil {
		internalError(w, "deliverables", err)
		return
	}

	cleanupCtx := context.WithoutCancel(r.Context())
	// Close the draft PR on GitHub (best-effort). The deliverable is already closed; a
	// GitHub hiccup leaves the PR for reconciliation to retire later. Branch kept.
	number := 0
	if prRef != nil {
		deliverableteardown.CloseDraftPR(cleanupCtx, ah.ghResolver, orgID, art, *prRef)
		number = prRef.Number
	}
	ah.pingConversationsResolved(orgID)
	ah.closeTaskIfTerminalAndResolved(cleanupCtx, orgID, userID, art.ConversationID)
	// Tell the drafting agent its draft PR was dismissed (live or via the ledger).
	ah.injectDeliverableNote(orgID, closed)

	writeJSON(w, http.StatusOK, map[string]any{
		"number":   number,
		"html_url": art.URL,
		"state":    domain.DeliverableStatePRClosed,
	})
}

// handleDeliverableReject resolves ONE draft pull request the hard way: the head
// branch is deleted from the upstream and the draft PR closed. It is the
// sibling of dismiss — same decoupled-sidecar contract (the deliverable flips, the
// conversation lifecycle is never touched, the shared terminal-on-last check
// runs after) — differing in exactly one write, and that write is the one
// irreversible act in the whole PR lifecycle, which is why it is its own verb
// behind its own confirmation rather than a flag on dismiss.
//
// The order is dictated by what can be undone. First the live PR is read and
// confirmed still a draft (liveDraftOr409): a PR resolved on GitHub in the
// meantime is not this verb's to touch — least of all its branch. Then the
// branch delete, pessimistic: if GitHub refuses (a protected branch, a
// credential without contents:write, an outage) nothing has changed anywhere,
// so the caller sees the refusal and can retry, dismiss, or open the PR
// conventionally. A ref the upstream no longer holds is the same answer as a
// resolved PR — reality moved, the row is reconciled, 409 — rather than a
// rejection recorded over a deletion this verb never made. Once the branch is
// gone the PR is doomed regardless (GitHub closes a PR whose head branch
// disappears), so the close and the bookkeeping after it are best-effort or
// retry-safe: a DB failure on the flip answers 500 with the deliverable still
// draft, and a retry finds the branch gone and reconciles.
//
// POST /api/deliverables/{id}/reject
func (ah *deliverablesHandler) handleDeliverableReject(w http.ResponseWriter, r *http.Request) {
	orgID, ok := requireOrg(w, r)
	if !ok {
		return
	}
	userID := ClaimsFrom(r.Context()).Subject
	id, ok := deliverableIDOr404(w, r)
	if !ok {
		return
	}

	art, ok := ah.loadDeliverable(w, r, orgID, userID, id)
	if !ok {
		return
	}
	if !requireDeliverableKind(w, art, domain.DeliverableKindPullRequest, "a pull request") {
		return
	}
	// Only a draft awaits a decision. A PR the user already opened, or one a
	// prior dismiss/reject retired, is terminal here — and an open PR's branch
	// is shipped work, never something this verb deletes.
	if art.State != domain.DeliverableStatePRDraft {
		httpx.WriteErrors(w, http.StatusConflict, httpx.ErrorItem{
			Reason:  httpx.ReasonAlreadyTerminal,
			Message: "this PR is no longer a draft awaiting resolution (state: " + art.State + ")",
		})
		return
	}

	// The head branch is the thing being deleted, so it has to be known and
	// has to be a branch this verb may delete. Both refusals are 409: the row is
	// real and readable, its recorded shape just cannot carry a rejection —
	// dismiss still can.
	details, derr := domain.ParsePRDeliverableDetails(art.DetailsJSON)
	if derr != nil || details.HeadBranch == "" {
		conflict(w, "this PR's head branch is not recorded, so its branch cannot be deleted; dismiss it instead to close the PR and keep the branch")
		return
	}
	if details.HeadBranch == details.Base {
		conflict(w, "this PR's head branch is its base branch, which is never deleted; dismiss it instead")
		return
	}

	gh, prRef, ok := ah.ghForDeliverable(w, r, orgID, art)
	if !ok {
		return
	}
	owner, repo, number := prRef.Owner, prRef.Repo, prRef.Number
	if _, ok := ah.liveDraftOr409(w, r, gh, orgID, userID, art, owner, repo, number); !ok {
		return
	}

	// The irreversible write, pessimistic — see the ordering above.
	switch err := gh.DeleteBranchRef(r.Context(), owner, repo, details.HeadBranch); {
	case err == nil:
	case errors.Is(err, ghclient.ErrBranchRefMissing):
		deliverablesLog.Info("reject: branch already gone from the upstream; reconciling the row instead", "deliverable", art.ID, "owner", owner, "repo", repo, "branch", details.HeadBranch)
		ah.reconcileDeliverableOutOfBand(r, orgID, userID, art)
		httpx.WriteErrors(w, http.StatusConflict, httpx.ErrorItem{
			Reason:  httpx.ReasonAlreadyTerminal,
			Message: "branch " + details.HeadBranch + " is already gone from the upstream, so this PR was resolved outside Triage Factory; its record has been refreshed",
		})
		return
	default:
		deliverablesLog.Warn("reject: DeleteBranchRef failed; nothing changed", "deliverable", art.ID, "owner", owner, "repo", repo, "branch", details.HeadBranch, "error", err)
		writeUpstreamGitHub(w, "GitHub refused to delete branch "+details.HeadBranch+"; the PR is still a draft", err)
		return
	}

	// From here the rejection has happened on GitHub, so nothing below may be
	// stranded by a client disconnect.
	cleanupCtx := context.WithoutCancel(r.Context())

	// Close the draft PR. GitHub closes a PR whose head branch disappears, so
	// this is a no-op most of the time and best-effort always: a hiccup leaves
	// the PR for reconciliation to retire, exactly as dismiss does.
	if err := gh.ClosePR(cleanupCtx, owner, repo, number); err != nil {
		deliverablesLog.Warn("reject: ClosePR failed (branch already deleted; PR left for reconciliation)", "deliverable", art.ID, "owner", owner, "repo", repo, "number", number, "error", err)
	}

	// Flip the PR deliverable to closed with the rejection recorded on the row (the
	// resolution note is derived from this row alone), retire the run's branch
	// deliverable for the same ref rather than leaving it for the reconciler to
	// notice, and audit both writes — all in one tx, so the audit never
	// disagrees with the state. The credential is classified before the tx
	// opens: it can reach GitHub, and the tx must not wait on a round trip.
	credential := ghclient.CredentialForRepo(cleanupCtx, ah.ghResolver, orgID, owner, repo)
	details.Resolution = domain.PRResolutionRejected
	closed := *art
	closed.State = domain.DeliverableStatePRClosed
	closed.DetailsJSON = domain.MarshalPRDeliverableDetails(details)
	repoPath := owner + "/" + repo
	headRef := "refs/heads/" + details.HeadBranch
	if err := ah.tx.WithTx(cleanupCtx, orgID, userID, func(tx db.TxStores) error {
		if _, e := tx.Deliverables.Upsert(cleanupCtx, orgID, closed); e != nil {
			return e
		}
		branchURL, e := retireBranchDeliverable(cleanupCtx, tx, orgID, art.ConversationID, art.Scope, prRef.RepositoryID, repoPath, headRef)
		if e != nil {
			return e
		}
		if e := tx.ExternalActions.Record(cleanupCtx, orgID,
			personDeliverableAction(art, userID, domain.ActionPRClosed, domain.DeliverableStatePRDraft, domain.DeliverableStatePRClosed, credential)); e != nil {
			return e
		}
		return tx.ExternalActions.Record(cleanupCtx, orgID,
			branchDeletedAction(art, userID, prRef.RepositoryID, repoPath, headRef, details.HeadBranch, branchURL, credential))
	}); err != nil {
		internalError(w, "deliverables", err)
		return
	}

	ah.pingConversationsResolved(orgID)
	ah.closeTaskIfTerminalAndResolved(cleanupCtx, orgID, userID, art.ConversationID)
	// Tell the drafting agent — live if warm, else via its ledger on resume,
	// which re-derives the rejection from the resolution on the row.
	ah.injectDeliverableNote(orgID, closed)

	writeJSON(w, http.StatusOK, map[string]any{
		"number":   number,
		"html_url": art.URL,
		"state":    domain.DeliverableStatePRClosed,
		"branch":   details.HeadBranch,
	})
}

// retireBranchDeliverable flips the conversation's `branch` deliverable for headRef
// in the repository whose registry row is repositoryID, on the GitHub host
// scope, to deleted, returning its web URL for the audit row. The repository is
// the pull request's (deliverablePullRequest) — the one the branch delete just
// went to under its current name, repoPath — and the deliverable is the one keyed
// on that row and headRef (domain.BranchDedupKey), whatever name its target
// spells. A conversation with no such deliverable — a push the capture writers
// missed, a pull request recorded unlinked (no repositoryID), or no
// conversation at all — is not an error: the PR deliverable carries the rejection
// on its own, and the returned URL falls back to the branch link on scope's
// host so the audit row still links somewhere.
func retireBranchDeliverable(ctx context.Context, tx db.TxStores, orgID, conversationID, scope, repositoryID, repoPath, headRef string) (string, error) {
	fallbackURL, _ := domain.BranchDeliverableWebURL(scope, repoPath, headRef)
	if conversationID == "" || repositoryID == "" {
		return fallbackURL, nil
	}
	key := domain.BranchDedupKey(repositoryID, headRef)
	arts, _, err := tx.Deliverables.ListByConversation(ctx, orgID, conversationID, db.Unwindowed)
	if err != nil {
		return "", err
	}
	for i := range arts {
		a := arts[i]
		if a.Kind != domain.DeliverableKindBranch || a.Scope != scope || a.DedupKey != key {
			continue
		}
		if a.State != domain.DeliverableStateBranchDeleted {
			a.State = domain.DeliverableStateBranchDeleted
			if _, err := tx.Deliverables.Upsert(ctx, orgID, a); err != nil {
				return "", err
			}
		}
		if a.URL != "" {
			return a.URL, nil
		}
		return fallbackURL, nil
	}
	return fallbackURL, nil
}

// branchDeletedAction builds the external_actions row for a rejection's branch
// delete: a human-authorized, org-executed GitHub write against the PR's repo,
// keyed on the ref the way branch_pushed is so the two read as one branch's
// history, and about the repository's registry row (repositoryID, empty for a
// pull request recorded unlinked). ConversationID is the drafting conversation, the
// actor the rejecting human, the team the deliverable's.
func branchDeletedAction(art *domain.Deliverable, userID, repositoryID, repoPath, headRef, branch, url, credential string) domain.ExternalAction {
	detail, _ := json.Marshal(map[string]string{"branch": branch})
	return domain.ExternalAction{
		TeamID:         art.TeamID,
		Provider:       domain.ProviderGitHub,
		Scope:          art.Scope,
		Action:         domain.ActionBranchDeleted,
		Target:         repoPath,
		RepositoryID:   repositoryID,
		ExternalID:     headRef,
		URL:            url,
		FromState:      domain.DeliverableStateBranchPushed,
		ToState:        domain.DeliverableStateBranchDeleted,
		ConversationID: art.ConversationID,
		ActorUserID:    userID,
		Credential:     credential,
		DetailJSON:     string(detail),
		PerformedBy:    domain.PerformedByPerson,
	}
}

// pingConversationsResolved announces that a deliverable resolution changed the
// conversations resource, as the payload-free org-scoped ping
// conversationevent owns.
//
// A resolve is the one write that moves a conversation between the shell
// rail's sets without touching conversations.status: the row is already
// terminal, and dismissing its last draft PR is the difference between "this
// is waiting on you" and "this is done". The task_updated the close check
// below emits covers only the case where the task actually closes — a
// blueprint that aborted, or one with another unresolved deliverable left, emits
// nothing at all, and the rail's `needs` would sit a decision stale until the
// next unrelated event.
func (ah *deliverablesHandler) pingConversationsResolved(orgID string) {
	conversationevent.Publish(ah.ws, orgID)
}

// closeTaskIfTerminalAndResolved is the shared terminal-on-last task-closure
// check, run after any per-deliverable resolve (approve or dismiss). It re-reads
// the task's deliverable set and closes the task IFF the controlling blueprint run
// reached a CLEAN completion AND no unresolved deliverable remains. Otherwise it
// no-ops:
//
//   - a LIVE blueprint run keeps running — resolving a deliverable never closes
//     its task; the run's own eventual termination (terminateBlueprint /
//     standalone completion) re-checks this and closes the task then.
//   - a cleanly-completed blueprint run with other unresolved deliverables leaves
//     its task open; the last resolution is what closes it.
//   - an ABORTED / FAILED / CANCELLED blueprint run leaves the task open for
//     human attention regardless of deliverable state — mirroring
//     terminateBlueprint (blueprint.go)
//     and processCompletion (run.go), which close the task only on a clean finish.
//     Resolving a stray draft PR on an aborted blueprint must not override the
//     "needs a human" disposition.
//   - a task nobody's agent holds any more, or one whose newest blueprint run
//     is not this deliverable's, is somebody else's now. Deliverables outlive the run
//     that produced them, so a resolution can arrive long after the task was
//     requeued, self-claimed or re-delegated — and closing on it would yank the
//     task to done under the person or agent working it today. Both guards are
//     needed: a re-delegation keeps the task bot-claimed and only the run id
//     tells the engagements apart, while a self-claim clears the agent id and
//     mints no run at all.
//
// No accept/dismiss distinction — the task closes on the last resolution
// regardless of whether anything was accepted (epic decision #3). This is the
// ONLY lifecycle effect of a resolve: it never flips conversations.status or
// resumes/terminates a blueprint.
//
// Governing signals: closure keys on the blueprint run behind the conversation
// reaching status=completed, so only a conversation that has one can close its
// task. A non-blueprint conversation (origin <> 'blueprint' — a future
// interactive/ad-hoc one) may well carry a task_id, but it has no blueprint run
// to read a terminal from, so it is a no-op here. The "anything still unresolved?" check is
// scoped to the whole TASK (all its conversations), matching
// the task-end teardown, so a stranded deliverable from a prior attempt blocks
// closure.
//
// Detached + best-effort: the caller already mutated the GitHub object and
// flipped the deliverable, so a failure here must not unwind that. Fails CLOSED on
// the close direction — any read error leaves the task open (recoverable; the
// next resolve or conversation termination re-checks) rather than closing a
// task that may still hold unresolved work.
func (ah *deliverablesHandler) closeTaskIfTerminalAndResolved(ctx context.Context, orgID, userID, conversationID string) {
	if conversationID == "" {
		return
	}
	var (
		taskID        string
		closeEligible bool
		unresolved    bool
	)
	if err := ah.tx.WithReadTx(ctx, orgID, userID, func(tx db.TxStores) error {
		// Step 1: the blueprint run is what carries the terminal this closure
		// keys on, so a conversation with none cannot drive it — whether or not
		// it carries a task_id of its own. Leave taskID empty and no-op below.
		br, _, bpErr := tx.Blueprints.GetRunForConversation(ctx, orgID, conversationID)
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
			return nil // no need to scan deliverables if we can't close anyway
		}
		// Step 2: the task must still be the one this run was about. A task
		// that has moved on — its claim handed to a human, or a later
		// delegation minted over it — is closed by whatever ends THAT
		// engagement, not by a resolution belonging to a superseded one.
		task, e := tx.Tasks.Get(ctx, orgID, taskID)
		if e != nil {
			return fmt.Errorf("read task: %w", e)
		}
		if task == nil || task.ClaimedByAgentID == "" {
			closeEligible = false
			return nil
		}
		isNewest, e := tx.Blueprints.IsNewestRunForTask(ctx, orgID, taskID, br.ID)
		if e != nil {
			return fmt.Errorf("is newest run for task: %w", e)
		}
		if !isNewest {
			closeEligible = false
			return nil
		}
		// Step 3: unresolved check scoped to the TASK (all its conversations),
		// matching the task-end teardown — not just the current blueprint's step
		// conversations. A stranded deliverable from a prior attempt (e.g. a
		// teardown that partially failed on a network blip) must block closure
		// rather than be silently skipped, so we never close the task with an
		// unresolved draft PR / pending review sitting on GitHub.
		convs, e := tx.Conversations.ListForTask(ctx, orgID, taskID)
		if e != nil {
			return fmt.Errorf("list conversations for task: %w", e)
		}
		has, e := conversationsHaveUnresolvedDeliverables(ctx, tx, orgID, convs)
		if e != nil {
			return e
		}
		unresolved = has
		return nil
	}); err != nil {
		deliverablesLog.Warn("terminal-on-last close check failed; leaving task open (fail closed)", "conversation", conversationID, "error", err)
		return
	}
	if taskID == "" || !closeEligible || unresolved {
		return
	}

	// Close in its own tx. Tasks.Close's WHERE is state-guarded (status NOT IN
	// ('done','dismissed')), so two concurrent resolves that both pass the gate
	// above race harmlessly: the second close matches nothing and answers
	// db.ErrNoSuchTask, which this treats as "already closed by the other
	// racer" rather than a real failure.
	if err := ah.tx.WithTx(ctx, orgID, userID, func(tx db.TxStores) error {
		_, err := tx.Tasks.Close(ctx, orgID, taskID, "run_completed", "")
		return err
	}); err != nil {
		if !errors.Is(err, db.ErrNoSuchTask) {
			deliverablesLog.Warn("terminal-on-last task close failed", "task", taskID, "conversation", conversationID, "error", err)
		}
		return
	}
	// Move the card to Done on peer boards without a refetch.
	ah.ws.Broadcast(websocket.Event{
		Type:  "task_updated",
		OrgID: orgID,
		Data:  map[string]any{"task_id": taskID, "status": "done"},
	})
}

// conversationsHaveUnresolvedDeliverables reports whether any of the given
// conversations still holds an unresolved deliverable (a draft PR or a ready
// review — domain.HasUnresolvedDeliverables), reading each conversation's
// deliverables under the caller's tx. One query per conversation, so the cost is
// the caller's set — the terminal-on-last check passes the whole task's
// conversations, which is what makes a draft carried in from an earlier
// engagement count. Shared by that check so the "task still has unresolved
// work" definition lives next to its single use.
func conversationsHaveUnresolvedDeliverables(ctx context.Context, tx db.TxStores, orgID string, convs []domain.Conversation) (bool, error) {
	for i := range convs {
		arts, _, err := tx.Deliverables.ListByConversation(ctx, orgID, convs[i].ID, db.Unwindowed)
		if err != nil {
			return false, fmt.Errorf("deliverables.ListByConversation(%s): %w", convs[i].ID, err)
		}
		if domain.HasUnresolvedDeliverables(arts) {
			return true, nil
		}
	}
	return false, nil
}

// upsertPRDetails writes the deliverable back with new details_json, inside a
// claims-set tx (app pool / RLS). State and coordinates are carried unchanged
// from the existing row; only DetailsJSON moves.
func (ah *deliverablesHandler) upsertPRDetails(ctx context.Context, orgID, userID string, art *domain.Deliverable, details domain.PRDeliverableDetails) error {
	next := *art
	next.DetailsJSON = domain.MarshalPRDeliverableDetails(details)
	return ah.tx.WithTx(ctx, orgID, userID, func(tx db.TxStores) error {
		_, e := tx.Deliverables.Upsert(ctx, orgID, next)
		return e
	})
}

// deliverableOnCurrentHost reports whether art was recorded on the org's GitHub
// host now, the host every client this handler resolves talks to, and returns
// that host.
func (ah *deliverablesHandler) deliverableOnCurrentHost(ctx context.Context, orgID string, art *domain.Deliverable) (bool, string, error) {
	var host string
	if err := ah.tx.WithReadTx(ctx, orgID, ClaimsFrom(ctx).Subject, func(tx db.TxStores) error {
		set, err := tx.Orgs.GetSettings(ctx, orgID)
		host = domain.ExternalObjectScope(domain.ProviderGitHub, set)
		return err
	}); err != nil {
		return false, "", fmt.Errorf("read the org's github host: %w", err)
	}
	return art.Scope == host, host, nil
}

// hostOrNotConfigured names a GitHub host for a message, or says there is none.
func hostOrNotConfigured(host string) string {
	if host == "" {
		return "not configured"
	}
	return host
}
