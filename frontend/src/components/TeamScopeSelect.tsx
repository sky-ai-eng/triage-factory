import { useState, useRef, useEffect } from 'react'
import { Check, ChevronDown, Users } from 'lucide-react'
import { useTeams } from '../hooks/useTeams'

interface Props {
  /** The selected team ids. Empty = "all my teams" (the union). */
  value: string[]
  onChange: (teamIds: string[]) => void
  className?: string
}

// TeamScopeSelect is the per-page read filter — a *multi-team* view
// scope. It renders nothing unless the viewer belongs to ≥2 teams (the
// same count gate that covers local + solo multi with no mode branch).
// Optional by design: the empty set is "all my teams" (the union);
// checking teams narrows the page to their union. The board can show
// {A}, {A,B}, all, or none — there's no single-team coupling, which is
// why the write picker (a single-team choice) is a separate control.
export default function TeamScopeSelect({ value, onChange, className = '' }: Props) {
  const { teams, multi } = useTeams()
  const [open, setOpen] = useState(false)
  const ref = useRef<HTMLDivElement>(null)

  useEffect(() => {
    if (!open) return
    const onDoc = (e: MouseEvent) => {
      if (ref.current && !ref.current.contains(e.target as Node)) setOpen(false)
    }
    document.addEventListener('mousedown', onDoc)
    return () => document.removeEventListener('mousedown', onDoc)
  }, [open])

  if (!multi) return null

  const selected = new Set(value)
  const chosen = teams.filter((t) => selected.has(t.id))
  const label =
    chosen.length === 0
      ? 'All my teams'
      : chosen.length === 1
        ? chosen[0].name
        : `${chosen.length} teams`

  const toggle = (id: string) => {
    const next = new Set(value)
    if (next.has(id)) {
      next.delete(id)
    } else {
      next.add(id)
    }
    // Preserve the teams' canonical order for a stable request shape.
    onChange(teams.filter((t) => next.has(t.id)).map((t) => t.id))
  }

  return (
    <div ref={ref} className={`relative inline-flex items-center ${className}`}>
      <button
        type="button"
        onClick={() => setOpen((v) => !v)}
        className="
          inline-flex items-center gap-1.5 rounded-lg border border-line-1
          bg-raised px-2.5 py-1 text-ui text-ink-1
          hover:bg-raised transition-colors
        "
        title="Filter by team"
      >
        <Users size={13} className="text-ink-3" />
        <span className="max-w-[12rem] truncate">{label}</span>
        <ChevronDown size={13} className="text-ink-3" />
      </button>

      {open && (
        <div
          className="
            absolute top-full left-0 z-30 mt-1 min-w-[14rem] rounded-lg border border-line-1
            bg-raised shadow-float py-1
          "
        >
          <button
            type="button"
            onClick={() => {
              onChange([])
              setOpen(false)
            }}
            className="flex w-full items-center justify-between gap-2 px-3 py-1.5 text-left text-ui hover:bg-tint-2"
          >
            <span className="flex flex-col">
              <span className="text-ink-1">All my teams</span>
              <span className="text-label text-ink-3">union of every team you&rsquo;re on</span>
            </span>
            {value.length === 0 && <Check size={13} className="shrink-0 text-warm" />}
          </button>
          <div className="my-1 h-px bg-line-1" />
          {teams.map((t) => {
            const isOn = selected.has(t.id)
            return (
              <button
                key={t.id}
                type="button"
                onClick={() => toggle(t.id)}
                className="flex w-full items-center justify-between gap-2 px-3 py-1.5 text-left text-ui hover:bg-tint-2"
              >
                <span className="flex items-center gap-2">
                  <span
                    className={`flex h-3.5 w-3.5 items-center justify-center rounded border ${
                      isOn ? 'border-warm bg-warm text-warm-ink' : 'border-line-1'
                    }`}
                  >
                    {isOn && <Check size={10} />}
                  </span>
                  <span className="text-ink-1 truncate">{t.name}</span>
                </span>
              </button>
            )
          })}
        </div>
      )}
    </div>
  )
}
