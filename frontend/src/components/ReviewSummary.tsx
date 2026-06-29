import { useState } from 'react'
import Markdown from 'react-markdown'

interface Props {
  owner: string
  repo: string
  prNumber: number
  reviewEvent: string
  reviewBody: string
  commentCount: number
  // How many commits the live PR head is ahead of the head the review was
  // finalized against (TFAC-500). null when it couldn't be computed; 0 means the
  // PR hasn't advanced. >0 surfaces the "N commits since this review was written"
  // staleness indicator + the Refresh affordance.
  commitsSinceFinalize: number | null
  // Refresh preview counts (TFAC-500): how many staged comments would be remapped
  // (moved) vs. dropped (outdated) when reconciling to the live head. Shown in the
  // confirm step so the human knows what they're discarding before refreshing.
  movedCount: number
  outdatedCount: number
  // onRefresh re-reconciles the staged comments to the live head (remap survivors,
  // drop outdated, re-pin the frame). Resolves once the overlay has reloaded.
  onRefresh: () => Promise<void>
  refreshing: boolean
  refreshError: string | null
  // Pessimistic: these stage the body/event into the artifact and reject on
  // failure, so the editor can surface the error and stay in edit mode rather
  // than show a green "saved" over a write that didn't land.
  onUpdateBody: (body: string) => Promise<void>
  onUpdateEvent: (event: string) => Promise<void>
  onSubmit: () => void
  // onClose dismisses the review popup without backend side
  // effects. Was previously named onDiscard to match an old
  // "Discard" button label that suggested destruction; renamed
  // alongside the SKY-207 button text rename so the prop, the
  // user-facing label, and the actual behavior all agree. The
  // genuinely destructive "throw the prepared review away and
  // re-queue the task" action lives on AgentCard's "Return to
  // queue" button.
  onClose: () => void
  submitting: boolean
}

const EVENT_OPTIONS = [
  {
    value: 'APPROVE',
    label: 'Approve',
    color: 'text-claim',
    bg: 'bg-claim/10',
    icon: (
      <svg
        width="14"
        height="14"
        viewBox="0 0 14 14"
        fill="none"
        stroke="currentColor"
        strokeWidth="1.5"
        strokeLinecap="round"
        strokeLinejoin="round"
      >
        <polyline points="3 7.5 5.5 10 11 4" />
      </svg>
    ),
  },
  {
    value: 'COMMENT',
    label: 'Comment',
    color: 'text-text-secondary',
    bg: 'bg-black/[0.04]',
    icon: (
      <svg
        width="14"
        height="14"
        viewBox="0 0 14 14"
        fill="none"
        stroke="currentColor"
        strokeWidth="1.5"
        strokeLinecap="round"
        strokeLinejoin="round"
      >
        <path d="M2 2.5h10a1 1 0 011 1v6a1 1 0 01-1 1H4l-2.5 2V3.5a1 1 0 011-1z" />
      </svg>
    ),
  },
  {
    value: 'REQUEST_CHANGES',
    label: 'Request Changes',
    color: 'text-dismiss',
    bg: 'bg-dismiss/10',
    icon: (
      <svg
        width="14"
        height="14"
        viewBox="0 0 14 14"
        fill="none"
        stroke="currentColor"
        strokeWidth="1.5"
        strokeLinecap="round"
        strokeLinejoin="round"
      >
        <circle cx="7" cy="7" r="5.5" />
        <line x1="5" y1="5" x2="9" y2="9" />
        <line x1="9" y1="5" x2="5" y2="9" />
      </svg>
    ),
  },
]

