import { useEffect, useMemo, useRef, useState } from 'react'
import Markdown from 'react-markdown'
import {
  Send,
  Square,
  ChevronDown,
  ChevronRight,
  AlertCircle,
  RotateCcw,
  BookOpen,
} from 'lucide-react'
import { useNavigate } from 'react-router'
import { useCuratorChat } from '../hooks/useCuratorChat'
import { useOrgHref } from '../hooks/useOrgHref'
import { linkifyMarkdown, type LinkifyContext } from '../lib/linkify'
import { toast } from './Toast/toastStore'
import PromptPicker from './PromptPicker'
import type { Message, CuratorRequestWithMessages, Project, Prompt, ToolCall } from '../types'

const SYSTEM_TICKET_SPEC_PROMPT_ID = 'system-ticket-spec'

// CuratorChat renders the streamed conversation for one project on
// the right column of /projects/:id. Backend wiring lives in the
// useCuratorChat hook; this file owns layout + per-message rendering.
//
// Design notes:
//   - Same visual primitives as AgentCard (status colors, tool-call
//     vocabulary, monospace timestamps) but with breathing room — the
//     panel is for active conversation, not transcript review.
//   - Tool calls collapse by default; the latest one auto-expands
//     while the request is still running. The user can manually
//     toggle either direction.
//   - Hidden subtype="context_change" rows (audit anchors)
//     are filtered out — they exist to inform the agent, not the
//     user.

interface Props {
  project: Project
  /** Patches the project. Used by the spec-skill picker in the header
   *  to write the new prompt id; the parent's PATCH handler is what
   *  actually validates + persists. Returns undefined / true on
   *  success, false on failure (the parent toasts on error). */
  onPatch: (body: Record<string, unknown>) => Promise<boolean | undefined>
}

