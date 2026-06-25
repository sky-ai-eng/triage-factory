import { useState } from 'react'
import Markdown from 'react-markdown'

interface Props {
  owner: string
  repo: string
  number: number
  headBranch: string
  baseBranch: string
  url: string
  title: string
  body: string
  // onUpdateTitle/Body return promises so the Save click handler
  // can await the PATCH before clearing edit-mode state. That
  // serialization is what prevents a click-Save-then-click-Open-PR
  // race from approving the PR in its pre-edit state.
  onUpdateTitle: (title: string) => Promise<void>
  onUpdateBody: (body: string) => Promise<void>
  onSubmit: () => void
  onClose: () => void
  submitting: boolean
}

// PendingPRSummary is the title/body editor + Open-PR button for the draft-PR
// overlay. Mirrors ReviewSummary's shape (header, body editor with Markdown
// preview, footer action cluster) but with PR-specific affordances:
//   - title editable as a single-line input (reviews don't have a title field)
//   - a #PR-number header linking to the real (draft) PR on GitHub
//   - explicit-Save UX matching ReviewSummary (no autosave)
//
// There's no draft toggle: the PR is created as a draft and stays one until
// "Open PR" (approve) promotes it to ready-for-review.
export default function PendingPRSummary({
  owner,
  repo,
  number,
  headBranch,
  baseBranch,
  url,
  title,
  body,
  onUpdateTitle,
  onUpdateBody,
  onSubmit,
  onClose,
  submitting,
}: Props) {
  const [editingTitle, setEditingTitle] = useState(false)
  const [editingBody, setEditingBody] = useState(false)
  const [rawView, setRawView] = useState(false)
  const [titleDraft, setTitleDraft] = useState(title)
  const [bodyDraft, setBodyDraft] = useState(body)
  // savingTitle/Body track the await on the PATCH so we can disable
  // submit while a save is in flight. With the parent's
  // lastSavePromise serialization the user is guaranteed not to
  // submit a stale row, but disabling the buttons during the in-
  // flight window also prevents the user from getting confused
  // about why the row hasn't appeared to update yet.
  const [savingTitle, setSavingTitle] = useState(false)
  const [savingBody, setSavingBody] = useState(false)
  // Per-field error state so the user sees *why* the save failed
  // (e.g. "title cannot be empty") instead of silently exiting edit
  // mode while the server still holds the old value.
  const [titleError, setTitleError] = useState<string | null>(null)
  const [bodyError, setBodyError] = useState<string | null>(null)

  const saveTitle = async () => {
    setSavingTitle(true)
    setTitleError(null)
    try {
      await onUpdateTitle(titleDraft)
      setEditingTitle(false)
    } catch (err) {
      // Stay in edit mode so the user can fix the input and retry.
      // Throwing means the parent's optimistic update never fired,
      // so server and client stay in sync at the old value.
      setTitleError(err instanceof Error ? err.message : String(err))
    } finally {
      setSavingTitle(false)
    }
  }
  const cancelTitle = () => {
    setTitleDraft(title)
    setTitleError(null)
    setEditingTitle(false)
  }
  const saveBody = async () => {
    setSavingBody(true)
    setBodyError(null)
    try {
      await onUpdateBody(bodyDraft)
      setEditingBody(false)
    } catch (err) {
      setBodyError(err instanceof Error ? err.message : String(err))
    } finally {
      setSavingBody(false)
    }
  }
  const cancelBody = () => {
    setBodyDraft(body)
    setBodyError(null)
    setEditingBody(false)
  }
  const saving = savingTitle || savingBody

  return (
    <div className="backdrop-blur-xl bg-surface-raised/70 border border-border-glass rounded-2xl shadow-sm shadow-black/[0.02] overflow-hidden">
      {/* Header */}
      <div className="px-5 pt-5 pb-4">
        <div className="flex items-start justify-between gap-4">
          <div className="min-w-0">
            <h2 className="text-[15px] font-semibold text-text-primary tracking-tight">Draft PR</h2>
            <p className="text-[12px] text-text-tertiary mt-0.5 font-mono truncate">
              {owner}/{repo} &middot; {headBranch} &rarr; {baseBranch}
            </p>
            <a
              href={url}
              target="_blank"
              rel="noreferrer"
              className="text-[10.5px] text-text-tertiary/70 hover:text-accent mt-0.5 font-mono inline-block transition-colors"
            >
              #{number} on GitHub &rarr;
            </a>
          </div>
        </div>
      </div>

      {/* Title (editable) */}
      <div className="px-5 pb-3">
        {editingTitle ? (
          <div className="space-y-2">
            <input
              value={titleDraft}
              onChange={(e) => setTitleDraft(e.target.value)}
              className="w-full text-[14px] font-medium text-text-primary bg-white/40 border border-border-subtle rounded-xl px-4 py-2.5 focus:outline-none focus:border-accent/30 focus:ring-1 focus:ring-accent/10"
              placeholder="PR title"
              autoFocus
              onKeyDown={(e) => {
                if (e.key === 'Enter') saveTitle()
                if (e.key === 'Escape') cancelTitle()
              }}
            />
            {titleError && <p className="text-[11px] text-dismiss px-1">{titleError}</p>}
            <div className="flex items-center gap-2 justify-end">
              <button
                onClick={cancelTitle}
                disabled={savingTitle}
                className="text-[11px] text-text-tertiary hover:text-text-secondary px-3 py-1.5 rounded-lg transition-colors disabled:opacity-50"
              >
                Cancel
              </button>
              <button
                onClick={saveTitle}
                disabled={savingTitle}
                className="text-[11px] font-medium text-white bg-accent hover:bg-accent/90 px-3 py-1.5 rounded-lg transition-colors disabled:opacity-50"
              >
                {savingTitle ? 'Saving…' : 'Save'}
              </button>
            </div>
          </div>
        ) : (
          <div
            onClick={() => {
              setTitleDraft(title)
              setEditingTitle(true)
            }}
            className="bg-white/30 rounded-xl px-4 py-2.5 border border-transparent hover:border-border-subtle transition-colors cursor-text group"
          >
            <span className="text-[14px] font-medium text-text-primary">
              {title || <span className="text-text-tertiary italic">No title</span>}
            </span>
            <span className="text-[10px] text-text-tertiary/70 ml-2 opacity-0 group-hover:opacity-100 transition-opacity">
              click to edit
            </span>
          </div>
        )}
      </div>

      {/* Body */}
      <div className="px-5 pb-4">
        {editingBody ? (
          <div className="space-y-2">
            <textarea
              value={bodyDraft}
              onChange={(e) => setBodyDraft(e.target.value)}
              className="w-full min-h-[120px] text-[13px] leading-relaxed text-text-primary bg-white/40 border border-border-subtle rounded-xl px-4 py-3 resize-y focus:outline-none focus:border-accent/30 focus:ring-1 focus:ring-accent/10 font-mono"
              placeholder="PR body (markdown supported)..."
              autoFocus
            />
            {bodyError && <p className="text-[11px] text-dismiss px-1">{bodyError}</p>}
            <div className="flex items-center gap-2 justify-end">
              <button
                onClick={cancelBody}
                disabled={savingBody}
                className="text-[11px] text-text-tertiary hover:text-text-secondary px-3 py-1.5 rounded-lg transition-colors disabled:opacity-50"
              >
                Cancel
              </button>
              <button
                onClick={saveBody}
                disabled={savingBody}
                className="text-[11px] font-medium text-white bg-accent hover:bg-accent/90 px-3 py-1.5 rounded-lg transition-colors disabled:opacity-50"
              >
                {savingBody ? 'Saving…' : 'Save'}
              </button>
            </div>
          </div>
        ) : (
          <div className="relative group">
            <div className="absolute top-2 right-2 flex items-center gap-1 opacity-0 group-hover:opacity-100 transition-opacity z-10">
              <button
                onClick={() => setRawView(!rawView)}
                className="text-[10px] text-text-tertiary hover:text-text-secondary px-1.5 py-0.5 rounded bg-white/60 border border-border-subtle transition-colors"
              >
                {rawView ? 'Preview' : 'Raw'}
              </button>
              <button
                onClick={() => {
                  setBodyDraft(body)
                  setEditingBody(true)
                }}
                className="text-[10px] text-text-tertiary hover:text-accent px-1.5 py-0.5 rounded bg-white/60 border border-border-subtle transition-colors"
              >
                Edit
              </button>
            </div>

            <div className="bg-white/30 rounded-xl px-4 py-3 border border-transparent hover:border-border-subtle transition-colors min-h-[48px]">
              {!body ? (
                <span
                  onClick={() => {
                    setBodyDraft(body)
                    setEditingBody(true)
                  }}
                  className="text-[13px] text-text-tertiary italic cursor-text"
                >
                  No description
                </span>
              ) : rawView ? (
                <pre className="text-[12.5px] leading-relaxed text-text-secondary font-mono whitespace-pre-wrap">
                  {body}
                </pre>
              ) : (
                <div className="review-markdown text-[13px] leading-relaxed text-text-secondary">
                  <Markdown>{body}</Markdown>
                </div>
              )}
            </div>
          </div>
        )}
      </div>

      {/* Footer actions */}
      <div className="px-5 py-3 border-t border-border-subtle flex items-center justify-end">
        <div className="flex items-center gap-2">
          <button
            onClick={onClose}
            className="text-[11px] font-medium text-text-tertiary hover:text-text-primary px-3 py-1.5 rounded-lg transition-colors"
          >
            Close
          </button>
          <button
            onClick={onSubmit}
            disabled={submitting || saving || editingTitle || editingBody}
            title={
              editingTitle || editingBody
                ? 'Save or cancel your edit before opening the PR'
                : saving
                  ? 'Waiting for your save to land before opening the PR'
                  : undefined
            }
            className={`flex items-center gap-1.5 text-[12px] font-semibold px-4 py-2 rounded-xl transition-all duration-150 ${
              submitting || saving || editingTitle || editingBody
                ? 'bg-accent/50 text-white/70 cursor-not-allowed'
                : 'text-white bg-claim hover:bg-claim/90'
            }`}
          >
            {submitting ? (
              <>
                <span className="inline-block w-3 h-3 border border-white/40 border-t-white rounded-full animate-spin" />
                Opening...
              </>
            ) : (
              <>Open PR</>
            )}
          </button>
        </div>
      </div>
    </div>
  )
}
