// The Trackers step body (optional) — just the org-level tracker picker: None,
// Jira, or Linear, one at a time. Trackers are the issue/work integrations,
// distinct from GitHub (the backbone). Picking one only decides which steps
// follow; the other can still be connected later in Settings.
//
// Each choice expands into its own atomic steps rather than hosting a connect
// inline here, so every step is one action: Jira into URL → deployment →
// access (JiraStep.tsx), Linear into access (LinearStep.tsx), each gated
// visible on the tracker.

import { useRef } from 'react'
import { nextRadioIndex } from '../../lib/rovingRadio'
import type { StepContext, TrackerKind } from './types'

interface TrackerCard {
  kind: TrackerKind
  title: string
  blurb: string
}

const CARDS: TrackerCard[] = [
  { kind: 'none', title: 'None', blurb: 'GitHub only — add a tracker later in Settings.' },
  { kind: 'jira', title: 'Jira', blurb: 'Track Jira issues alongside your PRs.' },
  { kind: 'linear', title: 'Linear', blurb: 'Track Linear issues alongside your PRs.' },
]

export default function TrackersStep({ state, patch }: StepContext) {
  const btnRefs = useRef<(HTMLButtonElement | null)[]>([])
  const selectedIndex = CARDS.findIndex((c) => c.kind === state.tracker)

  const select = (kind: TrackerKind) => {
    patch({ tracker: kind })
  }

  // Arrow keys move selection across the cards.
  const onKeyDown = (e: React.KeyboardEvent) => {
    const next = nextRadioIndex(e.key, selectedIndex, CARDS.length)
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
              tabIndex={i === tabbable ? 0 : -1}
              onClick={() => select(card.kind)}
              className={`flex flex-col items-start gap-1 rounded-xl border px-3.5 py-3 text-left transition-colors ${
                selected
                  ? 'border-warm/50 bg-warm/[0.06] shadow-float shadow-black/[0.03]'
                  : 'border-line-1 bg-raised hover:border-warm/30 hover:bg-sunk'
              }`}
            >
              <span className={`text-body font-medium ${selected ? 'text-warm' : 'text-ink-1'}`}>
                {card.title}
              </span>
              <span className="text-reported leading-snug text-ink-3">{card.blurb}</span>
            </button>
          )
        })}
      </div>
    </div>
  )
}