export default function CuratorChat({ project, onPatch }: Props) {
  const projectId = project.id
  const chat = useCuratorChat(projectId)
  const navigate = useNavigate()
  const orgHref = useOrgHref()

  // Lazy-fetch the prompts list so the header control can resolve the
  // active prompt's name for supporting UI (for example the button's
  // tooltip/title) without waiting for the picker to open. Same
  // per-mount AbortController pattern as the linkifier's settings
  // fetch — module-scope caching here would freeze the resolved
  // active-name metadata past edits made on the /prompts page.
  const [prompts, setPrompts] = useState<Prompt[]>([])
  // Scope to the project's team. The curator runs the spec prompt under
  // this project's team, and the PATCH validation only checks org
  // membership (projects.go), so an unscoped list would let a team-A
  // project attach (and then run) a team-B prompt. The system spec prompt
  // is visibility='org', so it stays in a team-scoped list. '' (legacy /
  // solo) falls back to unscoped, matching the prior behavior.
  const promptTeamId = project.team_id
  const refetchPrompts = useMemo(
    () => () => {
      const ac = new AbortController()
      const q = promptTeamId ? `?team_id=${encodeURIComponent(promptTeamId)}` : ''
      fetch(`/api/prompts${q}`, { signal: ac.signal })
        .then((r) => (r.ok ? r.json() : null))
        .then((d: Prompt[] | null) => {
          if (ac.signal.aborted) return
          if (Array.isArray(d)) setPrompts(d)
        })
        .catch(() => {
          // Header button degrades to "Spec skill" without a name.
        })
      return () => ac.abort()
    },
    [promptTeamId],
  )
  useEffect(() => refetchPrompts(), [refetchPrompts])

  const [pickerOpen, setPickerOpen] = useState(false)

  // Effective spec prompt: project's choice, then the seeded default,
  // then nothing. Mirrors the backend resolution order in
  // internal/curator/skill.go so the badge matches what the next
  // dispatch will materialize.
  const hasProjectSpecPrompt =
    !!project.spec_authorship_blueprint_id &&
    prompts.some((p) => p.id === project.spec_authorship_blueprint_id)
  const effectiveSpecPromptID = hasProjectSpecPrompt
    ? project.spec_authorship_blueprint_id
    : prompts.some((p) => p.id === SYSTEM_TICKET_SPEC_PROMPT_ID)
      ? SYSTEM_TICKET_SPEC_PROMPT_ID
      : ''
  const activeSpecPrompt = prompts.find((p) => p.id === effectiveSpecPromptID)
  const skillButtonLabel = activeSpecPrompt?.name ?? 'Spec skill'

  const handleSpecSelect = async (promptId: string) => {
    setPickerOpen(false)
    // Compare against the effective id (project's choice OR the inherited
    // default), not the raw stored field. If the project is inheriting
    // the seeded default and the user picks the same prompt the picker
    // already shows as active, no PATCH is needed — sending one would
    // silently flip the project from "inherit default" to "explicitly
    // pin this id," a semantic change with no current behavior delta
    // but one that prevents future default swaps from picking it up.
    if (promptId === effectiveSpecPromptID) return
    // Failure toast lives in ProjectDetail's patch() (handles both HTTP
    // and network errors). Toasting here too would double-fire.
    await onPatch({ spec_authorship_blueprint_id: promptId })
  }

  // Per-mount fetch of the Jira base URL for the linkifier. Earlier
  // versions cached at module scope, but that turned a transient fetch
  // failure into a session-permanent one. Per-mount is fine.
  // AbortController gates the setter against unmount/remount
  // so a slow response can't land on a stale view.
  const [jiraBaseURL, setJiraBaseURL] = useState<string | undefined>(undefined)
  useEffect(() => {
    const ac = new AbortController()
    fetch('/api/settings/org', { signal: ac.signal })
      .then((r) => (r.ok ? r.json() : null))
      .then((d) => {
        if (ac.signal.aborted) return
        setJiraBaseURL(d?.jira_base_url || undefined)
      })
      .catch(() => {
        // Linkifier degrades gracefully — Jira refs render as plain
        // text until a future remount succeeds. No toast: the user
        // didn't ask for anything that depends on this.
      })
    return () => ac.abort()
  }, [])
  const linkifyCtx: LinkifyContext = useMemo(() => ({ jiraBaseURL }), [jiraBaseURL])

  const scrollRef = useRef<HTMLDivElement>(null)
  // Auto-scroll on new content. We track the "latest message id" so a
  // bare status flip (no new content) doesn't yank the viewport. When
  // the user has scrolled up to read history, we suppress auto-scroll
  // until they hit bottom again — otherwise the agent's stream would
  // fight the user's scroll.
  const lastMessageKey = useMemo(() => {
    let last = ''
    for (const r of chat.requests) {
      if (r.messages.length > 0) {
        last = `${r.id}:${r.messages[r.messages.length - 1].id}`
      } else {
        last = `${r.id}:0`
      }
    }
    return last
  }, [chat.requests])

  const [autoScroll, setAutoScroll] = useState(true)
  useEffect(() => {
    const el = scrollRef.current
    if (!el) return
    if (autoScroll) {
      el.scrollTop = el.scrollHeight
    }
  }, [lastMessageKey, autoScroll])

  // Composer state lives here so the cancel button can reach it (we
  // don't clear the textarea on cancel — the user may want to re-send).
  const [draft, setDraft] = useState('')
  const composerRef = useRef<HTMLTextAreaElement>(null)
  const sending = useRef(false)

  const submit = async () => {
    if (sending.current) return
    if (!draft.trim()) return
    if (chat.inFlight) return
    sending.current = true
    const text = draft
    setDraft('')
    // Force-pin to the bottom on send. If the user was scrolled up
    // reading older history when they hit submit, they want to see
    // the message they just sent — the autoScroll-from-distance
    // heuristic alone wouldn't unstick that case.
    setAutoScroll(true)
    try {
      await chat.send(text)
    } finally {
      sending.current = false
      composerRef.current?.focus()
    }
  }

  const onCancel = async () => {
    await chat.cancel()
  }

  const onScroll = () => {
    const el = scrollRef.current
    if (!el) return
    const distanceFromBottom = el.scrollHeight - el.scrollTop - el.clientHeight
    setAutoScroll(distanceFromBottom < 80)
  }

  return (
    <section
      className="
        relative overflow-hidden rounded-2xl border border-line-1
        bg-raised
        shadow-float shadow-black/[0.03] backdrop-blur-xl
        lg:sticky lg:top-24 lg:h-[calc(100vh-12rem)]
        flex flex-col
      "
    >
      {/* Decorative diffuse highlight — same vocabulary as the other Cards */}
      <span
        aria-hidden
        className="pointer-events-none absolute -left-8 -top-8 h-24 w-24 rounded-full bg-raised blur-2xl"
      />

      <ChatHeader
        chat={chat}
        skillButtonLabel={skillButtonLabel}
        onOpenSkillPicker={() => setPickerOpen(true)}
      />

      <div
        ref={scrollRef}
        onScroll={onScroll}
        className="relative flex-1 overflow-y-auto px-5 py-4 space-y-6"
      >
        {chat.loading ? (
          <div className="text-ui text-ink-3">Loading conversation…</div>
        ) : chat.loadError ? (
          <div className="rounded-lg border border-alarm/20 bg-alarm/5 px-3 py-2 text-ui text-alarm flex items-start gap-2">
            <AlertCircle size={14} className="shrink-0 mt-0.5" />
            <span>{chat.loadError}</span>
          </div>
        ) : chat.requests.length === 0 ? (
          <EmptyState />
        ) : (
          chat.requests.map((req, i) => (
            <RequestBlock
              key={req.id}
              request={req}
              linkifyCtx={linkifyCtx}
              isLatest={i === chat.requests.length - 1}
            />
          ))
        )}
      </div>

      <Composer
        draft={draft}
        onDraftChange={setDraft}
        onSubmit={submit}
        onCancel={onCancel}
        inFlight={!!chat.inFlight}
        cancellable={!!chat.inFlight && !chat.inFlight.id.startsWith('optimistic-')}
        textareaRef={composerRef}
        sendError={chat.sendError}
        totalCostUSD={chat.totalCostUSD}
      />
      <PromptPicker
        open={pickerOpen}
        title="Curator spec skill"
        subtitle="The Curator uses this prompt as a Claude Code skill when authoring tickets. Edits take effect on the next turn."
        selectLabel="Use skill"
        selectedId={effectiveSpecPromptID}
        onSelect={handleSpecSelect}
        onClose={() => setPickerOpen(false)}
        onEditPrompts={() => navigate(orgHref('/prompts'))}
        // Scope the picker's options to the project's team (teamValue
        // without onTeamChange scopes the fetch without a header picker —
        // the team is fixed by the project, not chosen here).
        teamValue={project.team_id}
      />
    </section>
  )
}

