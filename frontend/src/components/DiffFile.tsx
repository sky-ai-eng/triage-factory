import { useState, useMemo } from 'react'
import { Diff, Hunk, markEdits, tokenize, getChangeKey } from 'react-diff-view'
import type { FileData, HunkData, ChangeData } from 'react-diff-view'
import ReviewComment from './ReviewComment'
import { diffViewRefractor, languageForPath } from '../lib/highlight'

export interface FileComment {
  id: string
  path: string
  line: number
  startLine?: number
  body: string
  severity?: string
  // Per-comment freshness vs. the live PR head (TFAC-500): 'current' | 'moved' |
  // 'outdated' | 'unknown'. mappedLine is the new-side line when 'moved'.
  freshness?: string
  mappedLine?: number
}

interface Props {
  file: FileData
  comments: FileComment[]
  defaultCollapsed?: boolean
  // Promise-returning (pessimistic) so the review overlay's per-comment edit/delete
  // can surface GitHub errors; a void no-op is still accepted (the PR overlay).
  onUpdateComment: (id: string, body: string) => void | Promise<void>
  onDeleteComment: (id: string) => void | Promise<void>
  // readOnly hides each comment's edit/delete affordances — a resolved review's
  // comments are published and immutable.
  readOnly?: boolean
}

export default function DiffFile({
  file,
  comments,
  defaultCollapsed = false,
  onUpdateComment,
  onDeleteComment,
  readOnly = false,
}: Props) {
  const [collapsed, setCollapsed] = useState(defaultCollapsed)

  // Build the widgets map (changeKey → ReactNode) and collect comments that can't
  // be anchored to a line shown in the diff. The overlay renders the FINALIZE-TIME
  // frame (TFAC-500), so a staged comment anchors at the line it was written
  // against (comment.line) — its freshness badge separately conveys how it relates
  // to the live head. The rare comment that still doesn't match a shown line (its
  // code became identical to base, or its patch was omitted from a truncated diff)
  // falls through to the unanchored block below rather than vanishing.
  const { widgets, unanchored } = useMemo(() => {
    const map: Record<string, React.ReactNode> = {}
    const placed = new Set<string>()

    for (const comment of comments) {
      // Find the change in any hunk that matches this comment's line
      for (const hunk of file.hunks) {
        let done = false
        for (const change of hunk.changes) {
          if (getLineNumber(change) === comment.line) {
            const key = getChangeKey(change)
            // Stack multiple comments on the same line
            const existing = map[key]
            map[key] = (
              <>
                {existing}
                <ReviewComment
                  key={comment.id}
                  id={comment.id}
                  path={comment.path}
                  line={comment.line}
                  body={comment.body}
                  severity={comment.severity}
                  freshness={comment.freshness}
                  mappedLine={comment.mappedLine}
                  onUpdate={onUpdateComment}
                  onDelete={onDeleteComment}
                  readOnly={readOnly}
                />
              </>
            )
            placed.add(comment.id)
            done = true
            break
          }
        }
        if (done) break
      }
    }

    return { widgets: map, unanchored: comments.filter((c) => !placed.has(c.id)) }
  }, [comments, file.hunks, onUpdateComment, onDeleteComment, readOnly])

  // Tokenize with syntax highlighting + word-level edit marks
  const displayPath = file.newPath === '/dev/null' ? file.oldPath : file.newPath
  const tokens = useMemo(() => {
    const lang = languageForPath(displayPath)
    try {
      const result = tokenize(file.hunks, {
        ...(lang
          ? { highlight: true, refractor: diffViewRefractor, language: lang }
          : { highlight: false }),
        enhancers: [markEdits(file.hunks, { type: 'block' })],
      })
      return result
    } catch (err) {
      console.warn('[DiffFile] tokenize failed for', displayPath, err)
      return undefined
    }
  }, [file.hunks, displayPath])

  // Count additions and deletions
  const stats = useMemo(() => {
    let additions = 0
    let deletions = 0
    for (const hunk of file.hunks) {
      for (const change of hunk.changes) {
        if (change.type === 'insert') additions++
        if (change.type === 'delete') deletions++
      }
    }
    return { additions, deletions }
  }, [file.hunks])

  const commentCount = comments.length

  // A file with no hunks renders an empty body. That happens for genuinely
  // empty diffs, but also for the too-large-diff fallback, where the backend
  // reassembles per-file patches and emits patch-less placeholder entries
  // (binary files, oversized files whose patch GitHub dropped, pure renames) —
  // react-diff-view keeps the file header but parses no hunks, so the
  // distinction the diff text carried is lost. Show a note instead of a blank
  // body so a truncated PR reads as a list of accounted-for files rather than
  // mystery empty rows.
  const emptyBodyNote =
    file.hunks.length === 0
      ? file.type === 'rename'
        ? 'Renamed with no textual changes (or content omitted from the truncated diff).'
        : 'No diff to display — file content omitted (binary, or too large to render in this view). Open the PR on GitHub to see the full change.'
      : null

  return (
    <div className="backdrop-blur-xl bg-raised/70 border border-line-1 rounded-2xl overflow-hidden shadow-float shadow-black/[0.02]">
      {/* File header */}
      <button
        onClick={() => setCollapsed(!collapsed)}
        className="w-full flex items-center gap-3 px-4 py-2.5 hover:bg-tint-1 transition-colors text-left"
      >
        {/* Chevron */}
        <svg
          width="12"
          height="12"
          viewBox="0 0 12 12"
          fill="none"
          stroke="currentColor"
          strokeWidth="1.5"
          strokeLinecap="round"
          strokeLinejoin="round"
          className={`text-ink-3 shrink-0 transition-transform duration-200 ${
            collapsed ? '' : 'rotate-90'
          }`}
        >
          <polyline points="4 2 8 6 4 10" />
        </svg>

        {/* File path */}
        <span className="text-card-title font-mono text-ink-1 truncate flex-1">{displayPath}</span>

        {/* Comment count */}
        {commentCount > 0 && (
          <span className="text-label font-medium text-ink-2 bg-tint-2 px-2 py-0.5 rounded-full shrink-0">
            {commentCount} comment{commentCount !== 1 ? 's' : ''}
          </span>
        )}

        {/* Stats */}
        <div className="flex items-center gap-2 shrink-0">
          {stats.additions > 0 && (
            <span className="text-reported font-medium text-ink-2">+{stats.additions}</span>
          )}
          {stats.deletions > 0 && (
            <span className="text-reported font-medium text-alarm">-{stats.deletions}</span>
          )}
        </div>
      </button>

      {/* Diff content */}
      {!collapsed &&
        (emptyBodyNote ? (
          <div className="border-t border-line-1 px-4 py-3">
            <p className="text-ui text-ink-3 italic">{emptyBodyNote}</p>
          </div>
        ) : (
          <div className="border-t border-line-1 overflow-x-auto">
            <Diff
              viewType="unified"
              diffType={file.type}
              hunks={file.hunks}
              widgets={widgets}
              tokens={tokens}
            >
              {(hunks: HunkData[]) => hunks.map((hunk) => <Hunk key={hunk.content} hunk={hunk} />)}
            </Diff>
          </div>
        ))}

      {/* Safety net: comments that couldn't be anchored to a shown line (their
          code became identical to base, or the patch was omitted from a truncated
          diff). Kept visible — with their badge — rather than silently dropped. */}
      {!collapsed && unanchored.length > 0 && (
        <div className="border-t border-line-1 px-3 py-2">
          <p className="px-1 pb-1 text-label text-ink-3">
            {unanchored.length} comment{unanchored.length !== 1 ? 's' : ''} not shown in this diff
          </p>
          {unanchored.map((c) => (
            <ReviewComment
              key={c.id}
              id={c.id}
              path={c.path}
              line={c.line}
              body={c.body}
              severity={c.severity}
              freshness={c.freshness}
              mappedLine={c.mappedLine}
              onUpdate={onUpdateComment}
              onDelete={onDeleteComment}
              readOnly={readOnly}
            />
          ))}
        </div>
      )}
    </div>
  )
}

function getLineNumber(change: ChangeData): number {
  if (change.type === 'normal') return change.newLineNumber
  if (change.type === 'insert') return change.lineNumber
  if (change.type === 'delete') return change.lineNumber
  return 0
}
