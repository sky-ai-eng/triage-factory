import { useCallback, useEffect, useState } from 'react'
import { Archive, Plus, RotateCcw, Users } from 'lucide-react'
import { useTeams } from '../hooks/useTeams'
import {
  fetchArchivedTeams,
  fetchArchivePreview,
  restoreTeam,
  type ArchivedTeam,
  type ArchivePreview,
} from '../lib/teamLifecycle'
import { toast } from './Toast/toastStore'
import ArchiveTeamModal from './ArchiveTeamModal'

// TeamManagementSection is the org-admin "add team" affordance.
// It lives in Settings (org admin), NOT in the scope dropdowns — it's how
// a solo multi-mode user grows past one team, at which point the count-gated
// selectors begin rendering. Multi-only: the caller gates it on
// multi-mode + org-admin, so it never renders in local mode.
//
// Deliberately minimal — just enough to exercise the ≥2-team path
// end-to-end (the issue defers richer team creation/management to its own
// ticket). It lists the org's teams (so the admin sees what exists) and
// takes a name to create another.
//
// A new team's prompts + handlers seed directly from TF's shipped defaults
// (ai.ShippedPrompts() / ai.ShippedBlueprints() / db.ShippedEventHandlers) —
// there is no per-org editable source for this section to point at.
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
  // per-team Settings danger zone). The consequences preview is fetched
  // BEFORE the dialog opens — what opens is already whole, and a preview that
  // cannot be read withdraws the question instead of opening it empty.
  const [archiveTarget, setArchiveTarget] = useState<{
    id: string
    name: string
    preview: ArchivePreview
  } | null>(null)
  const [archiveReading, setArchiveReading] = useState<string | null>(null)

  const openArchive = useCallback(
    async (t: { id: string; name: string }) => {
      if (archiveReading) return
      setArchiveReading(t.id)
      try {
        const preview = await fetchArchivePreview(t.id)
        setArchiveTarget({ id: t.id, name: t.name, preview })
      } catch {
        toast.error('Could not check the team for in-flight work. Try again.')
      } finally {
        setArchiveReading(null)
      }
    },
    [archiveReading],
  )

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
    <section className="rounded-2xl border border-line-1 bg-raised p-5">
      <div className="mb-3 flex items-center gap-2">
        <Users size={15} className="text-ink-3" />
        <h3 className="text-body font-semibold text-ink-1">Teams</h3>
      </div>
      <p className="mb-3 text-ui text-ink-3 leading-relaxed">
        Add a team to split work across groups. Once you belong to more than one team, a team
        selector appears on the factory, board, and queue, and the write modals ask which team new
        work belongs to.
      </p>

      {teams.length > 0 && (
        <ul className="mb-4 space-y-1">
          {teams.map((t) => (
            <li
              key={t.id}
              className="flex items-center justify-between rounded-lg border border-line-1 bg-raised px-3 py-1.5 text-body"
            >
              <span className="text-ink-1">{t.name}</span>
              <div className="flex items-center gap-3">
                <span className="text-reported text-ink-3">{t.slug}</span>
                <button
                  type="button"
                  onClick={() => void openArchive(t)}
                  disabled={archiveReading !== null}
                  className="inline-flex items-center gap-1 text-ui font-medium text-alarm hover:text-alarm/80 disabled:opacity-50"
                >
                  <Archive size={12} />
                  {archiveReading === t.id ? 'Checking…' : 'Archive…'}
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
            flex-1 rounded-lg border border-line-1 bg-raised px-3 py-1.5
            text-body text-ink-1 placeholder:text-ink-3
            focus:border-warm focus:bg-raised focus:outline-none
          "
        />
        <button
          type="submit"
          disabled={!name.trim() || creating}
          className="
            inline-flex items-center gap-1.5 rounded-lg bg-warm px-3 py-1.5 text-body
            font-medium text-warm-ink hover:bg-warm/90 disabled:opacity-40 transition-colors
          "
        >
          <Plus size={14} />
          {creating ? 'Adding…' : 'Add team'}
        </button>
      </form>

      {/* Archived teams (TFAC-448): a reveal toggle + restore buttons. Archived
          teams are hidden from the normal team list, so this is the org-admin
          surface that brings them back. */}
      <div className="mt-4 border-t border-line-1 pt-3">
        <button
          type="button"
          onClick={() => setShowArchived((v) => !v)}
          className="text-ui font-medium text-ink-3 transition-colors hover:text-ink-2"
        >
          {showArchived ? 'Hide archived teams' : 'Show archived teams'}
        </button>

        {showArchived && (
          <div className="mt-2">
            {archivedError ? (
              <p className="text-ui text-alarm">
                {archivedError}{' '}
                <button type="button" onClick={() => void loadArchived()} className="underline">
                  Retry
                </button>
              </p>
            ) : archived === null ? (
              <p className="text-ui text-ink-3">Loading archived teams…</p>
            ) : archived.length === 0 ? (
              <p className="text-ui text-ink-3">No archived teams.</p>
            ) : (
              <ul className="space-y-1">
                {archived.map((t) => (
                  <li
                    key={t.id}
                    className="flex items-center justify-between rounded-lg border border-line-1 bg-raised px-3 py-1.5 text-body"
                  >
                    <span className="text-ink-2">{t.name}</span>
                    <button
                      type="button"
                      onClick={() => void restore(t)}
                      disabled={restoringId === t.id}
                      className="inline-flex items-center gap-1 text-ui font-medium text-warm hover:text-warm/80 disabled:opacity-40"
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
          preview={archiveTarget.preview}
          onClose={() => setArchiveTarget(null)}
          onDone={(runs) => {
            toast.success(
              `Team archived — stopped ${runs} ${runs === 1 ? 'delegation' : 'delegations'}.`,
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
