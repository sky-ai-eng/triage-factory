// Presentational pieces of the setup stack: the section divider and the
// collapsed confirmation bar a completed/upcoming step recedes into. The
// active (expanded) step's heading + body live in Wizard.tsx, where they can
// own the focus ref and the expand/collapse animation.

import { useRef } from 'react'
import { ChevronRight } from 'lucide-react'
import { nextRadioIndex } from '../../lib/rovingRadio'
import { glassInputClass } from '../settings/primitives'

// ChoiceCards is the flush two-panel picker shared by the GitHub access-method,
// account-type, and clone-protocol steps: side-by-side options (a hairline
// between, no box), each a title + a one-line detail, with a chevron that fades
// in on hover to signal it advances. Action-on-click — the caller's onChoose
// both records the choice and advances (the step is selfAdvancing), so there's
// no Continue. The currently-selected option (when revisiting) reads in accent.
//
// An option may carry an optional `status` — a short line reflecting persisted
// reality (the GitHub mode cards use it for "Registered (slug) — not installed
// yet" / "Connected"), rendered under the detail in the claim tint so it reads
// as state, not description.
export function ChoiceCards<T extends string>({
  options,
  selected,
  onChoose,
  ariaLabel,
}: {
  options: { kind: T; title: string; detail: string; status?: string }[]
  selected: T | null
  onChoose: (kind: T) => void
  ariaLabel?: string
}) {
  const btnRefs = useRef<(HTMLButtonElement | null)[]>([])
  // Roving tabIndex: focus enters on the current choice, or the first option
  // when nothing's been picked yet.
  const selectedIndex = options.findIndex((o) => o.kind === selected)
  const tabbable = selectedIndex < 0 ? 0 : selectedIndex
  // Arrow keys move *focus* between the options, not selection: choosing here
  // advances the wizard (selfAdvancing), so an arrow press must never commit +
  // advance. The user lands focus, then activates with Enter/Space (native
  // button → onClick → onChoose).
  const onKeyDown = (e: React.KeyboardEvent) => {
    const current = btnRefs.current.findIndex((b) => b === document.activeElement)
    const next = nextRadioIndex(e.key, current, options.length)
    if (next === null) return
    e.preventDefault()
    btnRefs.current[next]?.focus()
  }
  return (
    <div
      role="radiogroup"
      aria-label={ariaLabel}
      onKeyDown={onKeyDown}
      className="grid grid-cols-2 divide-x divide-[var(--color-border-subtle)]"
    >
      {options.map((opt, i) => {
        const isSelected = selected === opt.kind
        return (
          <button
            key={opt.kind}
            ref={(el) => {
              btnRefs.current[i] = el
            }}
            type="button"
            role="radio"
            aria-checked={isSelected}
            tabIndex={i === tabbable ? 0 : -1}
            onClick={() => onChoose(opt.kind)}
            className={`group flex flex-col gap-1 rounded-lg text-left outline-none transition focus-visible:outline focus-visible:outline-2 focus-visible:outline-offset-2 focus-visible:outline-accent/50 ${i === 0 ? 'pr-5' : 'pl-5'}`}
          >
            <span className="flex items-center gap-1.5">
              <span
                className={`text-[14px] font-medium transition-colors ${
                  isSelected ? 'text-accent' : 'text-text-secondary group-hover:text-text-primary'
                }`}
              >
                {opt.title}
              </span>
              <ChevronRight
                size={14}
                aria-hidden
                className="text-accent opacity-0 transition-opacity group-hover:opacity-100"
              />
            </span>
            <span className="text-[11px] leading-snug text-text-tertiary">{opt.detail}</span>
            {opt.status && (
              <span className="mt-0.5 text-[11px] font-medium leading-snug text-claim">
                {opt.status}
              </span>
            )}
          </button>
        )
      })}
    </div>
  )
}

