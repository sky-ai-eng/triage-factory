import { useState, useEffect, useCallback, useRef } from 'react'
import { motion, useMotionValue, useTransform, AnimatePresence } from 'motion/react'
import type { PanInfo } from 'motion/react'
import * as Tooltip from '@radix-ui/react-tooltip'
import { useNavigate } from 'react-router-dom'
import type { WSEvent } from '../types'
import { useWebSocket } from '../hooks/useWebSocket'
import { SlidersHorizontal } from 'lucide-react'
import EventBadge from '../components/EventBadge'
import EventFilterPanel from '../components/EventFilterPanel'
import PromptPicker from '../components/PromptPicker'
import TaskRulesPanel from '../components/TaskRulesPanel'

interface Task {
  id: string
  source: string
  source_id: string
  source_url: string
  title: string
  description?: string
  repo?: string
  author?: string
  labels: string[]
  severity?: string
  diff_additions?: number
  diff_deletions?: number
  files_changed?: number
  ci_status?: string
  relevance_reason?: string
  event_type?: string
  scoring_status: string
  created_at: string
  status: string
  priority_score: number | null
  ai_summary?: string
  priority_reasoning?: string
  agent_confidence: number | null
}

type SwipeAction = 'claim' | 'dismiss' | 'snooze' | 'delegate'
type LoadState = 'loading' | 'empty' | 'ready'

const SWIPE_THRESHOLD = 100
const SWIPE_VELOCITY = 300