function ChatHeader({
  chat,
  skillButtonLabel,
  onOpenSkillPicker,
}: {
  chat: ReturnType<typeof useCuratorChat>
  skillButtonLabel: string
  onOpenSkillPicker: () => void
}) {
  const status = chat.inFlight?.status ?? 'idle'
  const tone =
    status === 'running'
      ? 'text-ink-2'
      : status === 'queued'
        ? 'text-ink-2'
        : status === 'failed'
          ? 'text-alarm'
          : status === 'cancelled'
            ? 'text-ink-3'
            : 'text-ink-2'
  const dot =
    status === 'running'
      ? '●'
      : status === 'queued'
        ? '◌'
        : status === 'failed'
          ? '✗'
          : status === 'cancelled'
            ? '◼'
            : '○'
  const label = status === 'idle' ? 'Idle' : status[0].toUpperCase() + status.slice(1)

  const onReset = async () => {
    // Confirm only when there's actual transcript to lose. Empty
    // sessions resetting on a phantom click would just be friction.
    const hasHistory = chat.requests.length > 0
    if (hasHistory) {
      const ok = confirm(
        'Reset the Curator chat?\n\nThis clears the session and deletes message history. The next message starts a brand-new conversation.',
      )
      if (!ok) return
    }
    const result = await chat.reset()
    if (!result.ok && result.error) {
      // 409 conflicts get warning tone (recoverable — the user just
      // needs to cancel the in-flight turn first); other failures
      // (network, 500) are real errors. Either way it's the toast
      // store, not alert() — alert is blocking + unstyled.
      if (result.conflict) {
        toast.warning(result.error)
      } else {
        toast.error(result.error)
      }
    }
  }

  return (
    <header className="relative px-5 pt-4 pb-3 border-b border-line-1/60">
      <div className="flex items-center justify-between">
        <h2 className="text-body font-semibold tracking-tight text-ink-1 uppercase">Curator</h2>
        <div className="flex items-center gap-2 text-reported">
          <button
            type="button"
            onClick={onOpenSkillPicker}
            aria-label="Choose ticket-spec skill"
            title={`Curator ticket-spec skill: ${skillButtonLabel}`}
            className="
              inline-flex items-center gap-1.5
              h-6 px-2 rounded-full
              text-ink-2 hover:text-ink-1 hover:bg-tint-3
              border border-line-1/70
              transition-colors
            "
          >
            <BookOpen size={11} className="shrink-0" />
            <span>Spec skill</span>
          </button>
          <span className={`inline-flex items-center gap-1 ${tone}`}>
            <span aria-hidden>{dot}</span>
            <span>{label}</span>
            {status === 'running' && (
              <span className="inline-block w-1.5 h-1.5 rounded-full bg-tint-2 animate-pulse ml-0.5" />
            )}
          </span>
          {chat.totalCostUSD > 0 && (
            <span className="text-ink-3 tabular-nums">${chat.totalCostUSD.toFixed(3)}</span>
          )}
          <button
            type="button"
            onClick={onReset}
            disabled={!!chat.inFlight}
            aria-label="Reset chat"
            title={
              chat.inFlight
                ? 'Cancel the current turn before resetting'
                : 'Start a fresh conversation (clears session + history)'
            }
            className="
              inline-flex items-center justify-center
              h-6 w-6 rounded-full
              text-ink-3 hover:text-ink-1 hover:bg-tint-3
              disabled:opacity-30 disabled:cursor-not-allowed
              transition-colors
            "
          >
            <RotateCcw size={11} />
          </button>
        </div>
      </div>
    </header>
  )
}