// UrlField is the base-URL input shared by the GitHub and Jira URL steps: a
// controlled field bound straight to wizard state (no local draft), so the
// step's Continue reads the typed value from state and runs the reachability
// probe itself — there is no separate "Verify" button. `invalid` turns the box
// red when the active step carries a probe error; the message itself renders
// once in the host's shared error line, so the field only owns the red border.
export function UrlField({
  label,
  value,
  onChange,
  placeholder,
  helpText,
  invalid,
}: {
  label: string
  value: string
  onChange: (value: string) => void
  placeholder?: string
  helpText?: string
  invalid?: boolean
}) {
  return (
    <div className="space-y-2">
      <label className="block">
        <span className="mb-2 block text-[11px] font-medium uppercase tracking-wide text-text-tertiary">
          {label}
        </span>
        <input
          type="url"
          value={value}
          placeholder={placeholder || 'https://…'}
          onChange={(e) => onChange(e.target.value)}
          aria-invalid={invalid || undefined}
          className={`${glassInputClass}${
            invalid
              ? ' !border-[var(--color-dismiss)] focus:!border-[var(--color-dismiss)] focus:!shadow-[0_0_0_4px_rgba(168,69,69,0.16)]'
              : ''
          }`}
        />
      </label>
      {helpText && <p className="text-[11px] leading-relaxed text-text-tertiary">{helpText}</p>}
    </div>
  )
}

// ReuseCredentialCheckbox is the local-mode-only "use this token as my own
// identity too" toggle the org GitHub-PAT and Jira-access steps render under
// their token field. Checking it reuses the just-entered org credential (PAT_1)
// to bind the operator's own identity (PAT_2) on connect, so a solo local
// operator — whose org and personal token are usually the same — doesn't paste
// it again on the User step. Multi never renders it (org creds stay non-user);
// the step bodies gate on isLocal.
export function ReuseCredentialCheckbox({
  checked,
  onChange,
  label,
  hint,
}: {
  checked: boolean
  onChange: (checked: boolean) => void
  label: string
  hint: string
}) {
  return (
    <label className="flex cursor-pointer items-start gap-2.5">
      <input
        type="checkbox"
        checked={checked}
        onChange={(e) => onChange(e.target.checked)}
        className="mt-0.5 accent-accent"
      />
      <span className="block">
        <span className="block text-[13px] text-text-secondary">{label}</span>
        <span className="mt-0.5 block text-[11px] leading-relaxed text-text-tertiary">{hint}</span>
      </span>
    </label>
  )
}

// SectionDivider labels a group of steps ("Organization settings" / "Team
// settings (first team)"). Rendered as the group's heading so the collapsed
// stack stays screen-reader navigable by section.
export function SectionDivider({ title, id }: { title: string; id: string }) {
  return (
    <div className="flex items-center gap-3 mb-2.5 mt-1">
      <h2 id={id} className="text-[11px] font-medium uppercase tracking-wide text-text-tertiary">
        {title}
      </h2>
      <div className="h-px flex-1 bg-border-subtle" />
    </div>
  )
}

// CollapsedStepBar renders a completed step that has receded above the active
// one: a thin, flush row — no card/pill chrome, and no marker (the wizard's
// gutter thread owns the marker) — with the title and the summary of what was
// saved, that re-expands on click. Only steps at or before the active step are
// ever rendered (nothing below the active step shows), so every row is reachable
// and editable; there is no inert "upcoming" variant.
export function CollapsedStepBar({
  title,
  summary,
  complete,
  onEdit,
}: {
  title: string
  summary: string
  complete: boolean
  onEdit: () => void
}) {
  return (
    <button
      type="button"
      onClick={onEdit}
      aria-label={complete ? `${title} — completed. Edit.` : `${title} — in progress. Edit.`}
      className="group flex w-full items-baseline gap-2.5 py-1 text-left"
    >
      <span className="text-[12px] font-medium uppercase tracking-[0.12em] text-text-tertiary transition-colors group-hover:text-text-secondary">
        {title}
      </span>
      {complete && (
        <span className="truncate text-[12px] text-text-tertiary/80" title={summary}>
          · {summary}
        </span>
      )}
      <span className="ml-auto text-[11px] text-text-tertiary opacity-0 transition-opacity group-hover:text-accent group-hover:opacity-100">
        Edit
      </span>
    </button>
  )
}
