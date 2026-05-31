import { useState, useRef, useEffect } from 'react'
import { Check, ChevronDown, Users } from 'lucide-react'
import { useTeams } from '../hooks/useTeams'

interface Props {
  /** The single active team id. */
  value: string
  onChange: (teamId: string) => void
  className?: string
}

// TeamSwitch is the single-team switcher for the write/config surfaces
// that show exactly one team at a time — the prompts page (which doubles
// as the auto-delegation binding graph). It is NOT the dashboard read
// filter (TeamScopeSelect, multi-select, "all" default) and NOT the
// create-modal write picker (TeamPicker) — it's a page-level "which team
// am I working in" toggle that drives both the page's reads and its
// writes. Renders nothing unless the viewer belongs to ≥2 teams (the same
// count gate as the other team controls), so solo + local users never see
// it and the page behaves exactly as it does today.
export default function TeamSwitch({ value, onChange, className = '' }: Props) {
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

  const current = teams.find((t) => t.id === value)
  const label = current?.name ?? 'Select team'

  return (
    <div ref={ref} className={`relative inline-flex items-center ${className}`}>
      <button
        type="button"
        onClick={() => setOpen((v) => !v)}
        className="
          inline-flex items-center gap-1.5 rounded-lg border border-border-subtle
          bg-white/60 px-2.5 py-1 text-[12px] text-text-primary
          hover:bg-white transition-colors
        "
        title="Active team — the team these prompts and triggers belong to"
      >
        <Users size={13} className="text-text-tertiary" />
        <span className="max-w-[12rem] truncate">{label}</span>
        <ChevronDown size={13} className="text-text-tertiary" />
      </button>

      {open && (
        <div
          className="
            absolute top-full left-0 z-30 mt-1 min-w-[14rem] rounded-lg border border-border-subtle
            bg-white shadow-lg py-1
          "
        >
          {teams.map((t) => {
            const isOn = t.id === value
            return (
              <button
                key={t.id}
                type="button"
                onClick={() => {
                  onChange(t.id)
                  setOpen(false)
                }}
                className="flex w-full items-center justify-between gap-2 px-3 py-1.5 text-left text-[12px] hover:bg-black/[0.03]"
              >
                <span className="text-text-primary truncate">{t.name}</span>
                {isOn && <Check size={13} className="shrink-0 text-accent" />}
              </button>
            )
          })}
        </div>
      )}
    </div>
  )
}
