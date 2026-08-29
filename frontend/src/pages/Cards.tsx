import { useState, useEffect, useCallback, useRef } from 'react'
import { motion, useMotionValue, useTransform, AnimatePresence } from 'motion/react'
import type { PanInfo } from 'motion/react'
import { Tooltip } from '../ui/tooltip/Tooltip'
import { useNavigate } from 'react-router'
import type { Task, WSEvent } from '../types'
import { useWebSocket } from '../hooks/useWebSocket'
import { useOrgHref } from '../hooks/useOrgHref'
import { useTeamFilter } from '../hooks/useTeams'
import { usePagedList } from '../hooks/usePagedList'
import { TASK_LIST_PATH, queueListBody } from '../lib/taskList'
import { SlidersHorizontal } from 'lucide-react'
import EventBadge from '../components/EventBadge'
import SourceBadge from '../components/SourceBadge'
import RequestedReviewerBadge from '../components/RequestedReviewerBadge'
import PromptPicker from '../components/PromptPicker'
import TaskRulesPanel from '../components/TaskRulesPanel'
import TeamScopeSelect from '../components/TeamScopeSelect'
import { apiFetch } from '../lib/apiClient'
import { snoozeUntilFromPreset } from '../lib/snooze'

type SwipeAction = 'claim' | 'dismiss' | 'snooze' | 'delegate'
type LoadState = 'loading' | 'empty' | 'ready'

const SWIPE_THRESHOLD = 100
const SWIPE_VELOCITY = 300
// How few cards may be left before the deck fetches its next page.
const DECK_TOP_UP_AT = 25
// The deck's swipe-down gesture carries no picker with it, so it snoozes for
// a fixed spell. Named here rather than inlined so the one place the deck
// decides "how long" is legible.
const DECK_SNOOZE_PRESET = '2h'

