import { useState, useEffect, useCallback, useRef } from 'react'
import { motion, AnimatePresence } from 'motion/react'
import { parseDiff } from 'react-diff-view'
import type { FileData } from 'react-diff-view'
import DiffFile from './DiffFile'
import type { FileComment } from './DiffFile'
import ReviewSummary from './ReviewSummary'

// ReviewArtifact mirrors internal/server/reviews_artifact_handler.go's
// reviewArtifactJSON. review_body / review_event are the STAGED values (applied
// to GitHub only on approval); the comments are staged TF-side (TFAC-494), each
// with its severity parsed back out of the body (chip) and the clean body shown.
// commits_since_finalize + per-comment freshness are computed against the live PR
// head at GET time (TFAC-500) so the human sees how far the PR has drifted since
// the agent wrote the review.
interface ReviewArtifact {
  id: string
  run_id?: string
  owner: string
  repo: string
  pr_number: number
  review_id: string
  review_body: string
  review_event: string
  state: string
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
    // 'current' | 'moved' | 'outdated' | 'unknown' (TFAC-500).
    freshness?: string
    // New-side position on the live head when freshness is 'moved'.
    mapped_line?: number
    mapped_path?: string
  }[]
}

interface Props {
  artifactId: string
  open: boolean
  onClose: () => void
}

