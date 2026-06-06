// Presentational pieces of the setup stack: the section divider and the
// collapsed confirmation bar a completed/upcoming step recedes into. The
// active (expanded) step's heading + body live in Wizard.tsx, where they can
// own the focus ref and the expand/collapse animation.

import { motion } from 'motion/react'
import { glassField } from './glassStyle'

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
          className={`${glassField}${
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
  id,
  title,
  summary,
  complete,
  onEdit,
}: {
  // Step id — used for the shared layoutId so the title morphs continuously
  // between this collapsed row and the active step's heading (the distill).
  id: string
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
      <motion.span
        layoutId={`setup-title-${id}`}
        className="text-[12px] font-medium uppercase tracking-[0.12em] text-text-tertiary transition-colors group-hover:text-text-secondary"
      >
        {title}
      </motion.span>
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