export default function Cards() {
  const [tasks, setTasks] = useState<Task[]>([])
  const [loadState, setLoadState] = useState<LoadState>('loading')
  const [cardStart, setCardStart] = useState(() => Date.now())
  const [undoTask, setUndoTask] = useState<{ id: string; action: string } | null>(null)
  const [showPromptPicker, setShowPromptPicker] = useState(false)
  const [filterOpen, setFilterOpen] = useState(false)
  const [rulesOpen, setRulesOpen] = useState(false)
  const hasFetched = useRef(false)
  const navigate = useNavigate()

  const fetchQueue = useCallback(async (preserveCurrent = false) => {
    const res = await fetch('/api/queue')
    if (res.ok) {
      const data: Task[] = await res.json()
      setTasks((prev) => {
        if (preserveCurrent && prev.length > 0) {
          // Keep the current top card in place, merge updated queue behind it
          const currentId = prev[0].id
          const updated = data.find((t) => t.id === currentId)
          const rest = data.filter((t) => t.id !== currentId)
          return [updated ?? prev[0], ...rest]
        }
        return data
      })
      if (!preserveCurrent) setCardStart(Date.now())
      hasFetched.current = true
      setLoadState(data.length === 0 ? 'empty' : 'ready')
    }
  }, [])

  // Initial queue load on mount. fetchQueue calls setState internally, which
  // the lint rule flags transitively — but fetching data on mount is the
  // canonical use of an effect and the safe pattern here.
  useEffect(() => {
    // eslint-disable-next-line react-hooks/set-state-in-effect
    fetchQueue()
  }, [fetchQueue])

  // Handle WS events for live triage pipeline updates
  useWebSocket(
    useCallback(
      (event: WSEvent) => {
        if (event.type === 'tasks_updated') {
          // New tasks arrived from poller — refetch but keep current card stable
          fetchQueue(true)
        }

        if (event.type === 'scoring_started' || event.type === 'scoring_completed') {
          // Refetch to pick up scoring_status changes
          fetchQueue(true)
        }
      },
      [fetchQueue],
    ),
  )

  const swipe = async (action: SwipeAction, promptId?: string) => {
    const task = tasks[0]
    if (!task) return

    // Delegate always requires an explicit prompt pick
    if (action === 'delegate' && !promptId) {
      setShowPromptPicker(true)
      return
    }

    const hesitationMs = Date.now() - cardStart

    try {
      const res =
        action === 'snooze'
          ? await fetch(`/api/tasks/${task.id}/snooze`, {
              method: 'POST',
              headers: { 'Content-Type': 'application/json' },
              body: JSON.stringify({ until: '2h', hesitation_ms: hesitationMs }),
            })
          : await fetch(`/api/tasks/${task.id}/swipe`, {
              method: 'POST',
              headers: { 'Content-Type': 'application/json' },
              body: JSON.stringify({
                action,
                hesitation_ms: hesitationMs,
                ...(promptId && { prompt_id: promptId }),
              }),
            })

      if (!res.ok) return
    } catch {
      return
    }

    setUndoTask({ id: task.id, action })
    setTasks((prev) => prev.slice(1))
    setCardStart(Date.now())
    setTimeout(() => setUndoTask(null), 5000)
  }

  const delegateWithPrompt = (promptId: string) => {
    setShowPromptPicker(false)
    swipe('delegate', promptId)
  }

  const undo = async () => {
    if (!undoTask) return
    try {
      const res = await fetch(`/api/tasks/${undoTask.id}/undo`, { method: 'POST' })
      if (!res.ok) return
    } catch {
      return
    }
    setUndoTask(null)
    fetchQueue()
  }

  useEffect(() => {
    const handler = (e: KeyboardEvent) => {
      if (e.key === 'ArrowLeft') swipe('dismiss')
      else if (e.key === 'ArrowRight') swipe('claim')
      else if (e.key === 'ArrowUp') swipe('delegate')
      else if (e.key === 'ArrowDown') swipe('snooze')
      else if ((e.ctrlKey || e.metaKey) && e.key === 'z') undo()
    }
    window.addEventListener('keydown', handler)
    return () => window.removeEventListener('keydown', handler)
  })

  // Loading state — waiting for first poll
  if (loadState === 'loading' && tasks.length === 0) {
    return (
      <div className="flex flex-col items-center justify-center min-h-[70vh] gap-4">
        <div className="w-16 h-16 rounded-full bg-accent-soft flex items-center justify-center">
          <motion.span
            className="text-accent text-2xl"
            animate={{ opacity: [0.3, 1, 0.3] }}
            transition={{ duration: 2, repeat: Infinity, ease: 'easeInOut' }}
          >
            ~
          </motion.span>
        </div>
        <p className="text-text-secondary text-sm">Polling for tasks...</p>
      </div>
    )
  }

  // Empty state — polled but nothing in queue
  if (tasks.length === 0) {
    return (
      <div className="flex flex-col items-center justify-center min-h-[70vh] gap-4">
        <div className="w-16 h-16 rounded-full bg-accent-soft flex items-center justify-center">
          <span className="text-accent text-2xl">~</span>
        </div>
        <p className="text-text-secondary text-sm">All clear. Nothing to triage.</p>
        <div className="flex items-center gap-2 relative">
          <EventFilterPanel
            open={filterOpen}
            onToggle={() => setFilterOpen((o) => !o)}
            onChange={() => fetchQueue()}
          />
          <button
            onClick={() => setRulesOpen((o) => !o)}
            className={`flex items-center gap-1.5 text-[12px] font-medium px-3 py-1.5 rounded-full border transition-colors ${
              rulesOpen
                ? 'bg-accent/10 text-accent border-accent/20'
                : 'text-text-tertiary border-border-subtle hover:text-text-secondary'
            }`}
            title="Task rules"
          >
            <SlidersHorizontal size={14} />
            <span>Rules</span>
          </button>
        </div>
      </div>
    )
  }

  return (
    <Tooltip.Provider delayDuration={300}>
      <div className="flex flex-col items-center justify-center min-h-[70vh] gap-8">
        {/* Counter + filter toggle */}
        <div className="flex items-center gap-3 relative">
          <p className="text-[13px] text-text-tertiary font-medium tracking-wide">
            {tasks.length} item{tasks.length !== 1 ? 's' : ''} in queue
          </p>
          <EventFilterPanel
            open={filterOpen}
            onToggle={() => setFilterOpen((o) => !o)}
            onChange={() => fetchQueue()}
          />
          <button
            onClick={() => setRulesOpen((o) => !o)}
            className={`flex items-center gap-1.5 text-[12px] font-medium px-3 py-1.5 rounded-full border transition-colors ${
              rulesOpen
                ? 'bg-accent/10 text-accent border-accent/20'
                : 'text-text-tertiary border-border-subtle hover:text-text-secondary'
            }`}
            title="Task rules"
          >
            <SlidersHorizontal size={14} />
            <span>Rules</span>
          </button>
        </div>

        {/* Card stack */}
        <div className="relative w-full max-w-[400px] h-[380px]">
          {/* Second card (behind) */}
          {tasks.length > 1 && (
            <SwipeCard
              key={tasks[1].id}
              task={tasks[1]}
              isScoring={tasks[1].scoring_status === 'scoring'}
              style={{
                zIndex: 1,
                transform: 'scale(0.96) translateY(10px)',
                pointerEvents: 'none',
                filter: 'brightness(0.97)',
              }}
              interactive={false}
            />
          )}

          {/* Top card (interactive) */}
          <SwipeCard
            key={tasks[0].id}
            task={tasks[0]}
            isScoring={tasks[0].scoring_status === 'scoring'}
            onSwipe={swipe}
            style={{ zIndex: 2 }}
            interactive
          />
        </div>

        {/* Action buttons */}
        <div className="flex gap-3">
          <ActionButton
            onClick={() => swipe('dismiss')}
            color="dismiss"
            label="Dismiss"
            shortcut="←"
          />
          <ActionButton
            onClick={() => swipe('snooze')}
            color="snooze"
            label="Snooze"
            shortcut="↓"
          />
          <ActionButton
            onClick={() => swipe('delegate')}
            color="delegate"
            label="Delegate"
            shortcut="↑"
          />
          <ActionButton onClick={() => swipe('claim')} color="claim" label="Claim" shortcut="→" />
        </div>

        {/* Undo toast */}
        <AnimatePresence>
          {undoTask && (
            <motion.div
              initial={{ opacity: 0, y: 20 }}
              animate={{ opacity: 1, y: 0 }}
              exit={{ opacity: 0, y: 20 }}
              className="fixed bottom-8 left-1/2 -translate-x-1/2 backdrop-blur-xl bg-white/70 border border-border-glass rounded-full px-5 py-2.5 flex items-center gap-3 shadow-lg shadow-black/5"
            >
              <span className="text-sm text-text-secondary">
                {undoTask.action === 'dismiss'
                  ? 'Dismissed'
                  : undoTask.action === 'claim'
                    ? 'Claimed'
                    : undoTask.action === 'delegate'
                      ? 'Delegated'
                      : 'Snoozed'}
              </span>
              <button
                onClick={undo}
                className="text-sm text-accent hover:text-accent/80 font-medium"
              >
                Undo
              </button>
            </motion.div>
          )}
        </AnimatePresence>
      </div>

      <PromptPicker
        open={showPromptPicker}
        onSelect={delegateWithPrompt}
        onClose={() => setShowPromptPicker(false)}
        onEditPrompts={() => {
          setShowPromptPicker(false)
          navigate('/prompts')
        }}
      />

      <TaskRulesPanel open={rulesOpen} onClose={() => setRulesOpen(false)} />
    </Tooltip.Provider>
  )
}