function EmptyState() {
  return (
    <div className="h-full flex items-center justify-center px-2">
      <div className="text-center max-w-xs">
        <div className="text-column font-medium tracking-tight text-ink-1 mb-2">
          A new conversation.
        </div>
        <div className="text-ui text-ink-3 leading-relaxed">
          The Curator has the project in view — pinned repos, trackers, knowledge.
          <br />
          Ask anything.
        </div>
      </div>
    </div>
  )
}

// RequestBlock renders one user→assistant exchange: the user bubble,
// then the streamed assistant turns underneath.
function RequestBlock({
  request,
  linkifyCtx,
  isLatest,
}: {
  request: CuratorRequestWithMessages
  linkifyCtx: LinkifyContext
  isLatest: boolean
}) {
  // Filter out hidden context-change audit rows (server-side anchors for
  // the agent's next turn) and reasoning rows — the chat shows the
  // curator's answers, not its interior monologue.
  const visibleMessages = useMemo(
    () =>
      request.messages.filter((m) => m.subtype !== 'context_change' && m.subtype !== 'thinking'),
    [request.messages],
  )

  // Pair tool results to their tool_use calls. Mirrors AgentCard's
  // pairing so the visual relationship is the same — call up top,
  // result nested underneath.
  const toolResults = useMemo(() => {
    const map = new Map<string, Message>()
    for (const m of visibleMessages) {
      if (m.role === 'tool' && m.tool_call_id) {
        map.set(m.tool_call_id, m)
      }
    }
    return map
  }, [visibleMessages])

  // Compute the latest tool_call id across the request so RequestBlock
  // can auto-expand exactly that one card. New tool call arrives →
  // recompute → previous latest collapses cleanly. The user can still
  // override either way.
  const latestToolCallID = useMemo(() => {
    let last = ''
    for (const m of visibleMessages) {
      if (m.role === 'assistant' && m.tool_calls?.length) {
        last = m.tool_calls[m.tool_calls.length - 1].id
      }
    }
    return last
  }, [visibleMessages])

  const isTerminal =
    request.status === 'done' || request.status === 'cancelled' || request.status === 'failed'

  const [userToggle, setUserToggle] = useState<Map<string, boolean>>(new Map())
  const isExpanded = (id: string) => {
    if (userToggle.has(id)) return userToggle.get(id) === true
    return id === latestToolCallID && !isTerminal
  }
  const toggle = (id: string, currentlyExpanded: boolean) => {
    setUserToggle((prev) => {
      const next = new Map(prev)
      next.set(id, !currentlyExpanded)
      return next
    })
  }

  // Show the streaming placeholder if the request is queued, OR if
  // it's running but no assistant content has arrived yet. Once the
  // first assistant message lands, the shimmer is replaced.
  const hasAssistantContent = visibleMessages.some((m) => m.role === 'assistant')
  const showShimmer = !isTerminal && !hasAssistantContent

  return (
    <article className="space-y-3">
      {request.user_input && <UserBubble text={request.user_input} />}

      {request.error_msg && request.status === 'failed' && (
        <div className="rounded-lg border border-alarm/20 bg-alarm/5 px-3 py-2 text-ui text-alarm flex items-start gap-2">
          <AlertCircle size={12} className="shrink-0 mt-0.5" />
          <span>{request.error_msg}</span>
        </div>
      )}

      {showShimmer && <StreamingShimmer />}

      {visibleMessages.map((msg) => {
        if (msg.role === 'tool') return null // rendered nested under tool_use
        if (msg.role !== 'assistant' && msg.role !== 'system') return null
        return (
          <AssistantTurn
            key={msg.id}
            message={msg}
            toolResults={toolResults}
            isExpanded={isExpanded}
            onToggle={toggle}
            linkifyCtx={linkifyCtx}
          />
        )
      })}

      {/* Soft cancellation marker */}
      {request.status === 'cancelled' && (
        <div className="text-reported text-ink-3 italic">Cancelled.</div>
      )}

      {/* Trailing pulse when running but content is already underway */}
      {request.status === 'running' && hasAssistantContent && isLatest && (
        <div className="flex items-center gap-1.5 text-reported text-ink-3">
          <span className="inline-block w-1.5 h-1.5 rounded-full bg-tint-2 animate-pulse" />
          <span>thinking…</span>
        </div>
      )}
    </article>
  )
}

