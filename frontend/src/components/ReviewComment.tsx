import { useState } from 'react'

interface Props {
  id: string
  path: string
  line: number
  body: string
  severity?: string
  onUpdate: (id: string, body: string) => void
  onDelete: (id: string) => void
}

// Native chip styling per severity level — the diff UI renders this
// instead of the shields.io badge (which is only applied when the review
// is posted to GitHub). Palette tracks domain.severityBadgeColor:
// BLOCKER→red, MAJOR→orange, MINOR→amber, CLEAN→blue.
const SEVERITY_CHIP: Record<string, string> = {
  BLOCKER: 'bg-red-500/[0.10] text-red-600 border-red-500/20',
  MAJOR: 'bg-orange-500/[0.10] text-orange-600 border-orange-500/20',
  MINOR: 'bg-amber-500/[0.12] text-amber-600 border-amber-500/25',
  CLEAN: 'bg-blue-500/[0.10] text-blue-600 border-blue-500/20',
}

function SeverityChip({ severity }: { severity?: string }) {
  if (!severity) return null
  const level = severity.toUpperCase()
  const cls = SEVERITY_CHIP[level]
  if (!cls) return null
  return (
    <span
      className={`inline-flex items-center rounded px-1.5 py-px text-[9px] font-semibold uppercase tracking-wider border ${cls}`}
    >
      {level}
    </span>
  )
}

export default function ReviewComment({
  id,
  path,
  line,
  body,
  severity,
  onUpdate,
  onDelete,
}: Props) {
  const [editing, setEditing] = useState(false)
  const [draft, setDraft] = useState(body)

  const save = () => {
    if (draft.trim() && draft !== body) {
      onUpdate(id, draft)
    }
    setEditing(false)
  }

  const cancel = () => {
    setDraft(body)
    setEditing(false)
  }

  // Parse suggestion blocks for display
  const renderBody = (text: string) => {
    const parts: React.ReactNode[] = []
    const regex = /```suggestion\n([\s\S]*?)```/g
    let last = 0
    let match: RegExpExecArray | null

    while ((match = regex.exec(text)) !== null) {
      if (match.index > last) {
        parts.push(
          <span key={last} className="whitespace-pre-wrap">
            {text.slice(last, match.index)}
          </span>,
        )
      }
      parts.push(
        <div
          key={match.index}
          className="mt-2 mb-1 rounded-lg border border-claim/20 overflow-hidden"
        >
          <div className="px-2.5 py-1 bg-claim/[0.06] text-[10px] font-semibold text-claim uppercase tracking-wider">
            Suggestion
          </div>
          <pre className="px-3 py-2 text-[12px] leading-relaxed bg-claim/[0.03] font-mono overflow-x-auto">
            {match[1]}
          </pre>
        </div>,
      )
      last = match.index + match[0].length
    }

    if (last < text.length) {
      parts.push(
        <span key={last} className="whitespace-pre-wrap">
          {text.slice(last)}
        </span>,
      )
    }

    return parts.length > 0 ? parts : <span className="whitespace-pre-wrap">{text}</span>
  }

  return (
    <div className="mx-3 my-2 group">
      <div className="backdrop-blur-xl bg-surface-raised/80 border border-border-glass rounded-xl shadow-sm shadow-black/[0.03] overflow-hidden">
        {/* Header */}
        <div className="flex items-center justify-between px-3 py-1.5 border-b border-border-subtle">
          <div className="flex items-center gap-2 min-w-0">
            <SeverityChip severity={severity} />
            <span className="text-[10px] text-text-tertiary font-medium truncate">
              {path}:{line}
            </span>
          </div>
          <div className="flex items-center gap-1 opacity-0 group-hover:opacity-100 transition-opacity">
            {!editing && (
              <>
                <button
                  onClick={() => setEditing(true)}
                  className="text-[10px] text-text-tertiary hover:text-accent px-1.5 py-0.5 rounded transition-colors"
                >
                  Edit
                </button>
                <button
                  onClick={() => onDelete(id)}
                  className="text-[10px] text-text-tertiary hover:text-dismiss px-1.5 py-0.5 rounded transition-colors"
                >
                  Delete
                </button>
              </>
            )}
          </div>
        </div>

        {/* Body */}
        <div className="px-3 py-2.5">
          {editing ? (
            <div className="space-y-2">
              <textarea
                value={draft}
                onChange={(e) => setDraft(e.target.value)}
                className="w-full min-h-[80px] text-[12.5px] leading-relaxed text-text-primary bg-white/40 border border-border-subtle rounded-lg px-3 py-2 font-mono resize-y focus:outline-none focus:border-accent/30 focus:ring-1 focus:ring-accent/10"
                autoFocus
              />
              <div className="flex items-center gap-2 justify-end">
                <button
                  onClick={cancel}
                  className="text-[11px] text-text-tertiary hover:text-text-secondary px-2.5 py-1 rounded-lg transition-colors"
                >
                  Cancel
                </button>
                <button
                  onClick={save}
                  className="text-[11px] font-medium text-white bg-accent hover:bg-accent/90 px-3 py-1 rounded-lg transition-colors"
                >
                  Save
                </button>
              </div>
            </div>
          ) : (
            <div className="text-[12.5px] leading-relaxed text-text-secondary">
              {renderBody(body)}
            </div>
          )}
        </div>
      </div>
    </div>
  )
}