function ActionButton({
  onClick,
  color,
  label,
  shortcut,
}: {
  onClick: () => void
  color: 'dismiss' | 'claim' | 'delegate' | 'snooze'
  label: string
  shortcut: string
}) {
  const colorMap = {
    dismiss: 'hover:bg-dismiss/10 hover:text-dismiss hover:border-dismiss/20',
    claim: 'hover:bg-claim/10 hover:text-claim hover:border-claim/20',
    delegate: 'hover:bg-delegate/10 hover:text-delegate hover:border-delegate/20',
    snooze: 'hover:bg-snooze/10 hover:text-snooze hover:border-snooze/20',
  }

  return (
    <button
      onClick={onClick}
      className={`text-[13px] text-text-tertiary border border-border-subtle rounded-full px-5 py-2 transition-all duration-200 ${colorMap[color]}`}
    >
      <span className="opacity-50 mr-1">{shortcut}</span> {label}
    </button>
  )
}

function SwipeCard({
  task,
  onSwipe,
  style,
  interactive = true,
  isScoring = false,
}: {
  task: Task
  onSwipe?: (action: SwipeAction) => void
  style?: React.CSSProperties
  interactive?: boolean
  isScoring?: boolean
}) {
  const x = useMotionValue(0)
  const y = useMotionValue(0)
  const rotate = useTransform(x, [-200, 200], [-8, 8])
  const opacity = useTransform(x, [-200, -150, 0, 150, 200], [0.6, 1, 1, 1, 0.6])

  // Directional tints
  const dismissBg = useTransform(x, [-150, 0], ['rgba(196,90,90,0.08)', 'rgba(196,90,90,0)'])
  const claimBg = useTransform(x, [0, 150], ['rgba(90,140,106,0)', 'rgba(90,140,106,0.08)'])
  const delegateBg = useTransform(y, [-150, 0], ['rgba(122,106,173,0.08)', 'rgba(122,106,173,0)'])

  // Direction labels
  const leftOpacity = useTransform(x, [-120, -40], [1, 0])
  const rightOpacity = useTransform(x, [40, 120], [0, 1])
  const upOpacity = useTransform(y, [-120, -40], [1, 0])

  const handleDragEnd = (_: unknown, info: PanInfo) => {
    if (!onSwipe) return
    const { offset, velocity } = info
    if (offset.x < -SWIPE_THRESHOLD || velocity.x < -SWIPE_VELOCITY) {
      onSwipe('dismiss')
    } else if (offset.x > SWIPE_THRESHOLD || velocity.x > SWIPE_VELOCITY) {
      onSwipe('claim')
    } else if (offset.y < -SWIPE_THRESHOLD || velocity.y < -SWIPE_VELOCITY) {
      onSwipe('delegate')
    } else if (offset.y > SWIPE_THRESHOLD || velocity.y > SWIPE_VELOCITY) {
      onSwipe('snooze')
    }
  }

  const age = formatAge(task.created_at)

  return (
    <motion.div
      className={`absolute inset-0 rounded-3xl select-none overflow-hidden ${interactive ? 'cursor-grab active:cursor-grabbing' : ''}`}
      style={interactive ? { x, y, rotate, opacity, ...style } : style}
      drag={interactive}
      dragConstraints={interactive ? { left: 0, right: 0, top: 0, bottom: 0 } : undefined}
      dragElastic={0.7}
      onDragEnd={interactive ? handleDragEnd : undefined}
    >
      {/* Glass card */}
      <motion.div
        className="absolute inset-0 rounded-3xl backdrop-blur-2xl bg-white/60 border border-white/80 shadow-xl shadow-black/[0.04]"
        style={{
          background: useTransform(
            x,
            [-150, 0, 150],
            ['rgba(255,255,255,0.55)', 'rgba(255,255,255,0.6)', 'rgba(255,255,255,0.55)'],
          ),
        }}
      />

      {/* Directional tint overlays */}
      <motion.div className="absolute inset-0 rounded-3xl" style={{ background: dismissBg }} />
      <motion.div className="absolute inset-0 rounded-3xl" style={{ background: claimBg }} />
      <motion.div className="absolute inset-0 rounded-3xl" style={{ background: delegateBg }} />

      {/* Direction labels — only on the interactive top card */}
      {interactive && (
        <>
          <motion.div
            style={{ opacity: leftOpacity }}
            className="absolute top-6 right-6 text-dismiss font-semibold text-xs tracking-wide uppercase border border-dismiss/30 rounded-full px-3 py-1"
          >
            Dismiss
          </motion.div>
          <motion.div
            style={{ opacity: rightOpacity }}
            className="absolute top-6 right-6 text-claim font-semibold text-xs tracking-wide uppercase border border-claim/30 rounded-full px-3 py-1"
          >
            Claim
          </motion.div>
          <motion.div
            style={{ opacity: upOpacity }}
            className="absolute top-6 right-6 text-delegate font-semibold text-xs tracking-wide uppercase border border-delegate/30 rounded-full px-3 py-1"
          >
            Delegate
          </motion.div>
        </>
      )}

      {/* Content */}
      <div className="relative h-full p-7 flex flex-col overflow-hidden">
        {/* Source badge row */}
        <div className="flex items-center gap-2.5 mb-4 shrink-0">
          <span
            className={`text-[11px] font-semibold uppercase tracking-wider px-2.5 py-1 rounded-full ${
              task.source === 'github'
                ? 'bg-black/[0.05] text-text-secondary'
                : 'bg-blue-500/10 text-blue-600'
            }`}
          >
            {task.source === 'github' ? 'GitHub' : 'Jira'}
          </span>
          <EventBadge eventType={task.event_type} />
          {task.repo && <span className="text-[12px] text-text-tertiary">{task.repo}</span>}
          {task.severity && (
            <span className="text-[11px] font-medium text-accent bg-accent-soft px-2 py-0.5 rounded-full">
              {task.severity}
            </span>
          )}
        </div>

        {/* Title */}
        <h2 className="text-[17px] leading-snug font-semibold text-text-primary mb-2 line-clamp-2 shrink-0">
          {task.title}
        </h2>

        {/* AI Summary — shimmer when scoring, content when scored */}
        {task.ai_summary ? (
          <div className="flex items-start gap-2 mb-3 shrink-0">
            <svg
              className="w-3.5 h-3.5 mt-0.5 shrink-0 text-text-tertiary opacity-50"
              viewBox="0 0 16 16"
              fill="none"
            >
              <path
                d="M8 1.5a6.5 6.5 0 100 13 6.5 6.5 0 000-13zM7 5h2v5H7V5zm0 6h2v2H7v-2z"
                fill="currentColor"
                fillRule="evenodd"
              />
            </svg>
            <p className="text-[13px] text-text-secondary leading-relaxed">{task.ai_summary}</p>
          </div>
        ) : isScoring ? (
          <div className="mb-3 shrink-0 space-y-2">
            <ScoringShimmer />
          </div>
        ) : null}

        {/* Priority reasoning — shimmer when scoring, content when scored */}
        {task.priority_reasoning && task.priority_score != null ? (
          <div className="flex items-start gap-2 mb-3 shrink-0">
            <PriorityGauge value={task.priority_score} />
            <p className="text-[13px] text-text-tertiary leading-relaxed">
              {task.priority_reasoning}
            </p>
          </div>
        ) : isScoring ? (
          <div className="flex items-start gap-2 mb-3 shrink-0">
            <div className="w-[18px] h-[12px] mt-0.5 shrink-0 rounded bg-black/[0.04] animate-pulse" />
            <div className="flex-1 h-4 rounded bg-black/[0.04] animate-pulse" />
          </div>
        ) : null}

        {/* Labels */}
        {task.labels.length > 0 && (
          <div className="flex flex-wrap gap-1.5 mb-3 shrink-0">
            {task.labels.slice(0, 4).map((label) => (
              <span
                key={label}
                className="text-[11px] text-text-tertiary bg-black/[0.04] px-2.5 py-0.5 rounded-full"
              >
                {label}
              </span>
            ))}
          </div>
        )}

        {/* Spacer */}
        <div className="flex-1" />

        {/* Metadata footer */}
        <div className="flex items-end justify-between shrink-0">
          <div className="flex flex-col gap-0.5 text-[12px] text-text-tertiary">
            <div className="flex items-center gap-1.5">
              {task.author && <span>{task.author}</span>}
              {task.author && <span className="opacity-30">·</span>}
              <span>{age}</span>
            </div>
            <div className="flex items-center gap-1.5">
              {task.diff_additions || task.diff_deletions ? (
                <>
                  <span className="inline-flex gap-1">
                    {task.diff_additions ? (
                      <span className="text-claim font-medium">
                        +{compactNum(task.diff_additions)}
                      </span>
                    ) : null}
                    {task.diff_deletions ? (
                      <span className="text-dismiss font-medium">
                        -{compactNum(task.diff_deletions)}
                      </span>
                    ) : null}
                  </span>
                  {task.files_changed != null && task.files_changed > 0 && (
                    <>
                      <span className="opacity-30">·</span>
                      <span>{task.files_changed} files</span>
                    </>
                  )}
                </>
              ) : task.files_changed != null && task.files_changed > 0 ? (
                <span>{task.files_changed} files</span>
              ) : null}
            </div>
          </div>

          <div className="flex items-center gap-3">
            {task.agent_confidence != null ? (
              <ConfidenceGauge value={task.agent_confidence} />
            ) : isScoring ? (
              <div className="w-7 h-[18px] rounded bg-black/[0.04] animate-pulse" />
            ) : null}
            <a
              href={task.source_url}
              target="_blank"
              rel="noopener noreferrer"
              className="text-[12px] text-accent hover:text-accent/70 font-medium transition-colors"
              onClick={(e) => e.stopPropagation()}
            >
              Open
            </a>
          </div>
        </div>
      </div>
    </motion.div>
  )
}