// ReviewOverlay is the modal opened from a parked review run's approval card. The
// agent already created a GitHub pending review (private to the bot); this overlay
// reads it 1:1, edits inline comments live, stages the summary body + verdict, and
// on Submit approves — submitting the pending review to GitHub. Mirrors
// PendingPROverlay's shape (artifactId-addressed, pessimistic sync, isolated diff
// error) with review-specific affordances (inline comments + body/event editor).
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
        const res = await fetch(`/api/artifacts/${artifactId}`)
        if (!res.ok) {
          const e = await res.json().catch(() => ({}))
          throw new Error(e.error || `Failed to load review (${res.status})`)
        }
        data = await res.json()
      } catch (err) {
        if (!cancelled) {
          setError(err instanceof Error ? err.message : String(err))
          setLoading(false)
        }
        return
      }
      if (cancelled) return
      setReview(data)

      try {
        const diffRes = await fetch(`/api/artifacts/${artifactId}/diff`)
        if (!diffRes.ok) {
          const e = await diffRes.json().catch(() => ({}))
          throw new Error(e.error || `Failed to load diff (${diffRes.status})`)
        }
        const diffText = await diffRes.text()
        if (cancelled) return
        setFiles(parseDiff(diffText))
        setTruncationNote(diffRes.headers.get('X-Diff-Truncated'))
      } catch (err) {
        if (!cancelled) setDiffError(err instanceof Error ? err.message : String(err))
      } finally {
        if (!cancelled) setLoading(false)
      }
    })()

    return () => {
      cancelled = true
    }
  }, [open, artifactId])

  // Pessimistic comment ops on the live GitHub pending review: await the request,
  // throw on !ok so the child (ReviewComment) surfaces the error and stays in edit
  // mode, and update local state only on success.
  const handleUpdateComment = useCallback(
    async (commentId: string, body: string) => {
      const res = await fetch(`/api/artifacts/${artifactId}/comments/${commentId}`, {
        method: 'PUT',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ body }),
      })
      if (!res.ok) {
        const e = await res.json().catch(() => ({}))
        throw new Error(e.error || `Save failed (${res.status})`)
      }
      setReview((prev) =>
        prev
          ? {
              ...prev,
              comments: prev.comments.map((c) => (c.id === commentId ? { ...c, body } : c)),
            }
          : prev,
      )
    },
    [artifactId],
  )

  const handleDeleteComment = useCallback(
    async (commentId: string) => {
      const res = await fetch(`/api/artifacts/${artifactId}/comments/${commentId}`, {
        method: 'DELETE',
      })
      if (!res.ok) {
        const e = await res.json().catch(() => ({}))
        throw new Error(e.error || `Delete failed (${res.status})`)
      }
      setReview((prev) =>
        prev ? { ...prev, comments: prev.comments.filter((c) => c.id !== commentId) } : prev,
      )
    },
    [artifactId],
  )

  // Body + event stage into the artifact's details_json (no GitHub call — applied
  // at approval). lastSavePromise lets the submit handler await the last edit.
  const patchReview = useCallback(
    async (patch: object) => {
      const res = await fetch(`/api/artifacts/${artifactId}`, {
        method: 'PATCH',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify(patch),
      })
      if (!res.ok) {
        const e = await res.json().catch(() => ({}))
        throw new Error(e.error || `Save failed (${res.status})`)
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
      const p = patchReview({ review_body: body })
      inFlightSaves.current.add(p)
      void p.finally(() => inFlightSaves.current.delete(p))
      await p
      setReview((prev) => (prev ? { ...prev, review_body: body } : prev))
    },
    [patchReview],
  )

  const handleUpdateEvent = useCallback(
    async (event: string) => {
      const p = patchReview({ review_event: event })
      inFlightSaves.current.add(p)
      void p.finally(() => inFlightSaves.current.delete(p))
      await p
      setReview((prev) => (prev ? { ...prev, review_event: event } : prev))
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
      const res = await fetch(`/api/artifacts/${artifactId}/approve`, {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
      })
      if (!res.ok) {
        const e = await res.json().catch(() => ({}))
        throw new Error(e.error || 'Submit failed')
      }
      onClose()
    } catch (err) {
      setSubmitError(err instanceof Error ? err.message : String(err))
    } finally {
      setSubmitting(false)
    }
  }, [artifactId, onClose])

  // Group comments by file path for the diff renderer. A "moved" comment is keyed
  // under its remapped (new-side) path so it lands on the file the code lives in on
  // the live head, not the path it was authored against (TFAC-500).
  const commentsByFile = (review?.comments ?? []).reduce<Record<string, FileComment[]>>(
    (acc, c) => {
      const path = c.freshness === 'moved' && c.mapped_path ? c.mapped_path : c.path
      ;(acc[path] ??= []).push({
        id: c.id,
        path,
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
            className="fixed inset-0 z-50 bg-black/20 backdrop-blur-sm"
            initial={{ opacity: 0 }}
            animate={{ opacity: 1 }}
            exit={{ opacity: 0 }}
            onClick={onClose}
          />

          {/* Panel */}
          <motion.div
            className="fixed inset-6 z-50 flex flex-col bg-surface/95 backdrop-blur-2xl border border-border-glass rounded-3xl shadow-2xl shadow-black/[0.08] overflow-hidden"
            initial={{ opacity: 0, scale: 0.97, y: 12 }}
            animate={{ opacity: 1, scale: 1, y: 0 }}
            exit={{ opacity: 0, scale: 0.97, y: 12 }}
            transition={{ type: 'spring', damping: 30, stiffness: 350 }}
            onClick={(e) => e.stopPropagation()}
          >
            {/* Top bar */}
            <div className="shrink-0 flex items-center justify-between px-6 py-4 border-b border-border-subtle">
              <div className="flex items-center gap-3">
                <div className="w-2 h-2 rounded-full bg-snooze animate-pulse" />
                <h1 className="text-[15px] font-semibold text-text-primary tracking-tight">
                  Pending Review
                </h1>
                {review && (
                  <span className="text-[12px] text-text-tertiary">
                    {review.owner}/{review.repo} #{review.pr_number}
                  </span>
                )}
              </div>
              <button
                onClick={onClose}
                className="text-text-tertiary hover:text-text-secondary transition-colors text-lg leading-none px-2 py-1 rounded-lg hover:bg-black/[0.03]"
              >
                &times;
              </button>
            </div>

            {/* Content */}
            <div className="flex-1 overflow-y-auto">
              {loading ? (
                <div className="flex items-center justify-center h-64">
                  <div className="flex flex-col items-center gap-3">
                    <div className="w-5 h-5 border-2 border-accent/30 border-t-accent rounded-full animate-spin" />
                    <span className="text-[12px] text-text-tertiary">Loading review...</span>
                  </div>
                </div>
              ) : error ? (
                <div className="flex items-center justify-center h-64">
                  <div className="text-center">
                    <p className="text-[13px] text-dismiss">{error}</p>
                    <button
                      onClick={onClose}
                      className="text-[12px] text-text-tertiary hover:text-text-secondary mt-2 transition-colors"
                    >
                      Close
                    </button>
                  </div>
                </div>
              ) : review ? (
                <div className="p-6 space-y-4 max-w-5xl mx-auto">
                  {/* Review summary + actions */}
                  <ReviewSummary
                    owner={review.owner}
                    repo={review.repo}
                    prNumber={review.pr_number}
                    reviewEvent={review.review_event}
                    reviewBody={review.review_body}
                    commentCount={review.comments.length}
                    commitsSinceFinalize={review.commits_since_finalize}
                    onUpdateBody={handleUpdateBody}
                    onUpdateEvent={handleUpdateEvent}
                    onSubmit={handleSubmit}
                    onClose={onClose}
                    submitting={submitting}
                  />

                  {submitError && (
                    <div className="rounded-xl border border-dismiss/30 bg-dismiss/[0.06] px-4 py-3 text-[12px] text-text-secondary">
                      <span className="font-semibold text-text-primary">
                        Couldn't submit review:
                      </span>{' '}
                      {submitError}. Your edits are saved on GitHub — you can retry Submit.
                    </div>
                  )}

                  {truncationNote && (
                    <div className="rounded-xl border border-snooze/30 bg-snooze/[0.06] px-4 py-3 text-[12px] text-text-secondary">
                      <span className="font-semibold text-text-primary">Diff truncated:</span>{' '}
                      {truncationNote}.
                    </div>
                  )}

                  {diffError && (
                    <div className="rounded-xl border border-dismiss/30 bg-dismiss/[0.06] px-4 py-3 text-[12px] text-text-secondary">
                      <span className="font-semibold text-text-primary">Diff unavailable:</span>{' '}
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
                        />
                      )
                    })}
                  </div>

                  {files.length === 0 && !truncationNote && !diffError && (
                    <div className="text-center py-12">
                      <p className="text-[13px] text-text-tertiary">No diff available</p>
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
