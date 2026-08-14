// ModelTierSelector — a capability *ladder*, not a row of equal cards. The
// tiers ascend left→right (Haiku → Sonnet → Opus), and the selection reads as a
// ceiling: a warm fill runs along the track up to the chosen tier, everything
// above it ghosts out (the clamped region), and a "No cap" (∞) end lifts the
// ceiling entirely. This maps the data's actual shape — an ordered cap — onto
// the control, so a team default above the cap *visibly* sits in the ghosted
// zone.
//
// Two modes, inferred from the options: a CAP set includes the "" (no cap) end
// and gets the fill + ghost-above ceiling treatment; a plain PICK set (the team
// default — concrete tiers only) just highlights the chosen tier. Controlled,
// value-typed as a plain string, radiogroup + roving tabIndex + arrow keys —
// the same a11y contract whichever surface composes it.

import { useRef } from 'react'
import { nextRadioIndex } from '../../lib/rovingRadio'
import type { ModelTierOption } from './modelTiers'

export type { ModelTierOption }

export default function ModelTierSelector({
  value,
  onChange,
  options,
  ariaLabel,
}: {
  value: string
  onChange: (value: string) => void
  options: ModelTierOption[]
  ariaLabel: string
}) {
  const btnRefs = useRef<(HTMLButtonElement | null)[]>([])

  // A CAP set carries the "" (no cap) end — that turns on the ceiling treatment.
  const isCap = options.some((o) => o.value === '')
  // Tier cells in ascending order (the no-cap end isn't a tier).
  const tiers = options.filter((o) => o.value !== '')
  // Where the ceiling sits among the tiers: no cap ("") ⇒ above every tier. An
  // unrecognized tier (a server-side value the UI doesn't list yet) findIndex's
  // to -1; treat that as no cap too, so the ladder doesn't ghost every row and
  // misread as fully disabled.
  const knownTierIndex = tiers.findIndex((o) => o.value === value)
  const capTierIndex = value === '' || knownTierIndex < 0 ? tiers.length : knownTierIndex

  const selectedIndex = options.findIndex((o) => o.value === value)
  const tabbable = selectedIndex < 0 ? 0 : selectedIndex
  const onKeyDown = (e: React.KeyboardEvent) => {
    const next = nextRadioIndex(e.key, selectedIndex, options.length)
    if (next === null) return
    e.preventDefault()
    onChange(options[next].value)
    btnRefs.current[next]?.focus()
  }

  return (
    <div
      role="radiogroup"
      aria-label={ariaLabel}
      onKeyDown={onKeyDown}
      className="flex items-stretch"
    >
      {options.map((opt, i) => {
        const isNoCap = opt.value === ''
        const selected = opt.value === value
        const tierIndex = isNoCap ? -1 : tiers.findIndex((t) => t.value === opt.value)

        let aboveCap = false
        let filled = false
        if (isCap) {
          if (isNoCap) {
            // The end is "filled" only when it's the active choice (full ceiling).
            filled = value === ''
          } else {
            aboveCap = tierIndex > capTierIndex
            filled = tierIndex <= capTierIndex // fill up to (and including) the cap
          }
        } else {
          filled = selected
        }

        const textClass = selected
          ? 'text-warm'
          : aboveCap
            ? 'text-ink-3'
            : 'text-ink-2'
        const railClass = selected
          ? 'bg-warm shadow-[0_0_10px_-1px_var(--color-warm)]'
          : filled
            ? 'bg-warm'
            : 'bg-[var(--color-line-1)]'

        return (
          <button
            key={opt.value || 'none'}
            ref={(el) => {
              btnRefs.current[i] = el
            }}
            type="button"
            role="radio"
            aria-checked={selected}
            tabIndex={i === tabbable ? 0 : -1}
            onClick={() => onChange(opt.value)}
            className={`group relative flex-1 px-3 pb-3 pt-2 text-left outline-none transition-opacity ${
              aboveCap ? 'opacity-50' : 'opacity-100'
            }`}
          >
            <span
              className={`flex items-center gap-1 text-body font-medium transition-colors ${textClass}`}
            >
              {isNoCap && <span className="text-column leading-none">∞</span>}
              {opt.label}
            </span>
            {opt.hint && (
              <span className="mt-0.5 block text-reported leading-snug text-ink-3">
                {opt.hint}
              </span>
            )}
            <span
              aria-hidden
              className={`absolute inset-x-0 bottom-0 h-[2px] transition-colors ${railClass}`}
            />
          </button>
        )
      })}
    </div>
  )
}