function PriorityGauge({ value }: { value: number }) {
  // 0.0 = low priority (cool), 1.0 = urgent (hot)
  const angle = -90 + value * 180
  const needleColor =
    value >= 0.7
      ? 'var(--color-dismiss)'
      : value >= 0.4
        ? 'var(--color-snooze)'
        : 'var(--color-claim)'

  return (
    <svg width="18" height="12" viewBox="0 0 28 18" fill="none" className="shrink-0 mt-0.5">
      <path
        d="M 4 16 A 10 10 0 0 1 24 16"
        stroke="currentColor"
        strokeWidth="2"
        strokeLinecap="round"
        className="text-black/[0.08]"
      />
      <path
        d="M 4 16 A 10 10 0 0 1 24 16"
        stroke={needleColor}
        strokeWidth="2"
        strokeLinecap="round"
        strokeDasharray={`${value * 31.4} 31.4`}
        opacity="0.5"
      />
      <line
        x1="14"
        y1="16"
        x2={14 + 8 * Math.cos(((angle - 90) * Math.PI) / 180)}
        y2={16 + 8 * Math.sin(((angle - 90) * Math.PI) / 180)}
        stroke={needleColor}
        strokeWidth="1.5"
        strokeLinecap="round"
      />
      <circle cx="14" cy="16" r="1.5" fill={needleColor} />
    </svg>
  )
}

