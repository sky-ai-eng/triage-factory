import { useState, useEffect, useCallback, useRef } from 'react'
import { motion, AnimatePresence } from 'motion/react'
import { parseDiff } from 'react-diff-view'
import type { FileData } from 'react-diff-view'
import DiffFile from './DiffFile'
import PendingPRSummary from './PendingPRSummary'

// PRArtifact mirrors the JSON shape internal/server/artifacts_handler.go returns
// for a pull_request artifact. title/body are the LIVE PR values (fetched from
// GitHub via GetPR), not a local snapshot — the draft PR is a real GitHub object
// now, edited 1:1.
interface PRArtifact {
  id: string
  run_id?: string
  owner: string
  repo: string
  number: number
  head_branch: string
  base_branch: string
  title: string
  body: string
  url: string
  state: string
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

  // Fetch the PR artifact + diff. Reset stale state from any prior PR before the
  // new fetch lands so the user doesn't see leftover values briefly.
  useEffect(() => {
    if (!open || !artifactId) return
    let cancelled = false
    setLoading(true)
    setError(null)
    setPR(null)
    setFiles([])
    setTruncationNote(null)
    ;(async () => {
      try {
        const prRes = await fetch(`/api/artifacts/${artifactId}`)
        if (!prRes.ok) {
          const data = await prRes.json().catch(() => ({}))
          throw new Error(data.error || `Failed to load PR (${prRes.status})`)
        }
        const prData: PRArtifact = await prRes.json()
        if (cancelled) return
        setPR(prData)

        const diffRes = await fetch(`/api/artifacts/${artifactId}/diff`)
        if (!diffRes.ok) {
          // Server returns JSON {"error": "..."} on failure. Surface that body
          // verbatim so the user sees the actual reason instead of a generic
          // "Failed to load diff" — the backend already produces an actionable
          // message, no point swallowing it.
          const data = await diffRes.json().catch(() => ({}))
          throw new Error(data.error || `Failed to load diff (${diffRes.status})`)
        }
        const diffText = await diffRes.text()
        if (cancelled) return
        setFiles(parseDiff(diffText))
        setTruncationNote(diffRes.headers.get('X-Diff-Truncated'))
      } catch (err) {
        if (!cancelled) setError(err instanceof Error ? err.message : String(err))
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
      const res = await fetch(`/api/artifacts/${artifactId}`, {
        method: 'PATCH',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify(body),
      })
      if (!res.ok) {
        const data = await res.json().catch(() => ({}))
        throw new Error(data.error || `Save failed (${res.status})`)
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
      setPR((prev) => (prev ? { ...prev, title: normalizedTitle } : prev))
    },
    [patchPR],
  )

  const handleUpdateBody = useCallback(
    async (body: string) => {
      const p = patchPR({ body })
      lastSavePromise.current = p
      await p
      setPR((prev) => (prev ? { ...prev, body } : prev))
    },
    [patchPR],
  )

  const handleSubmit = useCallback(async () => {
    setSubmitting(true)
    try {
      // Wait for any in-flight save to land before approving. Otherwise the
      // server would read the snapshot in its pre-edit state and footer/promote
      // stale title/body. Errors during the wait are swallowed — we only care
      // the PATCH completed; approve's own error handling kicks in if needed.
      if (lastSavePromise.current) {
        try {
          await lastSavePromise.current
        } catch {
          /* noop */
        }
      }
      const res = await fetch(`/api/artifacts/${artifactId}/approve`, {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
      })
      if (!res.ok) {
        const data = await res.json().catch(() => ({}))
        throw new Error(data.error || 'Open PR failed')
      }
      onClose()
    } catch (err) {
      setError(err instanceof Error ? err.message : String(err))
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
                  Draft PR
                </h1>
                {pr && (
                  <span className="text-[12px] text-text-tertiary font-mono">
                    {pr.owner}/{pr.repo} #{pr.number}
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
                    <span className="text-[12px] text-text-tertiary">Loading PR...</span>
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
              ) : pr ? (
                <div className="p-6 space-y-4 max-w-5xl mx-auto">
                  <PendingPRSummary
                    owner={pr.owner}
                    repo={pr.repo}
                    number={pr.number}
                    headBranch={pr.head_branch}
                    baseBranch={pr.base_branch}
                    url={pr.url}
                    title={pr.title}
                    body={pr.body}
                    onUpdateTitle={handleUpdateTitle}
                    onUpdateBody={handleUpdateBody}
                    onSubmit={handleSubmit}
                    onClose={onClose}
                    submitting={submitting}
                  />

                  {truncationNote && (
                    <div className="rounded-xl border border-snooze/30 bg-snooze/[0.06] px-4 py-3 text-[12px] text-text-secondary">
                      <span className="font-semibold text-text-primary">Diff truncated:</span>{' '}
                      {truncationNote}. The full PR is on GitHub — the cap only limits what's
                      rendered in this preview.
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

                  {files.length === 0 && !truncationNote && (
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
