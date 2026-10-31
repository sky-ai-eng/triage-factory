import { ExternalLink } from 'lucide-react'
import type { ActivityObject } from '../types'
import { VerbPill } from './ActionRow'
import { StateBadge } from './DeliverableRow'
import { metaForAction } from './actionMeta'
import { metaForObjectKind, rowTone } from './deliverableMeta'

// ObjectRow renders one item of the activity feed's Objects lens: an object
// the scope's runs changed, once however many runs changed it. It reads as
// what the runs did — the kind glyph, the last action's verb, the target, and
// how many runs when more than one did — and an object that is also a
// deliverable (a pull request, a posted review) is highlighted, carrying the
// deliverable's state. Always a link-out when the object has a url.

export function ObjectRow({ object, note }: { object: ActivityObject; note?: React.ReactNode }) {
  const kind = metaForObjectKind(object.object_kind)
  const action = metaForAction(object.last_action)
  const Icon = kind.icon
  const subject = object.target || kind.label
  const runs = object.run_count > 1 ? `${object.run_count} runs` : null
  const deliverable = object.deliverable

  const body = (
    <>
      <Icon size={13} className={`shrink-0 ${kind.text}`} aria-hidden />
      <VerbPill label={action.label} tone={action.tone} />
      <span className="min-w-0 flex-1 truncate font-mono text-reported text-ink-2">{subject}</span>
      {runs && (
        <span
          className="shrink-0 font-mono text-label-sm tabular-nums text-ink-3"
          title={`${object.action_count} actions across ${object.run_count} runs`}
        >
          {runs}
        </span>
      )}
      {note}
      {deliverable && <StateBadge state={deliverable.state} />}
    </>
  )

  const rowClass = `flex w-full items-center gap-2 rounded-[4px] border px-2 py-1.5 text-left transition-colors ${rowTone(!!deliverable)}`

  // The aria-label replaces the row's content for a screen reader, so it
  // spells out everything the row shows.
  const described = [
    `${kind.label} ${subject}`,
    `last ${action.label}`,
    runs,
    deliverable ? `${deliverable.kind.replace('_', ' ')} ${deliverable.state}` : null,
  ]
    .filter(Boolean)
    .join(', ')

  if (object.url) {
    return (
      <li>
        <a
          href={object.url}
          target="_blank"
          rel="noopener noreferrer"
          className={rowClass}
          aria-label={`${described} on ${object.provider}`}
          title={`Open on ${object.provider} (new tab)`}
          data-deliverable={deliverable ? deliverable.state : undefined}
        >
          {body}
          <ExternalLink size={11} className="shrink-0 text-ink-3/70" aria-hidden />
        </a>
      </li>
    )
  }
  return (
    <li
      className={rowClass}
      aria-label={described}
      data-deliverable={deliverable ? deliverable.state : undefined}
    >
      {body}
    </li>
  )
}