function UserBubble({ text }: { text: string }) {
  return (
    <div className="flex justify-end">
      <div
        className="
          max-w-[85%] rounded-2xl rounded-tr-md
          bg-warm-2/70 border border-warm/15
          px-3.5 py-2 text-body text-ink-1
          whitespace-pre-wrap leading-relaxed
        "
      >
        {text}
      </div>
    </div>
  )
}

function AssistantTurn({
  message,
  toolResults,
  isExpanded,
  onToggle,
  linkifyCtx,
}: {
  message: Message
  toolResults: Map<string, Message>
  isExpanded: (id: string) => boolean
  onToggle: (id: string, currentlyExpanded: boolean) => void
  linkifyCtx: LinkifyContext
}) {
  const linkified = useMemo(
    () => (message.content ? linkifyMarkdown(message.content, linkifyCtx) : ''),
    [message.content, linkifyCtx],
  )

  return (
    <div className="space-y-2">
      {linkified && (
        <div
          className="
            text-body text-ink-1 leading-relaxed
            prose prose-sm max-w-none
            prose-p:my-2 prose-pre:my-2 prose-ul:my-2 prose-ol:my-2
            prose-code:bg-tint-3 prose-code:px-1 prose-code:py-0.5 prose-code:rounded
            prose-code:text-ui prose-code:font-mono prose-code:text-ink-1
            prose-pre:bg-tint-3 prose-pre:text-ink-1 prose-pre:rounded-lg
            prose-a:text-warm prose-a:no-underline hover:prose-a:underline
          "
        >
          <Markdown
            components={{
              a: ({ href, children }) => (
                <a href={href} target="_blank" rel="noopener noreferrer">
                  {children}
                </a>
              ),
            }}
          >
            {linkified}
          </Markdown>
        </div>
      )}

      {message.tool_calls?.map((tc) => {
        const result = toolResults.get(tc.id)
        const expanded = isExpanded(tc.id)
        return (
          <ToolCallCard
            key={tc.id}
            toolCall={tc}
            result={result}
            expanded={expanded}
            onToggle={() => onToggle(tc.id, expanded)}
          />
        )
      })}
    </div>
  )
}