export default function ReviewSummary({
  owner,
  repo,
  prNumber,
  reviewEvent,
  reviewBody,
  commentCount,
  commitsSinceFinalize,
  movedCount,
  outdatedCount,
  onRefresh,
  refreshing,
  refreshError,
  onUpdateBody,
  onUpdateEvent,
  onSubmit,
  onClose,
  submitting,
}: Props) {
  const [editingBody, setEditingBody] = useState(false)
  const [rawView, setRawView] = useState(false)
  const [draft, setDraft] = useState(reviewBody)
  const [savingBody, setSavingBody] = useState(false)
  const [bodyError, setBodyError] = useState<string | null>(null)
  // pendingEvent disables the verdict buttons + flags which one is in flight so a
  // rapid double-click can't fire two stages; eventError surfaces a failed stage.
  const [pendingEvent, setPendingEvent] = useState<string | null>(null)
  const [eventError, setEventError] = useState<string | null>(null)
  // confirmingRefresh gates the Refresh action behind a one-step confirmation that
  // spells out what will move vs. be dropped (TFAC-500).
  const [confirmingRefresh, setConfirmingRefresh] = useState(false)

  const saveBody = async () => {
    setSavingBody(true)
    setBodyError(null)
    try {
      await onUpdateBody(draft)
      setEditingBody(false)
    } catch (err) {
      // Stay in edit mode so the user can retry; the parent never moved its state.
      setBodyError(err instanceof Error ? err.message : String(err))
    } finally {
      setSavingBody(false)
    }
  }

  const selectEvent = async (event: string) => {
    if (pendingEvent) return
    setPendingEvent(event)
    setEventError(null)
    try {
      await onUpdateEvent(event)
    } catch (err) {
      setEventError(err instanceof Error ? err.message : String(err))
    } finally {
      setPendingEvent(null)
    }
  }

  const cancelEdit = () => {
    setDraft(reviewBody)
    setBodyError(null)
    setEditingBody(false)
  }

  const currentEvent = EVENT_OPTIONS.find((e) => e.value === reviewEvent) ?? EVENT_OPTIONS[1]

  return (
    <div className="backdrop-blur-xl bg-surface-raised/70 border border-border-glass rounded-2xl shadow-sm shadow-black/[0.02] overflow-hidden">
      {/* Header */}
      <div className="px-5 pt-5 pb-4">
        <div className="flex items-start justify-between gap-4">
          <div>
            <h2 className="text-[15px] font-semibold text-text-primary tracking-tight">
              Review Preview
            </h2>
            <p className="text-[12px] text-text-tertiary mt-0.5">
              {owner}/{repo} #{prNumber}
            </p>
            {commitsSinceFinalize != null && commitsSinceFinalize > 0 && (
              <div className="mt-1.5 flex flex-col gap-1.5">
                <div className="flex items-center gap-2">
                  <span
                    className="inline-flex items-center gap-1.5 text-[11px] font-medium text-amber-600 bg-amber-500/[0.10] border border-amber-500/25 rounded-md px-2 py-0.5"
                    title="The PR has advanced since the agent wrote this review. The diff below shows the review as written; the per-comment badges show how each comment relates to the latest commit. Refresh to re-anchor against the latest."
                  >
                    <svg
                      width="11"
                      height="11"
                      viewBox="0 0 14 14"
                      fill="none"
                      stroke="currentColor"
                      strokeWidth="1.5"
                      strokeLinecap="round"
                      strokeLinejoin="round"
                    >
                      <circle cx="7" cy="7" r="5.5" />
                      <line x1="7" y1="4" x2="7" y2="7.5" />
                      <line x1="7" y1="10" x2="7" y2="10" />
                    </svg>
                    {commitsSinceFinalize} commit{commitsSinceFinalize !== 1 ? 's' : ''} since this
                    review was written
                  </span>
                  {!confirmingRefresh && (
                    <button
                      onClick={() => setConfirmingRefresh(true)}
                      disabled={refreshing}
                      className="text-[11px] font-medium text-accent hover:text-accent/80 px-2 py-0.5 rounded-md hover:bg-accent/[0.06] transition-colors disabled:opacity-60"
                    >
                      Refresh
                    </button>
                  )}
                </div>

                {confirmingRefresh && (
                  <div className="flex flex-col gap-1.5 rounded-lg border border-amber-500/25 bg-amber-500/[0.05] px-3 py-2 max-w-md">
                    <p className="text-[11px] text-text-secondary">
                      Re-anchor this review to the latest commit?{' '}
                      {movedCount > 0 && (
                        <>
                          {movedCount} comment{movedCount !== 1 ? 's' : ''} will move to their new
                          line{movedCount !== 1 ? 's' : ''}
                          {outdatedCount > 0 ? '; ' : '.'}
                        </>
                      )}
                      {outdatedCount > 0 && (
                        <span className="text-amber-700 font-medium">
                          {outdatedCount} outdated comment{outdatedCount !== 1 ? 's' : ''} will be
                          dropped.
                        </span>
                      )}
                      {movedCount === 0 && outdatedCount === 0 && (
                        <>No comments will move or be dropped.</>
                      )}
                    </p>
                    <div className="flex items-center gap-2">
                      <button
                        onClick={async () => {
                          try {
                            await onRefresh()
                            setConfirmingRefresh(false) // close only on success
                          } catch {
                            // refreshError already renders inside this panel; keep
                            // it open so the user sees the failure and can retry.
                          }
                        }}
                        disabled={refreshing}
                        className="text-[11px] font-medium text-white bg-accent hover:bg-accent/90 px-3 py-1 rounded-lg transition-colors disabled:opacity-60"
                      >
                        {refreshing ? 'Refreshing…' : 'Refresh'}
                      </button>
                      <button
                        onClick={() => setConfirmingRefresh(false)}
                        disabled={refreshing}
                        className="text-[11px] text-text-tertiary hover:text-text-secondary px-2.5 py-1 rounded-lg transition-colors"
                      >
                        Cancel
                      </button>
                    </div>
                    {refreshError && (
                      <p className="text-[10.5px] text-dismiss">
                        Couldn't refresh: {refreshError}. Retry.
                      </p>
                    )}
                  </div>
                )}
              </div>
            )}
          </div>

          {/* Event type selector */}
          <div className="flex items-center gap-1.5 shrink-0">
            {EVENT_OPTIONS.map((opt) => (
              <button
                key={opt.value}
                onClick={() => selectEvent(opt.value)}
                disabled={pendingEvent !== null}
                className={`flex items-center gap-1.5 text-[11px] font-medium px-2.5 py-1.5 rounded-lg border transition-all duration-150 disabled:opacity-60 ${
                  reviewEvent === opt.value
                    ? `${opt.color} ${opt.bg} border-current/20`
                    : 'text-text-tertiary border-transparent hover:bg-black/[0.03]'
                }`}
              >
                {opt.icon}
                {opt.label}
              </button>
            ))}
          </div>
        </div>
        {eventError && (
          <p className="mt-2 text-[11px] text-dismiss">
            Couldn't change the verdict: {eventError}. Retry.
          </p>
        )}
      </div>

      {/* Review body */}
      <div className="px-5 pb-4">
        {editingBody ? (
          <div className="space-y-2">
            <textarea
              value={draft}
              onChange={(e) => setDraft(e.target.value)}
              className="w-full min-h-[120px] text-[13px] leading-relaxed text-text-primary bg-white/40 border border-border-subtle rounded-xl px-4 py-3 resize-y focus:outline-none focus:border-accent/30 focus:ring-1 focus:ring-accent/10 font-mono"
              placeholder="Review summary..."
              autoFocus
            />
            {bodyError && (
              <p className="text-[11px] text-dismiss">
                Couldn't save: {bodyError}. Your edits are still here — retry Save.
              </p>
            )}
            <div className="flex items-center gap-2 justify-end">
              <button
                onClick={cancelEdit}
                className="text-[11px] text-text-tertiary hover:text-text-secondary px-3 py-1.5 rounded-lg transition-colors"
              >
                Cancel
              </button>
              <button
                onClick={saveBody}
                disabled={savingBody}
                className="text-[11px] font-medium text-white bg-accent hover:bg-accent/90 px-3 py-1.5 rounded-lg transition-colors disabled:opacity-60"
              >
                {savingBody ? 'Saving…' : 'Save'}
              </button>
            </div>
          </div>
        ) : (
          <div className="relative group">
            {/* View toggle + edit button */}
            <div className="absolute top-2 right-2 flex items-center gap-1 opacity-0 group-hover:opacity-100 transition-opacity z-10">
              <button
                onClick={() => setRawView(!rawView)}
                className="text-[10px] text-text-tertiary hover:text-text-secondary px-1.5 py-0.5 rounded bg-white/60 border border-border-subtle transition-colors"
              >
                {rawView ? 'Preview' : 'Raw'}
              </button>
              <button
                onClick={() => {
                  setDraft(reviewBody)
                  setEditingBody(true)
                }}
                className="text-[10px] text-text-tertiary hover:text-accent px-1.5 py-0.5 rounded bg-white/60 border border-border-subtle transition-colors"
              >
                Edit
              </button>
            </div>

            <div className="bg-white/30 rounded-xl px-4 py-3 border border-transparent hover:border-border-subtle transition-colors min-h-[48px]">
              {!reviewBody ? (
                <span
                  onClick={() => {
                    setDraft(reviewBody)
                    setEditingBody(true)
                  }}
                  className="text-[13px] text-text-tertiary italic cursor-text"
                >
                  No summary provided
                </span>
              ) : rawView ? (
                <pre className="text-[12.5px] leading-relaxed text-text-secondary font-mono whitespace-pre-wrap">
                  {reviewBody}
                </pre>
              ) : (
                <div className="review-markdown text-[13px] leading-relaxed text-text-secondary">
                  <Markdown>{reviewBody}</Markdown>
                </div>
              )}
            </div>
          </div>
        )}
      </div>

      {/* Footer actions */}
      <div className="px-5 py-3 border-t border-border-subtle flex items-center justify-between">
        <span className="text-[11px] text-text-tertiary">
          {commentCount} inline comment{commentCount !== 1 ? 's' : ''}
        </span>

        <div className="flex items-center gap-2">
          <button
            onClick={onClose}
            className="text-[11px] font-medium text-text-tertiary hover:text-text-primary px-3 py-1.5 rounded-lg transition-colors"
          >
            Close
          </button>
          <button
            onClick={onSubmit}
            disabled={submitting}
            className={`flex items-center gap-1.5 text-[12px] font-semibold px-4 py-2 rounded-xl transition-all duration-150 ${
              submitting
                ? 'bg-accent/50 text-white/70 cursor-not-allowed'
                : `text-white ${
                    reviewEvent === 'APPROVE'
                      ? 'bg-claim hover:bg-claim/90'
                      : reviewEvent === 'REQUEST_CHANGES'
                        ? 'bg-dismiss hover:bg-dismiss/90'
                        : 'bg-accent hover:bg-accent/90'
                  }`
            }`}
          >
            {submitting ? (
              <>
                <span className="inline-block w-3 h-3 border border-white/40 border-t-white rounded-full animate-spin" />
                Submitting...
              </>
            ) : (
              <>
                {currentEvent.icon}
                Submit to GitHub
              </>
            )}
          </button>
        </div>
      </div>
    </div>
  )
}
