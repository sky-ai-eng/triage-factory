// Presentational pieces of the setup stack: the section divider and the
// collapsed confirmation bar a completed/upcoming step recedes into. The
// active (expanded) step's heading + body live in Wizard.tsx, where they can
// own the focus ref and the expand/collapse animation.

import { Check } from 'lucide-react'

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

// StepMarker is the leading dot of a bar: a green check once complete, else
// the (1-based) step number in a neutral circle.
function StepMarker({ number, complete }: { number: number; complete: boolean }) {
  if (complete) {
    return (
      <span className="flex h-5 w-5 shrink-0 items-center justify-center rounded-full bg-[var(--color-claim)]/15 text-[var(--color-claim)]">
        <Check size={12} strokeWidth={3} aria-hidden />
      </span>
    )
  }
  return (
    <span className="flex h-5 w-5 shrink-0 items-center justify-center rounded-full bg-black/[0.05] text-[10px] font-medium text-text-tertiary">
      {number}
    </span>
  )
}

// CollapsedStepBar renders any non-active step: a compact bar with the marker,
// the step title, and — once complete — the summary of what was saved. A
// reachable step (already done, or earlier than the active one) is a button
// that re-expands it for editing; an unfinished upcoming step is an inert,
// dimmed row so the user can see the road ahead without skipping into it.
export function CollapsedStepBar({
  number,
  title,
  summary,
  complete,
  editable,
  onEdit,
}: {
  number: number
  title: string
  summary: string
  complete: boolean
  editable: boolean
  onEdit: () => void
}) {
  const inner = (
    <>
      <StepMarker number={number} complete={complete} />
      <span className="text-[13px] font-medium text-text-secondary">{title}</span>
      {complete && (
        <span className="truncate text-[12px] text-text-tertiary" title={summary}>
          {summary}
        </span>
      )}
      {editable && (
        <span className="ml-auto text-[11px] text-text-tertiary transition-colors group-hover:text-accent">
          Edit
        </span>
      )}
    </>
  )

  const base =
    'flex w-full items-center gap-2.5 rounded-xl border px-4 py-2.5 text-left transition-colors'

  if (editable) {
    return (
      <button
        type="button"
        onClick={onEdit}
        aria-label={`${title} — completed. Edit.`}
        className={`${base} group border-border-subtle bg-surface-raised/60 hover:border-accent/40 hover:bg-surface-raised`}
      >
        {inner}
      </button>
    )
  }
  return (
    <div
      aria-label={`${title} — not yet completed`}
      className={`${base} border-transparent bg-black/[0.02] opacity-55`}
    >
      {inner}
    </div>
  )
}