function ToolCallCard({
  toolCall,
  result,
  expanded,
  onToggle,
}: {
  toolCall: ToolCall
  result?: Message
  expanded: boolean
  onToggle: () => void
}) {
  const label = formatToolCall(toolCall.name, toolCall.input)
  const resultPreview = result ? formatToolResult(toolCall, result) : null
  const isError = !!result?.is_error

  return (
    <div
      className={`
        rounded-lg border overflow-hidden
        ${isError ? 'border-alarm/30 bg-alarm/5' : 'border-line-1 bg-tint-2'}
      `}
    >
      <button
        type="button"
        onClick={onToggle}
        className="
          w-full flex items-center gap-2 px-3 py-1.5
          text-left text-ui
          hover:bg-tint-2 transition-colors
        "
        aria-expanded={expanded}
      >
        {expanded ? (
          <ChevronDown size={11} className="shrink-0 text-ink-3" />
        ) : (
          <ChevronRight size={11} className="shrink-0 text-ink-3" />
        )}
        <span className={`flex-1 truncate ${isError ? 'text-alarm' : 'text-ink-2'}`}>{label}</span>
        {resultPreview && !expanded && (
          <span className="text-reported text-ink-3 truncate max-w-[40%] shrink-0">
            {resultPreview}
          </span>
        )}
        {!result && (
          <span className="inline-flex items-center gap-1 text-label text-ink-3 shrink-0">
            <span className="inline-block w-1.5 h-1.5 rounded-full bg-tint-2 animate-pulse" />
            running
          </span>
        )}
      </button>
      {expanded && (
        <div className="px-3 pb-2 pt-1 space-y-2 border-t border-line-1/60">
          <ToolDetail label="Args" content={formatJSONInput(toolCall.input)} />
          {result && (
            <ToolDetail
              label={isError ? 'Error' : 'Result'}
              content={result.content || '(empty)'}
              tone={isError ? 'error' : undefined}
            />
          )}
          {result?.content_blocks
            ?.filter((b) => b.type === 'image_url' && b.image_url?.url)
            .map((b, i) => (
              <img
                key={i}
                src={b.image_url?.url}
                alt="tool result"
                className="max-h-64 max-w-full rounded-lg border border-line-1"
              />
            ))}
        </div>
      )}
    </div>
  )
}

function ToolDetail({ label, content, tone }: { label: string; content: string; tone?: 'error' }) {
  // Defensive truncation: an unbounded grep result (or a Read against
  // a 50k-line file) would otherwise blow the panel out. Users can
  // always view the full output via the agent's worktree if needed.
  const MAX = 4000
  const truncated = content.length > MAX ? content.slice(0, MAX) + '\n…' : content
  return (
    <div>
      <div className="text-label uppercase tracking-wider text-ink-3 mb-1">{label}</div>
      <pre
        className={`
          text-reported leading-snug whitespace-pre-wrap break-words font-mono
          max-h-64 overflow-auto
          ${tone === 'error' ? 'text-alarm' : 'text-ink-2'}
        `}
      >
        {truncated}
      </pre>
    </div>
  )
}

function StreamingShimmer() {
  return (
    <div className="space-y-2 animate-pulse">
      <div className="h-3 rounded-full bg-tint-3 w-3/4" />
      <div className="h-3 rounded-full bg-tint-3 w-2/3" />
      <div className="h-3 rounded-full bg-tint-3 w-1/2" />
    </div>
  )
}

