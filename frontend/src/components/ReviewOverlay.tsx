import { useState, useEffect, useCallback, useId, useRef } from 'react'
import { motion, AnimatePresence } from 'motion/react'
import { parseDiff } from 'react-diff-view'
import type { FileData } from 'react-diff-view'
import DiffFile from './DiffFile'
import type { FileComment } from './DiffFile'
import ReviewSummary from './ReviewSummary'
import { useFocusTrap } from '../hooks/useFocusTrap'
import { apiFetch, apiJSON, httpErrorMessage } from '../lib/apiClient'

// ReviewArtifact mirrors GET /api/artifacts/{id} for a review artifact: the
// shared artifact envelope, with the review-shaped payload under `details`.
// review_body / review_event there are the STAGED values (applied to GitHub
// only on approval); the comments are staged TF-side, each with its
// severity parsed back out of the body (chip) and the clean body shown.
// commits_since_finalize + per-comment freshness are computed against the live
// PR head at GET time, so the human sees how far the PR has drifted
// since the agent wrote the review.
interface ReviewArtifact {
  id: string
  kind: string
  state: string
  // The posted review's GitHub deep link, stamped at approval; empty while the
  // draft is pending (nothing exists on GitHub yet).
  url: string
  details: {
    owner: string
    repo: string
    pr_number: number
    review_body: string
    review_event: string
    // null when it couldn't be computed (live head unreachable); 0 means the PR
    // hasn't advanced since finalize.
    commits_since_finalize: number | null
    comments: {
      id: string
      path: string
      line: number
      start_line?: number
      body: string
      severity?: string
      // 'current' | 'moved' | 'outdated' | 'unknown'.
      freshness?: string
      // New-side position on the live head when freshness is 'moved'.
      mapped_line?: number
      mapped_path?: string
    }[]
  }
}

interface Props {
  artifactId: string
  open: boolean
  onClose: () => void
}

