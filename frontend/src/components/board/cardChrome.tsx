import { Tooltip } from '../../ui/tooltip/Tooltip'
import type { Task } from '../../types'
import { eventDisplay, eventTone } from '../../lib/eventDisplay'
import { TONE_TEXT } from './cardStyle'

// cardChrome — the source and event readouts the run station's masthead
// composes. Every export here is a component (Fast Refresh rule); the tokens
// live in cardStyle.ts.

// EventTag is the detuned event-type label — uppercase tracked text in one of
// the four warm tones (no pastel pill). Keeps EventBadge's tooltip so the full
// description is a hover away.
export function EventTag({ eventType }: { eventType?: string }) {
  if (!eventType) return null
  const info = eventDisplay(eventType)
  const tone = eventTone(eventType)
  return (
    <Tooltip content={info.description} wrap>
      <span
        className={`cursor-default text-label font-semibold uppercase tracking-[0.09em] ${TONE_TEXT[tone]}`}
      >
        {info.label}
      </span>
    </Tooltip>
  )
}

// SourceTag is the source marker, detuned to a monospace uppercase glyph (no
// blue Jira pill) so it sits quietly in the warm field as a HUD readout.
export function SourceTag({ task }: { task: Task }) {
  const label =
    task.source === 'github'
      ? task.entity_kind === 'pr'
        ? 'PR'
        : 'GH'
      : task.source === 'slack'
        ? 'SLACK'
        : 'JIRA'
  return (
    <span className="font-mono text-label font-semibold uppercase tracking-[0.12em] text-ink-3/80">
      {label}
    </span>
  )
}
