import { useState } from 'react'
import type { CSSProperties, MouseEvent, KeyboardEvent, ReactNode } from 'react'
import { ICONS } from './icons'
import './source-card.css'

export type SourceState = 'configured' | 'unavailable' | 'soon'
export type SourceStat = [label: string, value: ReactNode, tone?: 'warm' | 'alarm']

export type SourceCardProps = {
  /** Display name, and the card's accessible name when clickable. */
  name: ReactNode
  /** Which brand mark to draw: github | jira | slack | linear | schedule. */
  source?: string
  /** Supply a mark directly, bypassing the built-in set. */
  icon?: ReactNode
  /**
   * configured  — this team has it on: raised, bordered, carries numbers, takes a click.
   * unavailable — the organization has not connected it: same card sunk, numbers
   *               replaced by the one sentence that would change that.
   * soon        — does not exist yet: outline only, nothing to sink or click.
   */
  state?: SourceState
  /** What the connection covers — an org, a project key, a workspace. */
  scope?: ReactNode
  /** Up to a few [label, value, tone] rows. Only drawn when configured. */
  stats?: SourceStat[]
  /** The sentence under the rule. Only drawn when NOT configured. */
  note?: ReactNode
  /** The verb offered on `unavailable` only. */
  actionLabel?: ReactNode
  onAction?: (event: MouseEvent | KeyboardEvent) => void
  /** Opens the source's own page. Only honoured when configured. */
  onClick?: (event: MouseEvent | KeyboardEvent) => void
  /** Persists the ask for this user under tf.src-request.<key>. */
  requestKey?: string
  /** Controlled form of the ask, when the caller holds the record. */
  requested?: boolean
  requestedLabel?: ReactNode
  title?: string
  /** Escape hatch for layout only. Color and state belong to source-card.css. */
  style?: CSSProperties
}

export function SourceCard({
  name,
  source,
  icon,
  state = 'configured',
  scope,
  stats = [],
  note,
  actionLabel,
  onAction,
  onClick,
  requestKey,
  requested,
  requestedLabel = 'requested · waiting on an org admin',
  title,
  style,
}: SourceCardProps) {
  // Asking an org admin to connect a source is a one-time act: there is nothing
  // a second request would add, so the card remembers it for this user and the
  // verb never comes back. Controlled with `requested` when the caller has the
  // record; otherwise the key is enough.
  const [asked, setAsked] = useState(() => {
    if (requested !== undefined) return requested
    if (!requestKey) return false
    try {
      return localStorage.getItem('tf.src-request.' + requestKey) === '1'
    } catch {
      return false
    }
  })
  const askedNow = requested !== undefined ? requested : asked
  const ask = (event: MouseEvent | KeyboardEvent) => {
    event.stopPropagation()
    if (askedNow) return
    if (requestKey) {
      try {
        localStorage.setItem('tf.src-request.' + requestKey, '1')
      } catch {
        /* private mode — the ask just does not persist */
      }
    }
    setAsked(true)
    if (onAction) onAction(event)
  }

  const live = state === 'configured'
  const blocked = state === 'unavailable'
  const clickable = live && !!onClick
  const mark = source || String(name || '').toLowerCase()
  const art = icon !== undefined ? icon : ICONS[mark] || null

  // Clicking a configured card opens that source's own page, so the card is a
  // link: role="link" activates on ENTER and not on space, which is what a
  // keyboard reader expects of something that navigates.
  const onKeyDown = clickable
    ? (event: KeyboardEvent<HTMLDivElement>) => {
        if (event.key !== 'Enter') return
        event.preventDefault()
        onClick!(event)
      }
    : undefined

  return (
    <div
      className="tf-src"
      data-state={state}
      data-clickable={clickable || undefined}
      role={clickable ? 'link' : undefined}
      tabIndex={clickable ? 0 : undefined}
      aria-label={clickable ? title || (typeof name === 'string' ? name : undefined) : undefined}
      onClick={clickable ? onClick : undefined}
      onKeyDown={onKeyDown}
      title={title}
      style={style}
    >
      <div className="tf-src__head">
        {art ? (
          <span className="tf-src__mark" data-mark={mark} aria-hidden="true">
            {art}
          </span>
        ) : null}
        <div className="tf-src__id">
          <span className="tf-src__name">{name}</span>
          {scope ? <span className="tf-src__scope">{scope}</span> : null}
        </div>
      </div>

      {live && stats.length ? (
        <div className="tf-src__stats">
          {stats.map(([label, value, tone], i) => (
            <div className="tf-src__stat" key={i}>
              <span className="tf-src__stat-label">{label}</span>
              {/* The leader is what makes a stack of two numbers a table. */}
              <span className="tf-src__leader" />
              <span className="tf-src__stat-value" data-tone={tone}>
                {value}
              </span>
            </div>
          ))}
        </div>
      ) : null}

      {!live && (note || actionLabel) ? (
        <div className="tf-src__foot">
          {note ? <span className="tf-src__note">{note}</span> : null}
          {/* Only the blocked state gets a verb: there is something a reader can
              do about an unconnected source, and nothing to do about one that
              has not been built. */}
          {blocked && (actionLabel || askedNow) ? (
            askedNow ? (
              <span className="tf-src__asked">
                <svg
                  width="9"
                  height="9"
                  viewBox="0 0 12 12"
                  fill="none"
                  stroke="currentColor"
                  strokeWidth="1.7"
                  strokeLinecap="round"
                  strokeLinejoin="round"
                  aria-hidden="true"
                >
                  <path d="M2.8 6.2 4.9 8.3 9.2 3.5" />
                </svg>
                {requestedLabel}
              </span>
            ) : (
              <button type="button" className="tf-src__ask" onClick={ask}>
                {actionLabel}
              </button>
            )
          ) : null}
        </div>
      ) : null}
    </div>
  )
}

export default SourceCard
