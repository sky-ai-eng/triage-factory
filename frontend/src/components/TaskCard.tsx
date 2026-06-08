import { forwardRef } from 'react'
import type { Task } from '../types'
import RequestedReviewerBadge from './RequestedReviewerBadge'
import { CardPlane, EventTag, SourceTag, Spine } from './board/cardChrome'
import { formatAge, type Tone } from './board/cardStyle'

interface Props {
  task: Task
  style?: React.CSSProperties
  isDragging?: boolean
  onRequeue?: () => void
  // SKY-261 B+: when a task is bot-claimed but the delegate run failed
  // to spawn (or no run materialized yet for the bot-claimed task),
  // the agent-lane card surfaces the failure here. delegateFailed
  // carries the error message (e.g. "prompt not found"); onRetry fires
  // the same delegate gesture again. Claim is commitment, runs are
  // execution; this prop is how the Board reflects that divergence.
  delegateFailed?: { message: string }
  onRetry?: () => void
  // SKY-330: caller-supplied assignee picker, rendered at the top-right.
  assigneeSlot?: React.ReactNode
}

// TaskCard — a borderless lit plane for a task with no run yet. Status is the
// spine: rust at rest, amber when snoozed (parked), red when a delegate failed
// to fire. No frame, no badge soup — the event reads as a quiet uppercase tag,
// the title leads.
const TaskCard = forwardRef<HTMLDivElement, Props & React.HTMLAttributes<HTMLDivElement>>(
  (
    { task, style, isDragging, onRequeue, delegateFailed, onRetry, assigneeSlot, ...props },
    ref,
  ) => {
    const age = formatAge(task.created_at)
    const subtaskCount = task.open_subtask_count ?? 0
    // SKY-261 B+: snooze is only expected on unclaimed tasks; we render the
    // badge whenever snooze_until is in the future (a past time means the
    // snooze elapsed and the next wake path will requeue it).
    const snoozedUntil = parseFutureSnooze(task.snooze_until)
    const isSnoozed = snoozedUntil !== null

    const tone: Tone = delegateFailed ? 'problem' : isSnoozed ? 'attention' : 'rust'

    return (
      <div
        ref={ref}
        style={style}
        className={`group relative cursor-grab active:cursor-grabbing ${isDragging ? 'z-50' : ''}`}
        {...props}
      >
        <CardPlane
          glow={delegateFailed ? { tone: 'problem', breathing: false } : undefined}
          dim={isSnoozed}
          dragging={isDragging}
        >
          <Spine tone={tone} />
          <div className="relative py-3.5 pl-4 pr-3.5">
            <div className="mb-2 flex items-center justify-between gap-2">
              <div className="flex min-w-0 items-center gap-2.5">
                <SourceTag task={task} />
                <EventTag eventType={task.event_type} />
                <RequestedReviewerBadge task={task} />
                {subtaskCount > 0 && <SubtaskHint count={subtaskCount} />}
                {isSnoozed && <SnoozedBadge until={snoozedUntil} />}
                {delegateFailed && <DelegateFailedBadge message={delegateFailed.message} />}
              </div>
              {assigneeSlot && <div className="shrink-0">{assigneeSlot}</div>}
            </div>

            <h3 className="mb-1 line-clamp-2 text-[13px] font-semibold leading-snug text-text-primary">
              {task.title}
            </h3>

            {task.ai_summary && (
              <p className="mb-2 line-clamp-2 text-[12px] leading-relaxed text-text-tertiary">
                {task.ai_summary}
              </p>
            )}

            <div className="flex items-center justify-between">
              <span className="text-[11px] tracking-wide text-text-tertiary/80">{age}</span>

              <div className="flex items-center gap-3">
                {delegateFailed && onRetry && (
                  <button
                    onClick={(e) => {
                      e.stopPropagation()
                      onRetry()
                    }}
                    onPointerDown={(e) => e.stopPropagation()}
                    className="text-[12px] font-medium text-dismiss transition-colors hover:text-dismiss/70"
                    title="Re-attempt the delegate run"
                  >
                    Retry
                  </button>
                )}
                {onRequeue && (
                  <button
                    onClick={(e) => {
                      e.stopPropagation()
                      onRequeue()
                    }}
                    onPointerDown={(e) => e.stopPropagation()}
                    className="text-[12px] font-medium text-text-tertiary transition-colors hover:text-text-secondary"
                    title="Return to queue"
                  >
                    Requeue
                  </button>
                )}
                <a
                  href={task.source_url}
                  target="_blank"
                  rel="noopener noreferrer"
                  className="text-[12px] font-semibold text-accent transition-colors hover:text-accent/70"
                  onClick={(e) => e.stopPropagation()}
                  onPointerDown={(e) => e.stopPropagation()}
                >
                  Open
                </a>
              </div>
            </div>
          </div>
        </CardPlane>
      </div>
    )
  },
)

TaskCard.displayName = 'TaskCard'
export default TaskCard

// SubtaskHint appears on a task card whose Jira entity has gained open
// subtasks since the task was created — the task represents scope that has
// since been decomposed. Amber to read "worth a second look, not an error".
function SubtaskHint({ count }: { count: number }) {
  const label = count === 1 ? '1 open subtask' : `${count} open subtasks`
  return (
    <span
      title="This ticket has open subtasks — the work may have been decomposed since the task was queued. Consider dismissing and working the subtasks directly."
      className="inline-flex shrink-0 items-center gap-1 text-[10px] font-medium text-snooze"
    >
      <span aria-hidden>⋮</span>
      {label}
    </span>
  )
}

// DelegateFailedBadge surfaces "the bot is claimed but the run didn't fire" —
// the SKY-261 B+ failure state. Red to match the card's problem spine.
function DelegateFailedBadge({ message }: { message: string }) {
  return (
    <span
      title={`The bot took this task but the run didn't fire: ${message}. Click Retry to re-attempt.`}
      className="inline-flex shrink-0 items-center gap-1 text-[10px] font-medium text-dismiss"
    >
      <span aria-hidden>⚠</span>
      didn't fire
    </span>
  )
}

// SnoozedBadge surfaces "task is parked until X." Amber, matching the snoozed
// card's spine + the muted plane opacity (set aside on purpose).
function SnoozedBadge({ until }: { until: Date }) {
  return (
    <span
      title={`Snoozed until ${until.toLocaleString()}. Wakes automatically on next matching event, or via the Requeue affordance.`}
      className="inline-flex shrink-0 items-center gap-1 text-[10px] font-medium text-snooze"
    >
      <span aria-hidden>⏾</span>
      wakes {formatSnoozeUntil(until)}
    </span>
  )
}

// parseFutureSnooze returns the snooze target as a Date when the task is
// currently snoozed (parseable + in the future); null otherwise.
function parseFutureSnooze(snoozeUntil: string | undefined): Date | null {
  if (!snoozeUntil) return null
  const d = new Date(snoozeUntil)
  if (Number.isNaN(d.getTime())) return null
  if (d.getTime() <= Date.now()) return null
  return d
}

// formatSnoozeUntil prints "in 2h" / "in 3d" / "Mar 5" so the badge stays
// compact. The full timestamp lives in the title attribute.
function formatSnoozeUntil(until: Date): string {
  const diff = until.getTime() - Date.now()
  const hours = Math.floor(diff / (1000 * 60 * 60))
  if (hours < 1) return 'soon'
  if (hours < 24) return `in ${hours}h`
  const days = Math.floor(hours / 24)
  if (days < 7) return `in ${days}d`
  return until.toLocaleDateString(undefined, { month: 'short', day: 'numeric' })
}
