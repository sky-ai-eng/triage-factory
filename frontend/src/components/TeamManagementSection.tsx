import { useState } from 'react'
import { Plus, Users } from 'lucide-react'
import { useTeams } from '../hooks/useTeams'
import { toast } from './Toast/toastStore'

// TeamManagementSection is the org-admin "add team" affordance.
// It lives in Settings (org admin), NOT in the scope dropdowns — it's how
// a solo hosted user grows past one team, at which point the count-gated
// selectors begin rendering. Hosted-only: the caller gates it on
// multi-mode + org-admin, so it never renders in local mode.
//
// Deliberately minimal — just enough to exercise the ≥2-team path
// end-to-end (the issue defers richer team creation/management to its own
// ticket). It lists the org's teams (so the admin sees what exists) and
// takes a name to create another.
//
// The defaults a new team inherits (prompts + handlers) are edited from the
// dedicated "Org template" route — its own org-admin nav entry, not this
// section (SKY-381 pinned a distinct surface so the template never reads as
// just another team). See pages/OrgTemplate.tsx.
export default function TeamManagementSection() {
  const { teams, createTeam } = useTeams()
  const [name, setName] = useState('')
  const [creating, setCreating] = useState(false)

  const submit = async (e: React.FormEvent) => {
    e.preventDefault()
    const trimmed = name.trim()
    if (!trimmed || creating) return
    setCreating(true)
    try {
      const created = await createTeam(trimmed)
      toast.success(`Created team "${created.name}"`)
      setName('')
    } catch (err) {
      toast.error(err instanceof Error ? err.message : 'Failed to create team')
    } finally {
      setCreating(false)
    }
  }

  return (
    <section className="rounded-2xl border border-border-subtle bg-white/40 p-5">
      <div className="mb-3 flex items-center gap-2">
        <Users size={15} className="text-text-tertiary" />
        <h3 className="text-[14px] font-semibold text-text-primary">Teams</h3>
      </div>
      <p className="mb-3 text-[12px] text-text-tertiary leading-relaxed">
        Add a team to split work across groups. Once you belong to more than one team, a team
        selector appears on the factory, board, and queue, and the write modals ask which team new
        work belongs to.
      </p>

      {teams.length > 0 && (
        <ul className="mb-4 space-y-1">
          {teams.map((t) => (
            <li
              key={t.id}
              className="flex items-center justify-between rounded-lg border border-border-subtle bg-white/50 px-3 py-1.5 text-[13px]"
            >
              <span className="text-text-primary">{t.name}</span>
              <span className="text-[11px] text-text-tertiary">{t.slug}</span>
            </li>
          ))}
        </ul>
      )}

      <form onSubmit={submit} className="flex items-center gap-2">
        <input
          type="text"
          value={name}
          onChange={(e) => setName(e.target.value)}
          placeholder="New team name"
          className="
            flex-1 rounded-lg border border-border-subtle bg-white/60 px-3 py-1.5
            text-[13px] text-text-primary placeholder:text-text-tertiary
            focus:border-accent focus:bg-white focus:outline-none
          "
        />
        <button
          type="submit"
          disabled={!name.trim() || creating}
          className="
            inline-flex items-center gap-1.5 rounded-lg bg-accent px-3 py-1.5 text-[13px]
            font-medium text-white hover:bg-accent/90 disabled:opacity-40 transition-colors
          "
        >
          <Plus size={14} />
          {creating ? 'Adding…' : 'Add team'}
        </button>
      </form>
    </section>
  )
}