function ConfidenceGauge({ value }: { value: number }) {
  // Needle angle: 0.0 (human, left) to 1.0 (AI, right) maps to -90deg to +90deg
  const angle = -90 + value * 180
  const pct = Math.round(value * 100)
  const label =
    value >= 0.7
      ? 'Highly automatable'
      : value >= 0.4
        ? 'Partially automatable'
        : 'Needs human attention'
  const needleColor =
    value >= 0.7
      ? 'var(--color-delegate)'
      : value >= 0.4
        ? 'var(--color-snooze)'
        : 'var(--color-dismiss)'

  return (
    <Tooltip.Root>
      <Tooltip.Trigger asChild>
        <span
          className="inline-flex items-center cursor-default"
          onClick={(e) => e.stopPropagation()}
        >
          <svg width="28" height="18" viewBox="0 0 28 18" fill="none">
            {/* Arc track */}
            <path
              d="M 4 16 A 10 10 0 0 1 24 16"
              stroke="currentColor"
              strokeWidth="2"
              strokeLinecap="round"
              className="text-black/[0.08]"
            />
            {/* Colored arc fill */}
            <path
              d="M 4 16 A 10 10 0 0 1 24 16"
              stroke={needleColor}
              strokeWidth="2"
              strokeLinecap="round"
              strokeDasharray={`${value * 31.4} 31.4`}
              opacity="0.5"
            />
            {/* Needle */}
            <line
              x1="14"
              y1="16"
              x2={14 + 8 * Math.cos(((angle - 90) * Math.PI) / 180)}
              y2={16 + 8 * Math.sin(((angle - 90) * Math.PI) / 180)}
              stroke={needleColor}
              strokeWidth="1.5"
              strokeLinecap="round"
            />
            {/* Center dot */}
            <circle cx="14" cy="16" r="1.5" fill={needleColor} />
          </svg>
        </span>
      </Tooltip.Trigger>
      <Tooltip.Portal>
        <Tooltip.Content
          side="top"
          sideOffset={6}
          className="z-50 rounded-lg backdrop-blur-xl bg-white/80 border border-border-glass px-3 py-2 shadow-lg shadow-black/5 text-[12px] max-w-[200px]"
        >
          <div className="font-medium text-text-primary mb-0.5">{label}</div>
          <div className="text-text-tertiary">
            {pct}% AI confidence —{' '}
            {value >= 0.7
              ? 'good candidate for delegation'
              : value >= 0.4
                ? 'may need some human guidance'
                : 'best handled by a human'}
          </div>
          <Tooltip.Arrow className="fill-white/80" />
        </Tooltip.Content>
      </Tooltip.Portal>
    </Tooltip.Root>
  )
}