export default function Cards() {
  // The deck is one page of the queue projection, threaded through the shared
  // list hook. A swipe removes the top card locally and the next fetch
  // reconciles; swiped cards leave the projection server-side, so a refetch
  // always returns the next slice of pickable work rather than the same rows.
  const deck = usePagedList<Task>(TASK_LIST_PATH, 'Could not load the queue.')
  const { load: loadDeck, loadMore: loadMoreDeck, setItems: setDeckItems } = deck
  const tasks = deck.items
  const [loadState, setLoadState] = useState<LoadState>('loading')
  const [cardStart, setCardStart] = useState(() => Date.now())
  const [undoTask, setUndoTask] = useState<{ id: string; action: string } | null>(null)
  const [showPromptPicker, setShowPromptPicker] = useState(false)
  const [rulesOpen, setRulesOpen] = useState(false)
  const hasFetched = useRef(false)
  const navigate = useNavigate()
  const orgHref = useOrgHref()
  // Per-page team read filter. '' = all my teams; a team id
  // narrows the queue. teamFilterRef keeps fetchQueue's identity stable
  // (it's a WS-callback dep) while always reading the latest value.
  const [teamFilter, setTeamFilter] = useTeamFilter('cards')
  const teamFilterRef = useRef(teamFilter)
  useEffect(() => {
    teamFilterRef.current = teamFilter
  }, [teamFilter])
  // The deck as of the last render, for the handlers that need to read it
  // without being re-created on every card (fetchQueue's pin-the-top-card
  // merge, the swipe's top-up check).
  const tasksRef = useRef<Task[]>(tasks)
  useEffect(() => {
    tasksRef.current = tasks
  }, [tasks])

  const fetchQueue = useCallback(
    async (preserveCurrent = false) => {
      const current = preserveCurrent ? tasksRef.current[0] : undefined
      const page = await loadDeck(queueListBody(teamFilterRef.current))
      // Leave the deck as it is on a failed poll: the next fetchQueue (a
      // swipe, an undo, a WS nudge) reconciles, and blanking the cards would
      // be worse than showing a slightly stale queue.
      if (!page) return
      if (current) {
        // Keep the current top card in place, merge the updated queue behind
        // it — mid-gesture the card under the user's finger must not move,
        // and it stays even if the refetch no longer lists it.
        setDeckItems((items) => [
          items.find((t) => t.id === current.id) ?? current,
          ...items.filter((t) => t.id !== current.id),
        ])
      } else {
        setCardStart(Date.now())
      }
      hasFetched.current = true
      setLoadState(page.items.length === 0 ? 'empty' : 'ready')
    },
    [loadDeck, setDeckItems],
  )

  // Initial queue load on mount. fetchQueue calls setState internally, which
  // the lint rule flags transitively — but fetching data on mount is the
  // canonical use of an effect and the safe pattern here.
  useEffect(() => {
    // eslint-disable-next-line react-hooks/set-state-in-effect
    fetchQueue()
  }, [fetchQueue])

  // Re-fetch when the team filter changes. fetchQueue reads the latest
  // filter via its ref, so the call here picks up the new scope.
  useEffect(() => {
    if (!hasFetched.current) return
    // eslint-disable-next-line react-hooks/set-state-in-effect
    fetchQueue()
  }, [teamFilter, fetchQueue])

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
    // Each gesture addresses its own route: the two lifecycle ones are field
    // writes on the task, and claim/delegate are verbs because of what they
    // reach — Jira, the spawner, artifact teardown.
    const request: [string, RequestInit] =
      action === 'snooze'
        ? [
            `/api/tasks/${task.id}`,
            {
              method: 'PATCH',
              body: JSON.stringify({
                snooze_until: snoozeUntilFromPreset(DECK_SNOOZE_PRESET),
                hesitation_ms: hesitationMs,
              }),
            },
          ]
        : action === 'dismiss'
          ? [
              `/api/tasks/${task.id}`,
              {
                method: 'PATCH',
                body: JSON.stringify({ status: 'dismissed', hesitation_ms: hesitationMs }),
              },
            ]
          : action === 'claim'
            ? [
                `/api/tasks/${task.id}/claim`,
                { method: 'POST', body: JSON.stringify({ hesitation_ms: hesitationMs }) },
              ]
            : [
                `/api/tasks/${task.id}/delegate`,
                {
                  method: 'POST',
                  body: JSON.stringify({
                    hesitation_ms: hesitationMs,
                    ...(promptId && { blueprint_id: promptId }),
                  }),
                },
              ]

    try {
      const [path, init] = request
      await apiFetch(path, { ...init, headers: { 'Content-Type': 'application/json' } })
    } catch {
      return
    }

    setUndoTask({ id: task.id, action })
    setDeckItems((prev) => prev.slice(1))
    setCardStart(Date.now())
    setTimeout(() => setUndoTask(null), 5000)
    // Top the deck up before it runs dry. A page holds the first 200 pickable
    // tasks; without this, a triage run long enough to swipe through them
    // would show an empty deck with work still queued behind it.
    if (deck.hasMore && tasksRef.current.length <= DECK_TOP_UP_AT) loadMoreDeck()
  }

  const delegateWithPrompt = (promptId: string) => {
    setShowPromptPicker(false)
    swipe('delegate', promptId)
  }

  const undo = async () => {
    if (!undoTask) return
    try {
      await apiFetch(`/api/tasks/${undoTask.id}/undo`, { method: 'POST' })
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
        <div className="w-16 h-16 rounded-full bg-warm-2 flex items-center justify-center">
          <motion.span
            className="text-warm text-2xl"
            animate={{ opacity: [0.3, 1, 0.3] }}
            transition={{ duration: 2, repeat: Infinity, ease: 'easeInOut' }}
          >
            ~
          </motion.span>
        </div>
        <p className="text-ink-2 text-sm">Polling for tasks...</p>
      </div>
    )
  }

  // Empty state — polled but nothing in queue
  if (tasks.length === 0) {
    return (
      <div className="flex flex-col items-center justify-center min-h-[70vh] gap-4">
        <div className="w-16 h-16 rounded-full bg-warm-2 flex items-center justify-center">
          <span className="text-warm text-2xl">~</span>
        </div>
        <p className="text-ink-2 text-sm">All clear. Nothing to triage.</p>
        <div className="flex items-center gap-2 relative">
          <TeamScopeSelect value={teamFilter} onChange={setTeamFilter} />
          <button
            onClick={() => setRulesOpen((o) => !o)}
            className={`flex items-center gap-1.5 text-ui font-medium px-3 py-1.5 rounded-full border transition-colors ${
              rulesOpen
                ? 'bg-warm/10 text-warm border-warm/20'
                : 'text-ink-3 border-line-1 hover:text-ink-2'
            }`}
            title="Task rules"
          >
            <SlidersHorizontal size={14} />
            <span>Rules</span>
          </button>
        </div>
        <TaskRulesPanel open={rulesOpen} onClose={() => setRulesOpen(false)} />
      </div>
    )
  }

  return (
    <>
    <div className="flex flex-col items-center justify-center min-h-[70vh] gap-8">
      {/* Counter + filter toggle */}
      <div className="flex items-center gap-3 relative">
        <p className="text-body text-ink-3 font-medium tracking-wide">
          {tasks.length} item{tasks.length !== 1 ? 's' : ''} in queue
        </p>
        <TeamScopeSelect value={teamFilter} onChange={setTeamFilter} />
        <button
          onClick={() => setRulesOpen((o) => !o)}
          className={`flex items-center gap-1.5 text-ui font-medium px-3 py-1.5 rounded-full border transition-colors ${
            rulesOpen
              ? 'bg-warm/10 text-warm border-warm/20'
              : 'text-ink-3 border-line-1 hover:text-ink-2'
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
            isScoring={tasks[1].scoring_status !== 'scored'}
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
          isScoring={tasks[0].scoring_status !== 'scored'}
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
            className="fixed bottom-8 left-1/2 -translate-x-1/2 backdrop-blur-xl bg-raised border border-line-1 rounded-full px-5 py-2.5 flex items-center gap-3 shadow-float shadow-black/5"
          >
            <span className="text-sm text-ink-2">
              {undoTask.action === 'dismiss'
                ? 'Dismissed'
                : undoTask.action === 'claim'
                  ? 'Claimed'
                  : undoTask.action === 'delegate'
                    ? 'Delegated'
                    : 'Snoozed'}
            </span>
            <button onClick={undo} className="text-sm text-warm hover:text-warm/80 font-medium">
              Undo
            </button>
          </motion.div>
        )}
      </AnimatePresence>
    </div>

    <PromptPicker
      open={showPromptPicker}
      source="blueprints"
      title="Choose a blueprint"
      subtitle="Select a blueprint to run for this task"
      selectLabel="Run"
      onSelect={delegateWithPrompt}
      onClose={() => setShowPromptPicker(false)}
      onEditPrompts={() => {
        setShowPromptPicker(false)
        navigate(orgHref('/prompts'))
      }}
    />

    <TaskRulesPanel open={rulesOpen} onClose={() => setRulesOpen(false)} />
    </>
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
    dismiss: 'hover:bg-alarm/10 hover:text-alarm hover:border-alarm/20',
    claim: 'hover:bg-tint-2 hover:text-ink-2 hover:border-line-1',
    delegate: 'hover:bg-tint-2 hover:text-ink-2 hover:border-line-1',
    snooze: 'hover:bg-tint-2 hover:text-ink-2 hover:border-line-1',
  }

  return (
    <button
      onClick={onClick}
      className={`text-body text-ink-3 border border-line-1 rounded-full px-5 py-2 transition-all duration-200 ${colorMap[color]}`}
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
      {/* Card surface — flat fill, themed via CSS variables. */}
      <div className="absolute inset-0 rounded-3xl bg-raised border border-line-1 shadow-float shadow-black/[0.04]" />

      {/* Directional tint overlays */}
      <motion.div className="absolute inset-0 rounded-3xl" style={{ background: dismissBg }} />
      <motion.div className="absolute inset-0 rounded-3xl" style={{ background: claimBg }} />
      <motion.div className="absolute inset-0 rounded-3xl" style={{ background: delegateBg }} />

      {/* Direction labels — only on the interactive top card */}
      {interactive && (
        <>
          <motion.div
            style={{ opacity: leftOpacity }}
            className="absolute top-6 right-6 text-alarm font-semibold text-xs tracking-wide uppercase border border-alarm/30 rounded-full px-3 py-1"
          >
            Dismiss
          </motion.div>
          <motion.div
            style={{ opacity: rightOpacity }}
            className="absolute top-6 right-6 text-ink-2 font-semibold text-xs tracking-wide uppercase border border-line-1 rounded-full px-3 py-1"
          >
            Claim
          </motion.div>
          <motion.div
            style={{ opacity: upOpacity }}
            className="absolute top-6 right-6 text-ink-2 font-semibold text-xs tracking-wide uppercase border border-line-1 rounded-full px-3 py-1"
          >
            Delegate
          </motion.div>
        </>
      )}

      {/* Content */}
      <div className="relative h-full p-7 flex flex-col overflow-hidden">
        {/* Source badge row */}
        <div className="flex items-center gap-2.5 mb-4 shrink-0">
          <SourceBadge task={task} size="lg" />
          <EventBadge eventType={task.event_type} />
          <RequestedReviewerBadge task={task} />
          {task.severity && (
            <span className="text-reported font-medium text-warm bg-warm-2 px-2 py-0.5 rounded-full">
              {task.severity}
            </span>
          )}
        </div>

        {/* Title */}
        <h2 className="text-section leading-snug font-semibold text-ink-1 mb-2 line-clamp-2 shrink-0">
          {task.title}
        </h2>

        {/* AI Summary — shimmer when scoring, content when scored */}
        {task.ai_summary ? (
          <div className="flex items-start gap-2 mb-3 shrink-0">
            <svg
              className="w-3.5 h-3.5 mt-0.5 shrink-0 text-ink-3 opacity-50"
              viewBox="0 0 16 16"
              fill="none"
            >
              <path
                d="M8 1.5a6.5 6.5 0 100 13 6.5 6.5 0 000-13zM7 5h2v5H7V5zm0 6h2v2H7v-2z"
                fill="currentColor"
                fillRule="evenodd"
              />
            </svg>
            <p className="text-body text-ink-2 leading-relaxed">{task.ai_summary}</p>
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
            <p className="text-body text-ink-3 leading-relaxed">{task.priority_reasoning}</p>
          </div>
        ) : isScoring ? (
          <div className="flex items-start gap-2 mb-3 shrink-0">
            <div className="w-[18px] h-[12px] mt-0.5 shrink-0 rounded bg-tint-3 animate-pulse" />
            <div className="flex-1 h-4 rounded bg-tint-3 animate-pulse" />
          </div>
        ) : null}

        {/* Spacer */}
        <div className="flex-1" />

        {/* Metadata footer */}
        <div className="flex items-end justify-between shrink-0">
          <div className="text-ui text-ink-3">
            <span>{age}</span>
          </div>

          <div className="flex items-center gap-3">
            {task.autonomy_suitability != null ? (
              <ConfidenceGauge value={task.autonomy_suitability} />
            ) : isScoring ? (
              <div className="w-7 h-[18px] rounded bg-tint-3 animate-pulse" />
            ) : null}
            {task.source_url && (
              <a
                href={task.source_url}
                target="_blank"
                rel="noopener noreferrer"
                className="text-ui text-warm hover:text-warm/70 font-medium transition-colors"
                onClick={(e) => e.stopPropagation()}
                onPointerDown={(e) => e.stopPropagation()}
              >
                Open
              </a>
            )}
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
    value >= 0.7 ? 'var(--color-alarm)' : value >= 0.4 ? 'var(--color-warm)' : 'var(--color-ink-3)'

  return (
    <svg width="18" height="12" viewBox="0 0 28 18" fill="none" className="shrink-0 mt-0.5">
      <path
        d="M 4 16 A 10 10 0 0 1 24 16"
        stroke="currentColor"
        strokeWidth="2"
        strokeLinecap="round"
        className="text-ink-4"
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
    value >= 0.7 ? 'var(--color-cool)' : value >= 0.4 ? 'var(--color-warm)' : 'var(--color-alarm)'

  return (
    <Tooltip
      wrap
      content={
        <>
          <div className="font-medium text-ink-1 mb-0.5">{label}</div>
          <div className="text-ink-3">
            {pct}% AI confidence —{' '}
            {value >= 0.7
              ? 'good candidate for delegation'
              : value >= 0.4
                ? 'may need some human guidance'
                : 'best handled by a human'}
          </div>
        </>
      }
    >
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
              className="text-ink-4"
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
    </Tooltip>
  )
}

function ScoringShimmer() {
  return (
    <div className="flex items-start gap-2">
      <motion.div
        className="w-3.5 h-3.5 mt-0.5 shrink-0 rounded-full bg-warm/20"
        animate={{ opacity: [0.3, 0.7, 0.3] }}
        transition={{ duration: 1.5, repeat: Infinity, ease: 'easeInOut' }}
      />
      <div className="flex-1 space-y-1.5">
        <motion.div
          className="h-3.5 rounded bg-tint-3"
          animate={{ opacity: [0.4, 0.7, 0.4] }}
          transition={{ duration: 1.5, repeat: Infinity, ease: 'easeInOut' }}
          style={{ width: '85%' }}
        />
        <motion.div
          className="h-3.5 rounded bg-tint-3"
          animate={{ opacity: [0.4, 0.7, 0.4] }}
          transition={{ duration: 1.5, repeat: Infinity, ease: 'easeInOut', delay: 0.2 }}
          style={{ width: '60%' }}
        />
      </div>
    </div>
  )
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
