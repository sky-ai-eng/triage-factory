import * as Tooltip from '@radix-ui/react-tooltip'
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
      className={`text-[10px] font-semibold uppercase tracking-wider px-1.5 py-0.5 rounded cursor-default ${info.color}`}
    >
      {info.label}
    </span>
  ) : (
    <span
      className={`text-[11px] font-semibold px-2.5 py-1 rounded-full cursor-default ${info.color}`}
    >
      {info.label}
    </span>
  )

  return (
    <Tooltip.Provider delayDuration={200}>
      <Tooltip.Root>
        <Tooltip.Trigger asChild>{badge}</Tooltip.Trigger>
        <Tooltip.Portal>
          <Tooltip.Content
            side="top"
            sideOffset={6}
            className="z-[100] max-w-[240px] rounded-lg bg-surface-raised border border-border-glass px-3 py-2 shadow-lg shadow-black/[0.06] text-[12px] text-text-secondary leading-relaxed animate-in fade-in-0 zoom-in-95"
          >
            {info.description}
            <Tooltip.Arrow className="fill-surface-raised" />
          </Tooltip.Content>
        </Tooltip.Portal>
      </Tooltip.Root>
    </Tooltip.Provider>
  )
}