function ScoringShimmer() {
  return (
    <div className="flex items-start gap-2">
      <motion.div
        className="w-3.5 h-3.5 mt-0.5 shrink-0 rounded-full bg-accent/20"
        animate={{ opacity: [0.3, 0.7, 0.3] }}
        transition={{ duration: 1.5, repeat: Infinity, ease: 'easeInOut' }}
      />
      <div className="flex-1 space-y-1.5">
        <motion.div
          className="h-3.5 rounded bg-black/[0.04]"
          animate={{ opacity: [0.4, 0.7, 0.4] }}
          transition={{ duration: 1.5, repeat: Infinity, ease: 'easeInOut' }}
          style={{ width: '85%' }}
        />
        <motion.div
          className="h-3.5 rounded bg-black/[0.04]"
          animate={{ opacity: [0.4, 0.7, 0.4] }}
          transition={{ duration: 1.5, repeat: Infinity, ease: 'easeInOut', delay: 0.2 }}
          style={{ width: '60%' }}
        />
      </div>
    </div>
  )
}

function compactNum(n: number): string {
  if (n >= 1000) return (n / 1000).toFixed(1).replace(/\.0$/, '') + 'k'
  return String(n)
}

function formatAge(dateStr: string): string {
  const diff = Date.now() - new Date(dateStr).getTime()
  const hours = Math.floor(diff / (1000 * 60 * 60))
  if (hours < 1) return 'just now'
  if (hours < 24) return `${hours}h ago`
  const days = Math.floor(hours / 24)
  if (days === 1) return '1d ago'
  return `${days}d ago`
}