// ReviewOverlay is the modal opened from a parked review run's approval card.
// The review is staged TF-side (TFAC-494); this overlay reads it 1:1, edits
// inline comments live, stages the summary body + verdict, and on Submit
// approves — creating + submitting the review on GitHub atomically. Mirrors
// PendingPROverlay's shape (artifactId-addressed, pessimistic sync, isolated
// diff error) with review-specific affordances (inline comments + body/event
// editor).
//
// A resolved review (submitted/dismissed) renders READ-ONLY: no Submit, no
// edits, a link out to the posted review instead. The artifacts list stops
// routing resolved reviews here, but the overlay can still land on one — it was
// already open when someone else approved, or an approval-roster row raced its
// fetch — and must not present a dead (or worse, double-posting) Submit.
export default function ReviewOverlay({ artifactId, open, onClose }: Props) {
  const [review, setReview] = useState<ReviewArtifact | null>(null)
  const [files, setFiles] = useState<FileData[]>([])
  const [loading, setLoading] = useState(true)
  // error is the blocking (full-screen) state: without the review there's nothing
  // to edit or approve.
  const [error, setError] = useState<string | null>(null)
  // truncationNote: the X-Diff-Truncated header when the PR diff was reassembled
  // from per-file patches (too large for GitHub to return verbatim).
  const [truncationNote, setTruncationNote] = useState<string | null>(null)
  // diffError is isolated from `error`: the diff is secondary to the review, so a
  // transient 406/502 on it must NOT hide the (already-loaded) editable review.
  const [diffError, setDiffError] = useState<string | null>(null)
  const [submitting, setSubmitting] = useState(false)
  // submitError is the approve (submit-to-GitHub) failure, shown inline so the
  // user can retry in place rather than close-and-reopen.
  const [submitError, setSubmitError] = useState<string | null>(null)
  // refreshing/refreshError back the Refresh action (TFAC-500): re-reconcile the
  // staged comments to the live head. reloadKey re-triggers the load effect after
  // a successful refresh so the overlay re-renders the new finalize frame.
  const [refreshing, setRefreshing] = useState(false)
  const [refreshError, setRefreshError] = useState<string | null>(null)
  const [reloadKey, setReloadKey] = useState(0)

  // Trap keyboard focus inside the overlay while open and restore it to the
  // trigger on close (WCAG 2.1.2); initial focus lands on the close button.
  const dialogRef = useRef<HTMLDivElement>(null)
  const closeRef = useRef<HTMLButtonElement>(null)
  const titleId = useId()
  useFocusTrap(dialogRef, { active: open, initialFocus: closeRef })

  // Fetch the review artifact (blocking), then the diff (isolated).
  useEffect(() => {
    if (!open || !artifactId) return
    let cancelled = false
    setLoading(true)
    setError(null)
    setSubmitError(null)
    setReview(null)
    setFiles([])
    setTruncationNote(null)
    setDiffError(null)
    ;(async () => {
      let data: ReviewArtifact
      try {
        data = await apiJSON<ReviewArtifact>(`/api/artifacts/${artifactId}`)
      } catch (err) {
        if (!cancelled) {
          setError(httpErrorMessage(err, 'Could not load the review.'))
          setLoading(false)
        }
        return
      }
      if (cancelled) return
      setReview(data)

      try {
        // The diff endpoint answers with the patch text, not JSON, and the
        // truncation flag rides on a header — so this reads the Response.
        const diffRes = await apiFetch(`/api/artifacts/${artifactId}/diff`)
        const diffText = await diffRes.text()
        if (cancelled) return
        setFiles(parseDiff(diffText))
        setTruncationNote(diffRes.headers.get('X-Diff-Truncated'))
      } catch (err) {
        if (!cancelled) setDiffError(httpErrorMessage(err, 'Could not load the diff.'))
      } finally {
        if (!cancelled) setLoading(false)
      }
    })()

    return () => {
      cancelled = true
    }
  }, [open, artifactId, reloadKey])

  // Pessimistic comment ops on the live GitHub pending review: await the request,
  // throw on !ok so the child (ReviewComment) surfaces the error and stays in edit
  // mode, and update local state only on success.
  const handleUpdateComment = useCallback(
    async (commentId: string, body: string) => {
      try {
        await apiFetch(`/api/artifacts/${artifactId}/comments/${commentId}`, {
          method: 'PATCH',
          headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify({ body }),
        })
      } catch (err) {
        // Rethrown as a clean message: ReviewComment renders it and stays in
        // edit mode, so an HttpError's raw body must not reach it.
        throw new Error(httpErrorMessage(err, 'Could not save the comment.'))
      }
      setReview((prev) =>
        prev
          ? {
              ...prev,
              details: {
                ...prev.details,
                comments: prev.details.comments.map((c) =>
                  c.id === commentId ? { ...c, body } : c,
                ),
              },
            }
          : prev,
      )
    },
    [artifactId],
  )

  const handleDeleteComment = useCallback(
    async (commentId: string) => {
      try {
        await apiFetch(`/api/artifacts/${artifactId}/comments/${commentId}`, { method: 'DELETE' })
      } catch (err) {
        throw new Error(httpErrorMessage(err, 'Could not delete the comment.'))
      }
      setReview((prev) =>
        prev
          ? {
              ...prev,
              details: {
                ...prev.details,
                comments: prev.details.comments.filter((c) => c.id !== commentId),
              },
            }
          : prev,
      )
    },
    [artifactId],
  )

  // Body + event stage into the artifact's details_json (no GitHub call — applied
  // at approval). lastSavePromise lets the submit handler await the last edit.
  const patchReview = useCallback(
    async (patch: object) => {
      try {
        await apiFetch(`/api/artifacts/${artifactId}/review`, {
          method: 'PATCH',
          headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify(patch),
        })
      } catch (err) {
        throw new Error(httpErrorMessage(err, 'Could not save the review.'))
      }
    },
    [artifactId],
  )

  // Track every in-flight body/event stage (not just the most recent), so submit
  // waits for all of them to settle — a body PATCH still writing when a later
  // event PATCH resolves must not let approve race ahead of the body edit.
  const inFlightSaves = useRef<Set<Promise<void>>>(new Set())

  const handleUpdateBody = useCallback(
    async (body: string) => {
      const p = patchReview({ body })
      inFlightSaves.current.add(p)
      void p.finally(() => inFlightSaves.current.delete(p))
      await p
      setReview((prev) =>
        prev ? { ...prev, details: { ...prev.details, review_body: body } } : prev,
      )
    },
    [patchReview],
  )

  const handleUpdateEvent = useCallback(
    async (event: string) => {
      const p = patchReview({ event })
      inFlightSaves.current.add(p)
      void p.finally(() => inFlightSaves.current.delete(p))
      await p
      setReview((prev) =>
        prev ? { ...prev, details: { ...prev.details, review_event: event } } : prev,
      )
    },
    [patchReview],
  )

  const handleSubmit = useCallback(async () => {
    setSubmitting(true)
    setSubmitError(null)
    try {
      // Settle EVERY in-flight body/event stage before approving so we don't submit
      // before the last edit lands. allSettled never rejects — a failed stage is
      // dropped (its error already surfaced) and approve reads the staged details,
      // which hold the last successful values.
      if (inFlightSaves.current.size > 0) {
        await Promise.allSettled([...inFlightSaves.current])
      }
      await apiFetch(`/api/artifacts/${artifactId}/approve`, {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
      })
      onClose()
    } catch (err) {
      setSubmitError(httpErrorMessage(err, 'Could not submit the review.'))
    } finally {
      setSubmitting(false)
    }
  }, [artifactId, onClose])

  // Re-reconcile the staged comments to the PR's live head (TFAC-500): survivors
  // are remapped, outdated comments dropped, the finalize frame re-pinned. On
  // success, re-trigger the load effect so the overlay re-renders the new frame
  // (count resets to 0, badges to current).
  const handleRefresh = useCallback(async () => {
    setRefreshing(true)
    setRefreshError(null)
    try {
      await apiFetch(`/api/artifacts/${artifactId}/review/refresh`, {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
      })
      setReloadKey((k) => k + 1)
    } catch (err) {
      setRefreshError(httpErrorMessage(err, 'Could not refresh the review.'))
      // Re-throw so ReviewSummary keeps the confirm panel open — that panel is the
      // only place refreshError renders, so closing it would hide the message.
      throw err
    } finally {
      setRefreshing(false)
    }
  }, [artifactId])

  // Per-verdict counts for the Refresh confirmation (computed from the freshness
  // the GET already returned — no extra round-trip): how many comments would be
  // remapped vs. dropped as outdated.
  const movedCount = (review?.details.comments ?? []).filter((c) => c.freshness === 'moved').length
  const outdatedCount = (review?.details.comments ?? []).filter(
    (c) => c.freshness === 'outdated',
  ).length

  // A resolved review is view-only; every mutating affordance below keys off this.
  const readOnly = review != null && review.state !== 'pending'

  // Group comments by file path for the diff renderer. The overlay renders the
  // finalize-time frame, so a comment anchors by the path + line it was written
  // against; freshness + mappedLine ride along for the badge only.
  const commentsByFile = (review?.details.comments ?? []).reduce<Record<string, FileComment[]>>(
    (acc, c) => {
      ;(acc[c.path] ??= []).push({
        id: c.id,
        path: c.path,
        line: c.line,
        startLine: c.start_line,
        body: c.body,
        severity: c.severity,
        freshness: c.freshness,
        mappedLine: c.mapped_line,
      })
      return acc
    },
    {},
  )

  // Close on Escape
  useEffect(() => {
    if (!open) return
    const handler = (e: KeyboardEvent) => {
      if (e.key === 'Escape') onClose()
    }
    window.addEventListener('keydown', handler)
    return () => window.removeEventListener('keydown', handler)
  }, [open, onClose])

  return (
    <AnimatePresence>
      {open && (
        <>
          {/* Backdrop */}
          <motion.div
            className="fixed inset-0 z-50 bg-scrim backdrop-blur-sm"
            initial={{ opacity: 0 }}
            animate={{ opacity: 1 }}
            exit={{ opacity: 0 }}
            onClick={onClose}
          />

          {/* Panel */}
          <motion.div
            ref={dialogRef}
            tabIndex={-1}
            role="dialog"
            aria-modal="true"
            aria-labelledby={titleId}
            className="fixed inset-6 z-50 flex flex-col bg-ground/95 backdrop-blur-2xl border border-line-1 rounded-3xl shadow-float shadow-black/[0.08] overflow-hidden"
            initial={{ opacity: 0, scale: 0.97, y: 12 }}
            animate={{ opacity: 1, scale: 1, y: 0 }}
            exit={{ opacity: 0, scale: 0.97, y: 12 }}
            transition={{ type: 'spring', damping: 30, stiffness: 350 }}
            onClick={(e) => e.stopPropagation()}
          >
            {/* Top bar */}
            <div className="shrink-0 flex items-center justify-between px-6 py-4 border-b border-line-1">
              <div className="flex items-center gap-3">
                {!readOnly && <div className="w-2 h-2 rounded-full bg-warm animate-pulse" />}
                <h1 id={titleId} className="text-column font-semibold text-ink-1 tracking-tight">
                  {readOnly
                    ? review.state === 'submitted'
                      ? 'Submitted Review'
                      : 'Dismissed Review'
                    : 'Pending Review'}
                </h1>
                {review && (
                  <span className="text-ui text-ink-3">
                    {review.details.owner}/{review.details.repo} #{review.details.pr_number}
                  </span>
                )}
              </div>
              <button
                ref={closeRef}
                onClick={onClose}
                className="text-ink-3 hover:text-ink-2 transition-colors text-lg leading-none px-2 py-1 rounded-lg hover:bg-tint-2"
              >
                &times;
              </button>
            </div>

            {/* Content */}
            <div className="flex-1 overflow-y-auto">
              {loading ? (
                <div className="flex items-center justify-center h-64">
                  <div className="flex flex-col items-center gap-3">
                    <div className="w-5 h-5 border-2 border-warm/30 border-t-warm rounded-full animate-spin" />
                    <span className="text-ui text-ink-3">Loading review...</span>
                  </div>
                </div>
              ) : error ? (
                <div className="flex items-center justify-center h-64">
                  <div className="text-center">
                    <p className="text-body text-alarm">{error}</p>
                    <button
                      onClick={onClose}
                      className="text-ui text-ink-3 hover:text-ink-2 mt-2 transition-colors"
                    >
                      Close
                    </button>
                  </div>
                </div>
              ) : review ? (
                <div className="p-6 space-y-4 max-w-5xl mx-auto">
                  {/* Review summary + actions */}
                  <ReviewSummary
                    owner={review.details.owner}
                    repo={review.details.repo}
                    prNumber={review.details.pr_number}
                    reviewEvent={review.details.review_event}
                    reviewBody={review.details.review_body}
                    commentCount={review.details.comments.length}
                    commitsSinceFinalize={review.details.commits_since_finalize}
                    movedCount={movedCount}
                    outdatedCount={outdatedCount}
                    onRefresh={handleRefresh}
                    refreshing={refreshing}
                    refreshError={refreshError}
                    onUpdateBody={handleUpdateBody}
                    onUpdateEvent={handleUpdateEvent}
                    onSubmit={handleSubmit}
                    onClose={onClose}
                    submitting={submitting}
                    readOnly={readOnly}
                    url={review.url}
                  />

                  {submitError && (
                    <div className="rounded-xl border border-alarm/30 bg-alarm/[0.06] px-4 py-3 text-ui text-ink-2">
                      <span className="font-semibold text-ink-1">Couldn't submit review:</span>{' '}
                      {submitError}. Your edits are saved on GitHub — you can retry Submit.
                    </div>
                  )}

                  {truncationNote && (
                    <div className="rounded-xl border border-line-1 bg-tint-2 px-4 py-3 text-ui text-ink-2">
                      <span className="font-semibold text-ink-1">Diff truncated:</span>{' '}
                      {truncationNote}.
                    </div>
                  )}

                  {diffError && (
                    <div className="rounded-xl border border-alarm/30 bg-alarm/[0.06] px-4 py-3 text-ui text-ink-2">
                      <span className="font-semibold text-ink-1">Diff unavailable:</span>{' '}
                      {diffError}. You can still edit and submit the review — the diff just couldn't
                      be loaded right now.
                    </div>
                  )}

                  {/* Diff files */}
                  <div className="space-y-3">
                    {files.map((file, i) => {
                      const path = file.newPath === '/dev/null' ? file.oldPath : file.newPath
                      return (
                        <DiffFile
                          key={path + i}
                          file={file}
                          comments={commentsByFile[path] ?? []}
                          defaultCollapsed={files.length > 8}
                          onUpdateComment={handleUpdateComment}
                          onDeleteComment={handleDeleteComment}
                          readOnly={readOnly}
                        />
                      )
                    })}
                  </div>

                  {files.length === 0 && !truncationNote && !diffError && (
                    <div className="text-center py-12">
                      <p className="text-body text-ink-3">No diff available</p>
                    </div>
                  )}
                </div>
              ) : null}
            </div>
          </motion.div>
        </>
      )}
    </AnimatePresence>
  )
}
