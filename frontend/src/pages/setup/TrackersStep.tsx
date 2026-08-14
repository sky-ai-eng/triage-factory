// The Trackers step body (optional) — now just the org-level tracker picker:
// None, Jira, or Linear, one at a time. Trackers are the issue/work
// integrations, distinct from GitHub (the backbone). Linear is a disabled
// "coming soon" placeholder (no poller, no credentials, no ingestion — out of
// scope), so it can be looked at but never selected.
//
// When Jira is chosen it expands into its own atomic steps — a Jira URL step
// and a Jira access step (JiraStep.tsx), gated visible on tracker === 'jira' —
// rather than hosting the URL + connect inline here, so every step is one
// action (the same shape as GitHub's URL/access split).

import { useRef } from 'react'
import { Clock } from 'lucide-react'
import { nextRadioIndex } from '../../lib/rovingRadio'
import type { StepContext, TrackerKind } from './types'

interface TrackerCard {
  kind: TrackerKind
  title: string
  blurb: string
  disabled?: boolean
}

const CARDS: TrackerCard[] = [
  { kind: 'none', title: 'None', blurb: 'GitHub only — add a tracker later in Settings.' },
  { kind: 'jira', title: 'Jira', blurb: 'Track Jira issues alongside your PRs.' },
  { kind: 'linear', title: 'Linear', blurb: 'Coming soon.', disabled: true },
]

export default function TrackersStep({ state, patch }: StepContext) {
  const btnRefs = useRef<(HTMLButtonElement | null)[]>([])
  const selectedIndex = CARDS.findIndex((c) => c.kind === state.tracker)

  const select = (kind: TrackerKind) => {
    if (kind === 'linear') return
    patch({ tracker: kind })
  }

  // Arrow keys move selection across the enabled cards, skipping the disabled
  // "coming soon" Linear card.
  const onKeyDown = (e: React.KeyboardEvent) => {
    const next = nextRadioIndex(e.key, selectedIndex, CARDS.length, (i) => !CARDS[i].disabled)
    if (next === null) return
    e.preventDefault()
    select(CARDS[next].kind)
    btnRefs.current[next]?.focus()
  }

  // Roving tabIndex: one tab stop — the selected card, or the first card when
  // nothing is selected — and arrows move from there.
  const tabbable = selectedIndex < 0 ? 0 : selectedIndex

  return (
    <div className="space-y-4">
      <p className="text-body leading-relaxed text-ink-2">
        Optionally connect an issue tracker. You can skip this and add one later in Settings.
      </p>

      <div
        role="radiogroup"
        aria-label="Issue tracker"
        onKeyDown={onKeyDown}
        className="grid gap-2 sm:grid-cols-3"
      >
        {CARDS.map((card, i) => {
          const selected = state.tracker === card.kind
          return (
            <button
              key={card.kind}
              ref={(el) => {
                btnRefs.current[i] = el
              }}
              type="button"
              role="radio"
              aria-checked={selected}
              aria-disabled={card.disabled}
              disabled={card.disabled}
              tabIndex={i === tabbable ? 0 : -1}
              onClick={() => select(card.kind)}
              className={`flex flex-col items-start gap-1 rounded-xl border px-3.5 py-3 text-left transition-colors ${
                card.disabled
                  ? 'cursor-default border-line-1 bg-tint-2 opacity-55'
                  : selected
                    ? 'border-warm/50 bg-warm/[0.06] shadow-float shadow-black/[0.03]'
                    : 'border-line-1 bg-raised hover:border-warm/30 hover:bg-raised'
              }`}
            >
              <span className="flex items-center gap-1.5">
                <span
                  className={`text-body font-medium ${
                    selected ? 'text-warm' : 'text-ink-1'
                  }`}
                >
                  {card.title}
                </span>
                {card.disabled && (
                  <span className="inline-flex items-center gap-0.5 rounded-full bg-tint-3 px-1.5 py-0.5 text-label-sm font-medium uppercase tracking-wide text-ink-3">
                    <Clock size={9} aria-hidden />
                    Soon
                  </span>
                )}
              </span>
              <span className="text-reported leading-snug text-ink-3">{card.blurb}</span>
            </button>
          )
        })}
      </div>
    </div>
  )
}
