import { useCallback, useEffect, useState } from 'react'
import { SourceFrame } from './SourceFrame'
import type { SourceBodyProps } from './SourceFrame'
import LinearTeamRulesGroup from '../settings/LinearTeamRulesGroup'
import {
  fetchTeamSettings,
  linearDraftAfterSave,
  linearTeamFromWire,
  linearTeamsBlocked,
  linearTeamsEqual,
  saveTeamLinearTeams,
} from '../settings/teamConfig'
import type { LinearTeamConfig } from '../settings/teamConfig'
import { useActiveOrgId } from '../../contexts/OrgContext'
import { useTeamActivity, activitySource, sinceLabel } from '../../hooks/useTeamActivity'
import { useEventSources } from '../../hooks/useEventSources'
import { sourceUnavailableReason } from '../../lib/eventSources'
import { toast } from '../../components/Toast/toastStore'

// Linear, as this team's event source.
//
// The editor is the one the local Settings page uses, so watching, the
// pre-fill from state types and the half-mapping check behave the same in
// both places. Unlike Jira's board it does not save as each gesture lands:
// the server stores a Linear team either unmapped or mapped in full, and a
// mapping is built a rule at a time, so the edits are held until Save.
//
// A member sees what is watched and how it is mapped, with the verbs absent.
//
// What is loaded and what is edited are each tagged with the team they belong
// to, and read only while that is still the page's team. The write is a
// replace-set, so a set held over a team switch would overwrite the new
// team's with the old one's.

const PROSE =
  'Watched Linear teams send events this team can automate, like issues being assigned to team ' +
  'members or moving between states. Map each team’s workflow states to pickup, in progress and ' +
  'done so Triage Factory knows where an issue stands. Changes do not apply to runs already in-flight.'

/** A set of Linear teams, and the team it belongs to. */
type TeamSet = { teamId: string; teams: LinearTeamConfig[] }

export default function LinearSource({ teamId, teamName, isAdmin, onBack }: SourceBodyProps) {
  const orgId = useActiveOrgId()
  const [loaded, setLoaded] = useState<TeamSet | null>(null)
  const [edited, setEdited] = useState<TeamSet | null>(null)
  const [failedFor, setFailedFor] = useState<string | null>(null)
  const [saving, setSaving] = useState(false)

  const baseline = loaded?.teamId === teamId ? loaded.teams : null
  const draft = edited?.teamId === teamId ? edited.teams : (baseline ?? [])
  const setDraft = (teams: LinearTeamConfig[]) => setEdited({ teamId, teams })
  const error = failedFor === teamId ? 'Could not read this team’s Linear configuration.' : ''

  const flow = activitySource(useTeamActivity(teamId, 7), 'linear')

  // Why Linear cannot reach this org right now, or null. A paused source keeps
  // its credential, so its catalog still reads and its mapping stays editable.
  // An unconnected one has no catalog or workflow to read: the editor shows
  // what is stored and says so, and a team can still be unwatched, which
  // never asks Linear anything.
  const { stateOf } = useEventSources()
  const state = stateOf('linear')
  const offReason = sourceUnavailableReason('linear', state)
  const connected = state === 'available' || state === 'disabled'

  useEffect(() => {
    if (!teamId) return
    let live = true
    void fetchTeamSettings(teamId).then((data) => {
      if (!live) return
      if (!data) {
        setFailedFor(teamId)
        return
      }
      setLoaded({ teamId, teams: (data.linear_teams ?? []).map(linearTeamFromWire) })
    })
    return () => {
      live = false
    }
  }, [teamId])

  const dirty = baseline !== null && !linearTeamsEqual(draft, baseline)
  const blocked = linearTeamsBlocked(draft)

  const save = async () => {
    if (baseline === null) return
    const savedFor = teamId
    const sent = draft
    setSaving(true)
    try {
      const res = await saveTeamLinearTeams(savedFor, sent)
      if (!res.ok) {
        toast.error(res.error)
        return
      }
      // The stored set, with each team's key and name and each state's name
      // and type as Linear gave them on the way in — unless the editor moved
      // on while the request was out, in which case those edits stay, unsaved.
      // A page that has since moved to another team is left alone.
      const forSaved = (cur: TeamSet | null) => !cur || cur.teamId === savedFor
      setLoaded((cur) => (forSaved(cur) ? { teamId: savedFor, teams: res.teams } : cur))
      setEdited((cur) =>
        forSaved(cur)
          ? { teamId: savedFor, teams: linearDraftAfterSave(cur?.teams ?? sent, sent, res.teams) }
          : cur,
      )
      toast.success('Linear teams saved')
    } finally {
      setSaving(false)
    }
  }

  const back = useCallback(() => {
    if (dirty && !window.confirm('Discard your unsaved Linear changes?')) return
    onBack()
  }, [dirty, onBack])

  return (
    <SourceFrame
      source="linear"
      name="Linear"
      teamName={teamName}
      onBack={back}
      events={flow?.events ?? null}
      tasks={flow ? flow.tasks : null}
      sincePoll={sinceLabel(flow?.last_poll_at ?? null)}
    >
      <div className="sp-cols">
        <div className="sp-left">
          <p className="sp-prose">{PROSE}</p>
          {offReason ? <p className="sp-offnote">{offReason}</p> : null}
        </div>

        <div className="sp-linear-right">
          {error ? <p className="sp-error">{error}</p> : null}
          {baseline === null || !orgId ? (
            error ? null : (
              <p className="sp-prose">Loading…</p>
            )
          ) : (
            <>
              <LinearTeamRulesGroup
                orgId={orgId}
                value={draft}
                onChange={setDraft}
                connected={connected}
                readOnly={!isAdmin}
                bare
              />
              {isAdmin && (
                <div className="mt-4 flex items-center justify-end gap-2">
                  {blocked && (
                    <span className="mr-auto text-reported text-alarm">
                      Finish or clear a partly mapped team to save.
                    </span>
                  )}
                  <button
                    type="button"
                    onClick={() => setEdited(null)}
                    disabled={!dirty || saving}
                    className="text-reported text-ink-2 hover:text-ink-1 disabled:opacity-40 rounded-xl px-3 py-1 transition-colors"
                  >
                    Discard
                  </button>
                  <button
                    type="button"
                    onClick={() => void save()}
                    disabled={!dirty || blocked || saving}
                    className="text-reported text-warm hover:text-warm/80 disabled:opacity-40 border border-warm/20 rounded-xl px-3 py-1 transition-colors"
                  >
                    {saving ? 'Saving…' : 'Save'}
                  </button>
                </div>
              )}
            </>
          )}
        </div>
      </div>
    </SourceFrame>
  )
}
