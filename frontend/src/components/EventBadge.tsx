import { Tooltip } from '../ui/tooltip/Tooltip'
import { eventDisplay } from '../lib/eventDisplay'

// EventBadge renders the saturated event-type pill used by the filter chips
// (where the color is load-bearing — selected shows full color, unselected
// grayscales). The label/description/color data lives in lib/eventDisplay so
// non-badge surfaces (the board cards' detuned event tags) can reuse it
// without importing this component.
export default function EventBadge({
  eventType,
  compact,
}: {
  eventType?: string
  compact?: boolean
}) {
  if (!eventType) return null
  const info = eventDisplay(eventType)

  const badge = compact ? (
    <span
      className={`text-label font-semibold uppercase tracking-wider px-1.5 py-0.5 rounded cursor-default ${info.color}`}
    >
      {info.label}
    </span>
  ) : (
    <span
      className={`text-reported font-semibold px-2.5 py-1 rounded-full cursor-default ${info.color}`}
    >
      {info.label}
    </span>
  )

  return (
    <Tooltip content={info.description} wrap>
      {badge}
    </Tooltip>
  )
}
