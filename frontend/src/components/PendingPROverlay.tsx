import { useState, useEffect, useCallback, useId, useRef } from 'react'
import { motion, AnimatePresence } from 'motion/react'
import { parseDiff } from 'react-diff-view'
import type { FileData } from 'react-diff-view'
import DiffFile from './DiffFile'
import PendingPRSummary from './PendingPRSummary'
import { useFocusTrap } from '../hooks/useFocusTrap'
import { apiFetch, apiJSON, httpErrorMessage } from '../lib/apiClient'

// PRArtifact mirrors GET /api/artifacts/{id} for a pull_request artifact: the
// shared artifact envelope, with the PR-shaped payload under `details`.
// title/body there are the LIVE PR values (fetched from GitHub via GetPR), not
// a local snapshot — the draft PR is a real GitHub object now, edited 1:1.
interface PRArtifact {
  id: string
  kind: string
  url: string
  state: string
  details: {
    owner: string
    repo: string
    number: number
    head_branch: string
    base_branch: string
    title: string
    body: string
  }
}

interface Props {
  artifactId: string
  open: boolean
  onClose: () => void
}

// PendingPROverlay is the modal the user opens from a delegated run's "Open PR"
// button. The agent already opened a real GitHub draft PR; this overlay reads it
// 1:1, edits title/body live, shows the diff from GitHub, and on "Open PR" marks
// the draft ready for review (approve). Mirrors ReviewOverlay's shape (backdrop +
// centered panel + summary + diff list) but with PR-specific affordances:
//   - title editor (reviews don't have a title)
//   - no inline-comment surface (commentsByFile is always empty)
//   - no draft toggle (the PR is always a draft until approval promotes it)
//
// The diff comes from GitHub (/api/artifacts/{id}/diff) — the PR exists, so the
// server proxies GitHub's diff with the same 406→per-file fallback the review
// overlay uses.
export default function PendingPROverlay({ artifactId, open, onClose }: Props) {
  const [pr, setPR] = useState<PRArtifact | null>(null)
  const [files, setFiles] = useState<FileData[]>([])
  const [loading, setLoading] = useState(true)
  const [error, setError] = useState<string | null>(null)
  const [submitting, setSubmitting] = useState(false)
  // truncationNote is the X-Diff-Truncated header from /diff, surfaced as a
  // banner so the user knows the diff was capped/reassembled (parseDiff of the
  // truncated text alone would just yield fewer files — or zero — and the "No
  // diff available" fallback would mislead the user into thinking it's empty).
  const [truncationNote, setTruncationNote] = useState<string | null>(null)
  // diffError is isolated from `error`: the diff is secondary to the summary, so
  // a transient 406/502 on it must NOT hide the (already-loaded) editable PR.
  // It renders as an inline banner while title/body editing + approve stay live.
  const [diffError, setDiffError] = useState<string | null>(null)
  // submitError is the "Open PR" (approve) failure, shown as an inline banner
  // rather than the full-screen error state: a partial failure (UpdatePR landed
  // but MarkPRReady failed) is safely retryable in place — the artifact stays
  // draft and approve re-strips/re-appends the footer — so we keep the editor
  // visible instead of forcing a close-and-reopen.
  const [submitError, setSubmitError] = useState<string | null>(null)

  // Trap keyboard focus inside the overlay while open and restore it to the
  // trigger on close (WCAG 2.1.2); initial focus lands on the close button.
  const dialogRef = useRef<HTMLDivElement>(null)
  const closeRef = useRef<HTMLButtonElement>(null)
  const titleId = useId()
  useFocusTrap(dialogRef, { active: open, initialFocus: closeRef })

  // Fetch the PR artifact, then the diff. Reset stale state from any prior PR
  // before the new fetch lands so the user doesn't see leftover values briefly.
  useEffect(() => {
    if (!open || !artifactId) return
    let cancelled = false
    setLoading(true)
    setError(null)
    setSubmitError(null)
    setPR(null)
    setFiles([])
    setTruncationNote(null)
    setDiffError(null)
    ;(async () => {
      // PR fetch — a failure here blocks: there's nothing to edit or approve
      // without the PR, so it lands in `error` (the full-screen error state).
      let prData: PRArtifact
      try {
        prData = await apiJSON<PRArtifact>(`/api/artifacts/${artifactId}`)
      } catch (err) {
        if (!cancelled) {
          setError(httpErrorMessage(err, 'Could not load the pull request.'))
          setLoading(false)
        }
        return
      }
      if (cancelled) return
      setPR(prData)

      // Diff fetch — isolated: its failure must not block editing/approving, so
      // it lands in diffError (an inline banner), never the full-screen error.
      try {
        // The diff endpoint answers with the patch text, not JSON, and the
        // truncation flag rides on a header — so this reads the Response. Its
        // FAILURE body is JSON, and httpErrorMessage surfaces that `error`
        // verbatim: the backend already produces an actionable message, no
        // point swallowing it behind a generic "Failed to load diff".
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
  }, [open, artifactId])

  // lastSavePromise tracks the most recent in-flight title/body PATCH so the
  // submit handler awaits it before approving — otherwise a click-Save-then-
  // click-Open-PR race could promote the PR before the last edit lands.
  const lastSavePromise = useRef<Promise<void> | null>(null)

  // Title + body updates — explicit-Save model (PendingPRSummary's Edit / Save /
  // Cancel buttons drive this), not autosave. Pessimistic: PATCH first, throw on
  // !ok so PendingPRSummary stays in edit mode and surfaces the error rather
  // than showing a green "saved" over a write GitHub rejected.
  const patchPR = useCallback(
    async (body: object) => {
      try {
        await apiFetch(`/api/artifacts/${artifactId}/pr`, {
          method: 'PATCH',
          headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify(body),
        })
      } catch (err) {
        // Rethrown as a clean message: PendingPRSummary renders it and stays in
        // edit mode, so an HttpError's raw body must not reach it.
        throw new Error(httpErrorMessage(err, 'Could not save the pull request.'))
      }
    },
    [artifactId],
  )

  const handleUpdateTitle = useCallback(
    async (title: string) => {
      const normalizedTitle = title.trim()
      const p = patchPR({ title: normalizedTitle })
      lastSavePromise.current = p
      await p
      setPR((prev) =>
        prev ? { ...prev, details: { ...prev.details, title: normalizedTitle } } : prev,
      )
    },
    [patchPR],
  )

  const handleUpdateBody = useCallback(
    async (body: string) => {
      const p = patchPR({ body })
      lastSavePromise.current = p
      await p
      setPR((prev) => (prev ? { ...prev, details: { ...prev.details, body } } : prev))
    },
    [patchPR],
  )

  const handleSubmit = useCallback(async () => {
    setSubmitting(true)
    setSubmitError(null)
    try {
      // Wait for any in-flight save to settle before approving, so we don't race
      // a PATCH that's still writing to GitHub. A FAILED last save is
      // intentionally swallowed rather than blocking approval: the user already
      // saw its error banner in PendingPRSummary, and approve reads the live PR
      // from GitHub — which holds the last *successfully* saved state — so the
      // failed edit is simply dropped (the work proceeds with what actually
      // landed on GitHub, never stale local state).
      if (lastSavePromise.current) {
        try {
          await lastSavePromise.current
        } catch {
          /* failed save already surfaced to the user; drop it and approve live state */
        }
      }
      await apiFetch(`/api/artifacts/${artifactId}/approve`, {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
      })
      onClose()
    } catch (err) {
      // Inline (not full-screen): keep the editor visible so the user can retry
      // "Open PR" in place — a partial failure is safely re-runnable.
      setSubmitError(httpErrorMessage(err, 'Could not open the pull request.'))
    } finally {
      setSubmitting(false)
    }
  }, [artifactId, onClose])

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
                <div className="w-2 h-2 rounded-full bg-warm animate-pulse" />
                <h1 id={titleId} className="text-column font-semibold text-ink-1 tracking-tight">
                  Draft PR
                </h1>
                {pr && (
                  <span className="text-ui text-ink-3 font-mono">
                    {pr.details.owner}/{pr.details.repo} #{pr.details.number}
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
                    <span className="text-ui text-ink-3">Loading PR...</span>
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
              ) : pr ? (
                <div className="p-6 space-y-4 max-w-5xl mx-auto">
                  <PendingPRSummary
                    owner={pr.details.owner}
                    repo={pr.details.repo}
                    number={pr.details.number}
                    headBranch={pr.details.head_branch}
                    baseBranch={pr.details.base_branch}
                    url={pr.url}
                    title={pr.details.title}
                    body={pr.details.body}
                    onUpdateTitle={handleUpdateTitle}
                    onUpdateBody={handleUpdateBody}
                    onSubmit={handleSubmit}
                    onClose={onClose}
                    submitting={submitting}
                  />

                  {submitError && (
                    <div className="rounded-xl border border-alarm/30 bg-alarm/[0.06] px-4 py-3 text-ui text-ink-2">
                      <span className="font-semibold text-ink-1">Couldn't open PR:</span>{' '}
                      {submitError}. Your edits are saved on GitHub — you can retry Open PR.
                    </div>
                  )}

                  {truncationNote && (
                    <div className="rounded-xl border border-line-1 bg-tint-2 px-4 py-3 text-ui text-ink-2">
                      <span className="font-semibold text-ink-1">Diff truncated:</span>{' '}
                      {truncationNote}. The full PR is on GitHub — the cap only limits what's
                      rendered in this preview.
                    </div>
                  )}

                  {diffError && (
                    <div className="rounded-xl border border-alarm/30 bg-alarm/[0.06] px-4 py-3 text-ui text-ink-2">
                      <span className="font-semibold text-ink-1">Diff unavailable:</span>{' '}
                      {diffError}. You can still edit and open the PR — the diff just couldn't be
                      loaded right now.
                    </div>
                  )}

                  {/* Diff files — same DiffFile component the review overlay uses;
                      commentsByFile is always empty for the PR overlay (no
                      inline-comment surface). */}
                  <div className="space-y-3">
                    {files.map((file, i) => {
                      const path = file.newPath === '/dev/null' ? file.oldPath : file.newPath
                      return (
                        <DiffFile
                          key={path + i}
                          file={file}
                          comments={[]}
                          defaultCollapsed={files.length > 8}
                          onUpdateComment={() => {}}
                          onDeleteComment={() => {}}
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
