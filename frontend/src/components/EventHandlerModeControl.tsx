import { Segmented } from '../ui/segmented/Segmented'
import type { EventHandlerMode } from '../types'
import { EVENT_HANDLER_MODES, notifySource } from '../lib/eventHandlers'

interface EventHandlerModeControlProps {
  mode: EventHandlerMode
  eventType: string
  // hasBlueprint is whether the handler has a blueprint to run; automate is
  // unpickable without one.
  hasBlueprint: boolean
  onChange: (mode: EventHandlerMode) => void
  disabled?: boolean
  size?: 'sm' | 'md'
}

// EventHandlerModeControl picks a handler's mode. Automate needs a blueprint,
// and notify a GitHub, Jira or Linear event; the server refuses both
// otherwise, so the control strikes them with the reason rather than letting
// the write fail.
export default function EventHandlerModeControl({
  mode,
  eventType,
  hasBlueprint,
  onChange,
  disabled,
  size = 'sm',
}: EventHandlerModeControlProps) {
  const options = EVENT_HANDLER_MODES.map((m) => {
    if (m.value === 'automate' && !hasBlueprint) {
      return { ...m, disabled: true, note: 'Attach a blueprint to automate this event.' }
    }
    if (m.value === 'notify' && !notifySource(eventType)) {
      return { ...m, disabled: true, note: 'Notify covers GitHub, Jira and Linear events.' }
    }
    return m
  })
  return (
    <Segmented
      options={options}
      value={mode}
      onChange={(v) => onChange(v as EventHandlerMode)}
      size={size}
      mono={false}
      label="Mode"
      disabled={disabled}
    />
  )
}