function Composer({
  draft,
  onDraftChange,
  onSubmit,
  onCancel,
  inFlight,
  cancellable,
  textareaRef,
  sendError,
  totalCostUSD: _totalCostUSD,
}: {
  draft: string
  onDraftChange: (s: string) => void
  onSubmit: () => void
  onCancel: () => void
  inFlight: boolean
  cancellable: boolean
  textareaRef: React.RefObject<HTMLTextAreaElement | null>
  sendError: string | null
  totalCostUSD: number
}) {
  // Auto-resize. We measure scrollHeight on every value change and
  // clamp to 6 rows worth (~144px) so the composer doesn't eat the
  // transcript on long drafts.
  useEffect(() => {
    const el = textareaRef.current
    if (!el) return
    el.style.height = 'auto'
    el.style.height = Math.min(el.scrollHeight, 144) + 'px'
  }, [draft, textareaRef])

  // Enter submits, Shift+Enter inserts a newline. ⌘/Ctrl+Enter also
  // submits — kept as an alternate so muscle memory from chat clients
  // that use the modifier path still works.
  const onKeyDown = (e: React.KeyboardEvent<HTMLTextAreaElement>) => {
    if (e.key === 'Enter' && !e.shiftKey) {
      e.preventDefault()
      onSubmit()
    }
  }

  const buttonInactive = inFlight ? !cancellable : !draft.trim()

  return (
    <div className="relative px-4 pt-3 pb-4 border-t border-line-1/60">
      {sendError && (
        <div className="mb-2 rounded-lg border border-alarm/20 bg-alarm/5 px-2.5 py-1.5 text-reported text-alarm flex items-start gap-1.5">
          <AlertCircle size={11} className="shrink-0 mt-0.5" />
          <span>{sendError}</span>
        </div>
      )}
      <div
        className="
          flex items-center gap-2 rounded-xl border border-line-1
          bg-raised px-3 py-2
          focus-within:border-warm transition-colors
        "
      >
        <textarea
          ref={textareaRef}
          value={draft}
          onChange={(e) => onDraftChange(e.target.value)}
          onKeyDown={onKeyDown}
          placeholder={inFlight ? 'Curator is working…' : 'Message the Curator…'}
          rows={1}
          className="
            flex-1 resize-none bg-transparent text-body text-ink-1
            placeholder:text-ink-3 leading-relaxed
            focus:outline-none
          "
        />
        <button
          type="button"
          onClick={inFlight ? onCancel : onSubmit}
          disabled={buttonInactive}
          aria-label={inFlight ? 'Cancel running turn' : 'Send message'}
          className={`
            shrink-0 inline-flex items-center justify-center
            h-8 w-8 rounded-full transition-colors
            disabled:opacity-30 disabled:cursor-not-allowed
            ${
              inFlight
                ? 'bg-alarm/10 text-alarm hover:bg-alarm/20'
                : 'bg-warm text-warm-ink hover:opacity-90'
            }
          `}
        >
          {inFlight ? <Square size={12} fill="currentColor" /> : <Send size={13} />}
        </button>
      </div>
      <div className="mt-1.5 flex items-center justify-between text-label text-ink-3">
        <span>
          <kbd className="font-mono">↵</kbd> send · <kbd className="font-mono">⇧↵</kbd> newline
        </span>
      </div>
    </div>
  )
}

// --- Tool-call vocabulary (mirrors lib/runFeed's formatToolCall but keeps
// curator-side knowledge of the tools the curator actually uses) ---

function formatToolCall(name: string, input: Record<string, unknown>): string {
  if (name === 'Read') return `Reading ${basename(String(input.file_path || ''))}`
  if (name === 'Write') return `Writing ${basename(String(input.file_path || ''))}`
  if (name === 'Edit') return `Editing ${basename(String(input.file_path || ''))}`
  if (name === 'Glob') return `Searching for ${String(input.pattern || 'files')}`
  if (name === 'Grep') return `Searching for "${String(input.pattern || '').slice(0, 40)}"`
  if (name === 'Bash') {
    const cmd = String(input.command || '')
    const trimmed = cmd.length > 80 ? cmd.slice(0, 77) + '…' : cmd
    return `Running: ${trimmed}`
  }
  return name
}

function formatToolResult(_tc: ToolCall, result: Message): string {
  const text = result.content || ''
  if (!text) return result.is_error ? 'error' : '✓'
  const oneLine = text.split('\n')[0]
  return oneLine.length > 60 ? oneLine.slice(0, 57) + '…' : oneLine
}

function formatJSONInput(input: Record<string, unknown>): string {
  try {
    return JSON.stringify(input, null, 2)
  } catch {
    return String(input)
  }
}

function basename(path: string): string {
  const parts = path.split('/')
  return parts[parts.length - 1] || path
}
