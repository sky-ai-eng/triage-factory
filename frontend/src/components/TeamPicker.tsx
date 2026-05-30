import { useState, useRef, useEffect } from 'react'
import { Check, ChevronDown, Users } from 'lucide-react'
import { useTeams } from '../hooks/useTeams'

interface Props {
  /** The picked team id (required when shown). */
  value: string
  onChange: (teamId: string) => void
  label?: string
  className?: string
}

// TeamPicker is the write-time team picker: a *required* team
// selector with no "all" option — the choice is the acting team the
// backend stamps onto the new rule / prompt / project / task. Like the
// read filter it renders nothing below 2 teams (the modal silently
// targets the sole team, exactly as the resolver's sole-team fallback
// does), so callers can mount it unconditionally and let the count gate
// hide it.
//
// Controlled: the parent seeds the initial value via pickerDefault()
// (sticky default → most-recent → first team) and threads `value` into
// the submit body. When this renders null (≤1 team) the parent should
// submit team_id='' and let the server resolve the sole team.
export default function TeamPicker({ value, onChange, label = 'Team', className = '' }: Props) {
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

  const selected = teams.find((t) => t.id === value)

  return (
    <div className={className}>
      <label className="mb-1 block text-[11px] font-medium uppercase tracking-wide text-text-tertiary">
        {label}
      </label>
      <div ref={ref} className="relative">
        <button
          type="button"
          onClick={() => setOpen((v) => !v)}
          className="
            flex w-full items-center justify-between gap-2 rounded-lg border border-border-subtle
            bg-white/60 px-3 py-1.5 text-left text-[13px] text-text-primary
            hover:bg-white focus:border-accent focus:outline-none transition-colors
          "
        >
          <span className="inline-flex items-center gap-1.5 truncate">
            <Users size={13} className="text-text-tertiary" />
            <span className={selected ? '' : 'text-text-tertiary'}>
              {selected ? selected.name : 'Select a team…'}
            </span>
          </span>
          <ChevronDown size={14} className="shrink-0 text-text-tertiary" />
        </button>
        {open && (
          <div
            className="
              absolute top-full left-0 z-40 mt-1 max-h-56 w-full overflow-y-auto rounded-lg
              border border-border-subtle bg-white py-1 shadow-lg
            "
          >
            {teams.map((t) => (
              <button
                key={t.id}
                type="button"
                onClick={() => {
                  onChange(t.id)
                  setOpen(false)
                }}
                className="flex w-full items-center justify-between gap-2 px-3 py-1.5 text-left text-[13px] hover:bg-black/[0.03]"
              >
                <span className="truncate text-text-primary">{t.name}</span>
                {value === t.id && <Check size={13} className="shrink-0 text-accent" />}
              </button>
            ))}
          </div>
        )}
      </div>
    </div>
  )
}
