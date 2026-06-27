import { ArrowRight, Bot, ExternalLink, User } from 'lucide-react'
import type { ActivityAction } from '../types'
import { TONE_TEXT, TONE_VAR, type Tone } from './board/cardStyle'
import { metaForAction } from './actionMeta'

// ActionRow renders one external_actions row for the activity feed's Actions lens
// (TFAC-483) — the audit log of record. It is the sibling of ArtifactRow (Objects
// lens), sharing the same row chrome but carrying the action-specific shape: the
// verb (what TF did), a from→to transition where one applies, the target object,
// the acting identity (the authorizing human on the org feed, else bot/human),
// and a link-out to the object. Always a link-out — an action row never opens the
// approval overlay (that's the live board's job, not the audit log's).

export function ActionRow({ action, note }: { action: ActivityAction; note?: React.ReactNode }) {
  const meta = metaForAction(action.action)
  const Icon = meta.icon

  const body = (
    <>
      <Icon size={13} className={`shrink-0 ${meta.text}`} aria-hidden />
      <VerbPill label={meta.label} tone={meta.tone} />
      <span className="min-w-0 flex-1 truncate font-mono text-[11px] text-text-secondary">
        {action.target || action.external_id || meta.label}
      </span>
      {action.from_state && action.to_state && (
        <TransitionChip from={action.from_state} to={action.to_state} />
      )}
      {note}
      <ActorChip name={action.actor_name} actorUserID={action.actor_user_id} />
    </>
  )

  const rowClass =
    'flex w-full items-center gap-2 rounded-[4px] border border-border-subtle bg-black/[0.015] px-2 py-1.5 text-left transition-colors hover:bg-black/[0.04]'

  if (action.url) {
    return (
      <li>
        <a
          href={action.url}
          target="_blank"
          rel="noopener noreferrer"
          className={rowClass}
          aria-label={`${meta.label}: ${action.target} on ${action.provider}`}
          title={`Open on ${action.provider} (new tab)`}
        >
          {body}
          <ExternalLink size={11} className="shrink-0 text-text-tertiary/70" aria-hidden />
        </a>
      </li>
    )
  }
  // No external link (a transition with no browse URL) — a plain, non-interactive
  // row carrying the same content.
  return (
    <li className={rowClass} aria-label={`${meta.label}: ${action.target}`}>
      {body}
    </li>
  )
}

// VerbPill — the action verb in a quiet toned pill (StateBadge's sibling, keyed
// off the action's tone rather than a lifecycle state).
function VerbPill({ label, tone }: { label: string; tone: Tone }) {
  return (
    <span
      className={`shrink-0 rounded-[3px] px-1.5 py-0.5 font-mono text-[9px] font-semibold uppercase tracking-[0.06em] ${TONE_TEXT[tone]}`}
      style={{ background: `color-mix(in srgb, ${TONE_VAR[tone]} 12%, transparent)` }}
    >
      {label}
    </span>
  )
}

// TransitionChip — the from→to endpoints of a transition (a Jira status move, a
// PR draft→open), mono with an arrow between.
function TransitionChip({ from, to }: { from: string; to: string }) {
  return (
    <span className="hidden shrink-0 items-center gap-1 font-mono text-[9px] text-text-tertiary sm:inline-flex">
      <span className="truncate">{from}</span>
      <ArrowRight size={9} className="shrink-0" aria-hidden />
      <span className="truncate font-semibold text-text-secondary">{to}</span>
    </span>
  )
}

// ActorChip — who performed the action: the resolved human display name on the
// org feed; "human" when a human authorized it but the name isn't resolved (the
// team feed); "bot" for an autonomous system action (no actor — an
// event-triggered run, the Jira mirror).
function ActorChip({ name, actorUserID }: { name?: string; actorUserID?: string }) {
  const isHuman = !!name || !!actorUserID
  const label = name || (actorUserID ? 'human' : 'bot')
  const Icon = isHuman ? User : Bot
  return (
    <span
      className="inline-flex max-w-[8rem] shrink-0 items-center gap-1 truncate rounded-[3px] bg-black/[0.04] px-1.5 py-0.5 font-mono text-[9px] text-text-tertiary"
      title={isHuman ? `Authorized by ${label}` : 'Autonomous bot action'}
    >
      <Icon size={9} className="shrink-0" aria-hidden />
      <span className="truncate">{label}</span>
    </span>
  )
}
