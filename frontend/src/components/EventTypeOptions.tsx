import type { EventType } from '../types'
import { sourceLabel } from '../lib/eventSources'

// EventTypeOptions — a handler editor's event-type choices as <option>s
// grouped under their source, in the order the catalog lists them, so a
// GitHub vulnerable-package event sits under GitHub beside the pull request
// events. The caller filters; this only groups.
export default function EventTypeOptions({ eventTypes }: { eventTypes: EventType[] }) {
  const groups = new Map<string, EventType[]>()
  for (const et of eventTypes) {
    const group = groups.get(et.source)
    if (group) group.push(et)
    else groups.set(et.source, [et])
  }
  return (
    <>
      {[...groups].map(([source, items]) => (
        <optgroup key={source} label={sourceLabel(source)}>
          {items.map((et) => (
            <option key={et.id} value={et.id}>
              {et.label} ({et.id})
            </option>
          ))}
        </optgroup>
      ))}
    </>
  )
}
