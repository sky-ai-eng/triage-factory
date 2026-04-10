import { forwardRef } from 'react'
import type { Task } from '../types'
import EventBadge from './EventBadge'

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
          <span className={`text-[10px] font-semibold uppercase tracking-wider px-1.5 py-0.5 rounded ${
            task.source === 'github' ? 'bg-black/[0.04] text-text-secondary' : 'bg-blue-500/10 text-blue-600'
          }`}>
            {task.source === 'github' ? 'GH' : 'Jira'}
          </span>
          <EventBadge eventType={task.event_type} compact />
          {task.repo && <span className="text-[11px] text-text-tertiary truncate">{task.repo}</span>}
        </div>

        <h3 className="text-[13px] font-semibold text-text-primary leading-snug line-clamp-2 mb-1">
          {task.title}
        </h3>

        {task.ai_summary && (
          <p className="text-[12px] text-text-tertiary line-clamp-2 mb-2">{task.ai_summary}</p>
        )}

        <div className="flex items-center justify-between">
          <div className="flex items-center gap-2 text-[11px] text-text-tertiary">
            {task.author && <span>{task.author}</span>}
            {(task.diff_additions || task.diff_deletions) && (
              <span>
                {task.diff_additions ? <span className="text-claim">+{task.diff_additions}</span> : null}
                {task.diff_deletions ? <span className="text-dismiss ml-1">-{task.diff_deletions}</span> : null}
              </span>
            )}
            <span>{age}</span>
          </div>

          <div className="flex items-center gap-3">
            {onRequeue && (
              <button
                onClick={(e) => { e.stopPropagation(); onRequeue() }}
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
  }
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
