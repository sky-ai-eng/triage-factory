import { useState } from 'react'
import { Check, ShieldQuestion, X } from 'lucide-react'
import { stripWorktree } from '../../lib/worktree'
import { tint } from '../runstation/stationStyle'
import type { PendingPermission, PermissionDecisionInput } from '../../lib/permissions'

// PermissionPrompt — the head of a conversation's tool-approval queue, rendered with
// priority because it's parking the agent's turn. The SDK's prompt sentence
// (or a tool-name fallback) + a compact input summary, Deny / Allow, and an "N
// more" count when parallel tool calls stacked up. Shared between the RunStation dock and the board's AgentCard so the
// approve-what-runs control looks (and behaves) the same everywhere; pass
// `compact` on the card, where the surrounding card owns the vertical spacing.
export function PermissionPrompt({
  prompt,
  remaining,
  worktree,
  onResolve,
  compact = false,
}: {
  prompt: PendingPermission
  remaining: number
  worktree?: string
  onResolve?: (toolCallID: string, decision: PermissionDecisionInput) => Promise<void>
  /** Drop the standalone bottom margin — the host (a card) supplies spacing. */
  compact?: boolean
}) {
  const tone = 'var(--color-snooze)' // amber — a blocking "your move"
  // Single-flight: the first click wins. Without it a quick Deny→Allow puts
  // two resolves in flight and the broker honors whichever lands first —
  // non-deterministic from the user's view. Resolving resets on settle so a
  // transient failure (prompt stays up) remains answerable.
  const [resolving, setResolving] = useState(false)
  const resolve = async (behavior: 'allow' | 'deny') => {
    if (resolving || !onResolve) return
    setResolving(true)
    try {
      await onResolve(prompt.tool_call_id, { behavior })
    } finally {
      setResolving(false)
    }
  }
  // Worktree-relative paths, same as the transcript — but always the real
  // command/input, never the agent's description: this is a security
  // decision, so the user approves what will run, not what the agent says.
  const summary = stripWorktree(summarizePermissionInput(prompt.tool_name, prompt.input), worktree)
  // The SDK's own prompt sentence ("Claude wants to read foo.txt") when it
  // rendered one, else the reconstruction from the tool name. It only ever
  // replaces the label — the summary above stays the real input, so better copy
  // never costs the user sight of what they're approving.
  const headline = prompt.title || `Allow ${prompt.tool_name}?`
  return (
    <div
      className={`flex items-center gap-2.5 rounded-[5px] px-3 py-2 ${compact ? '' : 'mb-2.5'}`}
      style={{ background: tint(tone, 10), boxShadow: `inset 0 0 0 1px ${tint(tone, 32)}` }}
    >
      <ShieldQuestion size={15} className="shrink-0" style={{ color: tone }} />
      <span
        className={`shrink-0 font-mono text-[10px] font-semibold leading-none ${
          prompt.title ? 'max-w-[45%] truncate tracking-[0.04em]' : 'uppercase tracking-[0.12em]'
        }`}
        style={{ color: tone }}
        title={prompt.title || undefined}
      >
        {headline}
      </span>
      {summary && (
        <span
          className="min-w-0 flex-1 truncate font-mono text-[11.5px] text-text-secondary"
          title={summary}
        >
          {summary}
        </span>
      )}
      {!summary && <span className="min-w-0 flex-1" />}
      {remaining > 0 && (
        <span
          className="shrink-0 font-mono text-[10px] tabular-nums text-text-tertiary/80"
          title={`${remaining} more prompt${remaining === 1 ? '' : 's'} queued`}
        >
          +{remaining} more
        </span>
      )}
      <div className="flex shrink-0 items-center gap-2">
        <PromptButton
          tone="var(--color-dismiss)"
          onClick={() => void resolve('deny')}
          disabled={resolving || !onResolve}
          icon={<X size={11} />}
        >
          Deny
        </PromptButton>
        <PromptButton
          tone="var(--color-claim)"
          solid
          onClick={() => void resolve('allow')}
          disabled={resolving || !onResolve}
          icon={<Check size={11} />}
        >
          Allow
        </PromptButton>
      </div>
    </div>
  )
}

// PromptButton — the Deny/Allow control, matching the RunStation dock's button
// grammar (mono uppercase micro-label, toned solid for the affirmative, toned
// wash for the secondary) so the prompt reads identically on both surfaces.
function PromptButton({
  children,
  tone,
  onClick,
  disabled,
  solid,
  icon,
}: {
  children: React.ReactNode
  tone: string
  onClick: () => void
  disabled?: boolean
  solid?: boolean
  icon?: React.ReactNode
}) {
  return (
    <button
      type="button"
      onClick={onClick}
      disabled={disabled}
      className="inline-flex items-center gap-1.5 rounded-[4px] px-2.5 py-1 font-mono text-[10px] font-semibold uppercase tracking-[0.1em] transition-colors disabled:cursor-wait disabled:opacity-60"
      style={
        solid
          ? { color: 'var(--hmi-screen)', background: tone, boxShadow: `0 0 16px -4px ${tone}` }
          : {
              color: tone,
              background: tint(tone, 12),
              boxShadow: `inset 0 0 0 1px ${tint(tone, 26)}`,
            }
      }
    >
      {icon}
      {children}
    </button>
  )
}

// summarizePermissionInput renders a compact one-line preview of a tool call's
// input for the permission prompt — the command for Bash, the path for file
// tools, the pattern for search, else the first short string field or a trimmed
// JSON blob. Collapsed whitespace, capped length.
function summarizePermissionInput(tool: string, input: Record<string, unknown>): string {
  const str = (k: string) => (typeof input[k] === 'string' ? (input[k] as string) : '')
  let s = ''
  if (tool === 'Bash') s = str('command')
  else if (tool === 'Read' || tool === 'Write' || tool === 'Edit') s = str('file_path')
  else if (tool === 'Glob' || tool === 'Grep') s = str('pattern')
  else if (tool === 'WebFetch') s = str('url')
  else if (tool === 'WebSearch') s = str('query')
  // Arbitrary tools (MCP servers, etc.) fall through to the first string field
  // below — best-effort, since their input shape isn't known here.
  if (!s) {
    for (const v of Object.values(input)) {
      if (typeof v === 'string' && v) {
        s = v
        break
      }
    }
  }
  if (!s) {
    try {
      s = JSON.stringify(input)
    } catch {
      s = ''
    }
  }
  s = s.replace(/\s+/g, ' ').trim()
  return s.length > 80 ? s.slice(0, 79) + '…' : s
}
