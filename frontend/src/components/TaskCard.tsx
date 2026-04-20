import { forwardRef } from 'react'
import type { Task } from '../types'
import EventBadge from './EventBadge'
import SourceBadge from './SourceBadge'

interface Props {
  task: Task
  style?: React.CSSProperties
  isDragging?: boolean
  onRequeue?: () => void
}

const TaskCard = forwardRef<HTMLDivElement, Props & React.HTMLAttributes<HTMLDivElement>>(
  ({ task, style, isDragging, onRequeue, ...props }, ref) => {
    const age = formatAge(task.created_at)

    return (
      <div
        ref={ref}
        style={style}
        className={`bg-surface-raised backdrop-blur-xl border border-border-glass rounded-2xl p-4 shadow-sm shadow-black/[0.02] transition-shadow cursor-grab active:cursor-grabbing ${
          isDragging ? 'shadow-lg shadow-black/[0.08] border-accent/30 z-50' : ''
        }`}
        {...props}
      >
        <div className="flex items-center gap-2 mb-2">
          <SourceBadge task={task} />
          <EventBadge eventType={task.event_type} compact />
          {(task.open_subtask_count ?? 0) > 0 && <SubtaskHint count={task.open_subtask_count!} />}
        </div>

        <h3 className="text-[13px] font-semibold text-text-primary leading-snug line-clamp-2 mb-1">
          {task.title}
        </h3>

        {task.ai_summary && (
          <p className="text-[12px] text-text-tertiary line-clamp-2 mb-2">{task.ai_summary}</p>
        )}

        <div className="flex items-center justify-between">
          <div className="flex items-center gap-2 text-[11px] text-text-tertiary">
            <span>{age}</span>
          </div>

          <div className="flex items-center gap-3">
            {onRequeue && (
              <button
                onClick={(e) => {
                  e.stopPropagation()
                  onRequeue()
                }}
                onPointerDown={(e) => e.stopPropagation()}
                className="text-[12px] text-text-tertiary hover:text-text-primary font-medium transition-colors"
                title="Return to queue"
              >
                Requeue
              </button>
            )}
            <a
              href={task.source_url}
              target="_blank"
              rel="noopener noreferrer"
              className="text-[12px] text-accent hover:text-accent/70 font-medium transition-colors"
              onClick={(e) => e.stopPropagation()}
              onPointerDown={(e) => e.stopPropagation()}
            >
              Open
            </a>
          </div>
        </div>
      </div>
    )
  },
)

TaskCard.displayName = 'TaskCard'
export default TaskCard

function formatAge(dateStr: string): string {
  const diff = Date.now() - new Date(dateStr).getTime()
  const hours = Math.floor(diff / (1000 * 60 * 60))
  if (hours < 1) return 'just now'
  if (hours < 24) return `${hours}h ago`
  const days = Math.floor(hours / 24)
  return `${days}d ago`
}

// SubtaskHint appears on a task card whose Jira entity has gained open
// subtasks since the task was created — the task represents scope that
// has since been decomposed. Uses the snooze color (warm amber) rather
// than dismiss/accent to read as "worth a second look, not an error".
function SubtaskHint({ count }: { count: number }) {
  const label = count === 1 ? '1 open subtask' : `${count} open subtasks`
  return (
    <span
      title="This ticket has open subtasks — the work may have been decomposed since the task was queued. Consider dismissing and working the subtasks directly."
      className="inline-flex items-center gap-1 rounded-full border border-snooze/25 bg-snooze/[0.08] px-1.5 py-0.5 text-[10px] font-medium text-snooze"
    >
      <span aria-hidden>⋮</span>
      {label}
    </span>
  )
}
