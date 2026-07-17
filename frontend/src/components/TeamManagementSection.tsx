import { useCallback, useEffect, useState } from 'react'
import { Archive, Plus, RotateCcw, Users } from 'lucide-react'
import { useTeams } from '../hooks/useTeams'
import { fetchArchivedTeams, restoreTeam, type ArchivedTeam } from '../lib/teamLifecycle'
import { toast } from './Toast/toastStore'
import ArchiveTeamModal from './ArchiveTeamModal'

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
// Org page's Template tab (TFAC-436), not this section (a
// distinct surface was pinned so the template never reads as just another team).
// See pages/OrgTemplate.tsx.
export default function TeamManagementSection() {
  const { teams, createTeam, refresh: refreshTeams } = useTeams()
  const [name, setName] = useState('')
  const [creating, setCreating] = useState(false)

  // Archived-teams restore surface (TFAC-448): an org admin can list archived
  // teams and restore them. Lazy — the list is only fetched when revealed.
  const [showArchived, setShowArchived] = useState(false)
  const [archived, setArchived] = useState<ArchivedTeam[] | null>(null)
  const [archivedError, setArchivedError] = useState<string | null>(null)
  const [restoringId, setRestoringId] = useState<string | null>(null)

  // Which team's archive confirm is open, if any (ArchiveTeamModal,
  // reused here so the org-wide list gets the same destructive-confirm as the
  // per-team Settings danger zone).
  const [archiveTarget, setArchiveTarget] = useState<{ id: string; name: string } | null>(null)

  const loadArchived = useCallback(async () => {
    setArchivedError(null)
    try {
      setArchived(await fetchArchivedTeams())
    } catch (err) {
      setArchivedError(err instanceof Error ? err.message : 'Failed to load archived teams')
    }
  }, [])

  useEffect(() => {
    if (showArchived && archived === null) void loadArchived()
  }, [showArchived, archived, loadArchived])

  const restore = async (t: ArchivedTeam) => {
    if (restoringId) return
    setRestoringId(t.id)
    try {
      await restoreTeam(t.id)
      toast.success(`Restored team "${t.name}"`)
      setArchived((cur) => (cur ?? []).filter((x) => x.id !== t.id))
      void refreshTeams()
    } catch (err) {
      toast.error(err instanceof Error ? err.message : 'Failed to restore team')
    } finally {
      setRestoringId(null)
    }
  }

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
              <div className="flex items-center gap-3">
                <span className="text-[11px] text-text-tertiary">{t.slug}</span>
                <button
                  type="button"
                  onClick={() => setArchiveTarget({ id: t.id, name: t.name })}
                  className="inline-flex items-center gap-1 text-[12px] font-medium text-dismiss hover:text-dismiss/80"
                >
                  <Archive size={12} />
                  Archive…
                </button>
              </div>
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

      {/* Archived teams (TFAC-448): a reveal toggle + restore buttons. Archived
          teams are hidden from the normal team list, so this is the org-admin
          surface that brings them back. */}
      <div className="mt-4 border-t border-border-subtle pt-3">
        <button
          type="button"
          onClick={() => setShowArchived((v) => !v)}
          className="text-[12px] font-medium text-text-tertiary transition-colors hover:text-text-secondary"
        >
          {showArchived ? 'Hide archived teams' : 'Show archived teams'}
        </button>

        {showArchived && (
          <div className="mt-2">
            {archivedError ? (
              <p className="text-[12px] text-dismiss">
                {archivedError}{' '}
                <button type="button" onClick={() => void loadArchived()} className="underline">
                  Retry
                </button>
              </p>
            ) : archived === null ? (
              <p className="text-[12px] text-text-tertiary">Loading archived teams…</p>
            ) : archived.length === 0 ? (
              <p className="text-[12px] text-text-tertiary">No archived teams.</p>
            ) : (
              <ul className="space-y-1">
                {archived.map((t) => (
                  <li
                    key={t.id}
                    className="flex items-center justify-between rounded-lg border border-border-subtle bg-white/40 px-3 py-1.5 text-[13px]"
                  >
                    <span className="text-text-secondary">{t.name}</span>
                    <button
                      type="button"
                      onClick={() => void restore(t)}
                      disabled={restoringId === t.id}
                      className="inline-flex items-center gap-1 text-[12px] font-medium text-accent hover:text-accent/80 disabled:opacity-40"
                    >
                      <RotateCcw size={12} />
                      {restoringId === t.id ? 'Restoring…' : 'Restore'}
                    </button>
                  </li>
                ))}
              </ul>
            )}
          </div>
        )}
      </div>

      {archiveTarget && (
        <ArchiveTeamModal
          teamId={archiveTarget.id}
          teamName={archiveTarget.name}
          onClose={() => setArchiveTarget(null)}
          onDone={(runs, sessions) => {
            toast.success(
              `Team archived — stopped ${runs} ${runs === 1 ? 'delegation' : 'delegations'} and ${sessions} curator ${
                sessions === 1 ? 'session' : 'sessions'
              }.`,
            )
            setArchiveTarget(null)
            // Force a refetch next time the archived list is revealed (or right
            // away if it's already open) so the newly archived team shows up.
            setArchived(null)
            void refreshTeams()
          }}
        />
      )}
    </section>
  )
}
