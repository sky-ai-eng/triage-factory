import { useCallback, useMemo, useState } from 'react'
import { useSearchParams } from 'react-router'
import Table from '../../ui/table/Table'
import type { TableRow } from '../../ui/table/Table'
import SourceCard from '../../ui/sourcecard/SourceCard'
import StatusRules from '../../ui/statusrules/StatusRules'
import Dialog from '../../ui/dialog/Dialog'
import { usePageHeader } from '../../contexts/ChromeContext'
import { useTeams } from '../../hooks/useTeams'
import { useTeamRole } from '../../hooks/useTeamRole'
import { useMemberRoster, displayNameFor } from '../../hooks/useMemberRoster'
import type { MemberRosterAdapter, RosterMember } from '../../hooks/useMemberRoster'
import { apiFetch, apiJSON } from '../../lib/apiClient'
import './team-settings.css'

// Team settings — one route, four views.
//
// The member view is a STRICT SUBSET of the same page, never a different page.
// Nothing is greyed out: a control a member may never use is absent, because a
// verb that 403s is not information. The gate is `team.role === 'admin'` and
// nothing else — an org admin is deliberately not a team admin here.
//
// What is NOT wired, and why it reads as `—` rather than as a number: the
// header figures, the 14-day series behind the chart, and live task arrivals
// all need aggregations the API does not have yet (backend-needs.md items
// 17-19). A zero there would be a claim about a team's week; a dash says the
// question has not been answered. Same rule the rail follows for its counts.

type View = 'overview' | 'github' | 'jira' | 'slack'

const VIEWS: View[] = ['overview', 'github', 'jira', 'slack']

type TeamRosterApiResponse = {
  members: Array<{
    user_id: string
    display_name: string
    github_username: string | null
    jira_account_id: string | null
    role: string
    is_current_user: boolean
  }>
}

const TEAM_ROLES = ['admin', 'member', 'viewer']
const ROLE_LABELS: Record<string, string> = {
  admin: 'Team admin',
  member: 'Member',
  viewer: 'Viewer — read only',
}

/** A figure the API cannot answer yet. Absent, never zero. */
function Figure({ label, value }: { label: string; value?: number | string | null }) {
  return (
    <div className="ts-fig">
      <span className="ts-fig-v">{value == null ? '—' : value}</span>
      <span className="ts-fig-l">{label}</span>
    </div>
  )
}

export default function TeamSettings() {
  const [params, setParams] = useSearchParams()
  const raw = params.get('source')
  const view: View = VIEWS.includes(raw as View) ? (raw as View) : 'overview'
  const go = useCallback(
    (v: View) => {
      const next = new URLSearchParams(params)
      if (v === 'overview') next.delete('source')
      else next.set('source', v)
      setParams(next, { replace: false })
    },
    [params, setParams],
  )

  const { teams, lastActingTeamId, loaded } = useTeams()
  // The team the viewer is actually acting as, not whichever came back first.
  // `teams[0]` is arbitrary once someone belongs to more than one.
  const team = loaded ? (teams.find((t) => t.id === lastActingTeamId) ?? teams[0] ?? null) : null
  const teamId = team?.id ?? ''
  const { roleForTeam } = useTeamRole()
  // An org admin is NOT a team admin here. The gate is the viewer's own
  // membership row and nothing else.
  const isAdmin = roleForTeam(teamId) === 'admin'

  const [deleting, setDeleting] = useState(false)

  const adapter = useMemo<MemberRosterAdapter>(
    () => ({
      roles: TEAM_ROLES,
      protectedRole: 'admin',
      roleLabels: ROLE_LABELS,
      async fetchMembers(): Promise<RosterMember[]> {
        // Never build the URL without an id. `/api/teams//members` matches no
        // route, so it 404s as "endpoint not found" — which reads as a missing
        // API rather than as a request that should not have been sent.
        if (!teamId) return []
        const data = await apiJSON<TeamRosterApiResponse>(`/api/teams/${teamId}/members`)
        return data.members.map((m) => ({
          userId: m.user_id,
          displayName: m.display_name,
          githubUsername: m.github_username,
          jiraAccountId: m.jira_account_id,
          role: m.role,
          isCurrentUser: m.is_current_user,
        }))
      },
      async changeRole(userId, role) {
        await apiFetch(`/api/teams/${teamId}/members/${userId}`, {
          method: 'PATCH',
          headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify({ role }),
        })
      },
      async remove(userId) {
        await apiFetch(`/api/teams/${teamId}/members/${userId}`, { method: 'DELETE' })
      },
    }),
    [teamId],
  )

  const { members, loading, error } = useMemberRoster(adapter)

  const header = useMemo(
    () => ({
      crumbs: view === 'overview' ? undefined : [{ name: 'Team', onClick: () => go('overview') }],
      title: view === 'overview' ? (team?.name ?? 'Team') : view[0].toUpperCase() + view.slice(1),
      subtitle:
        view === 'overview'
          ? 'who is on this team, what they may run, and where its work comes from'
          : undefined,
    }),
    [view, team?.name, go],
  )
  usePageHeader(header)

  const rows: TableRow[] = members.map((m) => ({
    id: m.userId,
    name: displayNameFor(m),
    github: m.githubUsername ?? '—',
    role: ROLE_LABELS[m.role] ?? m.role,
    you: m.isCurrentUser,
  }))

  if (!team) {
    return <p className="ts-empty">No team resolved for this org yet.</p>
  }

  return (
    <div className="ts">
      {/* The header carries the team's headline figures. They are dashes until
          the aggregations exist — see the note at the top of this file. */}
      <div className="ts-band">
        <Figure label="events · 7d" />
        <Figure label="became tasks" />
        <Figure label="spend · 14d" />
      </div>

      {view === 'overview' ? (
        <>
          <section className="ts-sec">
            <h2 className="ts-h">ROSTER</h2>
            {error ? (
              <p className="ts-error">{error}</p>
            ) : (
              <Table
                build
                label={loading ? 'MEMBERS' : 'MEMBERS · ' + rows.length}
                columns={[
                  { key: 'name', label: 'NAME', type: 'identity' },
                  { key: 'github', label: 'GITHUB', color: () => 'var(--color-ink-3)', drop: 1 },
                  { key: 'role', label: 'ROLE', align: 'end' },
                ]}
                rows={rows}
                pageSize={8}
                sortKey="name"
                barPosition="absolute"
                // Selection exists for the verbs, so it goes with them: a member
                // sees no row checkboxes and no bar at all.
                selectable={isAdmin}
                bar={
                  isAdmin
                    ? {
                        picker: {
                          label: 'Role',
                          options: TEAM_ROLES.map((r) => ({ id: r, name: ROLE_LABELS[r] })),
                          action: {
                            id: 'role',
                            label: 'role',
                            message: (n, o) =>
                              n + (n === 1 ? ' member is' : ' members are') + ' now ' + o?.name,
                          },
                        },
                        danger: {
                          label: 'Hold to remove',
                          action: {
                            id: 'remove',
                            label: 'remove',
                            message: (n) => n + (n === 1 ? ' member' : ' members') + ' removed',
                          },
                        },
                      }
                    : null
                }
                mutate={(row, id, pick) => {
                  if (id === 'remove') return null
                  return { ...row, role: pick ? pick.name : row.role }
                }}
                onCommit={(id, ids) => {
                  const list = ids.map(String)
                  if (id === 'remove') list.forEach((u) => void adapter.remove(u))
                }}
              />
            )}
          </section>

          <section className="ts-sec">
            <h2 className="ts-h">CONFIGURED MODELS</h2>
            <p className="ts-note">
              What this team may run. Model configuration is org-level today, so this section reads
              rather than writes — the per-team override is not built.
            </p>
          </section>

          <section className="ts-sec">
            <h2 className="ts-h">EVENT SOURCES</h2>
            <div className="ts-sources">
              <SourceCard
                name="GitHub"
                source="github"
                state="configured"
                scope="repositories this team tracks"
                stats={[
                  ['events · 7d', '—'],
                  ['became tasks', '—'],
                ]}
                onClick={() => go('github')}
              />
              <SourceCard
                name="Jira"
                source="jira"
                state="configured"
                scope="projects this team watches"
                stats={[
                  ['events · 7d', '—'],
                  ['became tasks', '—'],
                ]}
                onClick={() => go('jira')}
              />
              <SourceCard
                name="Slack"
                source="slack"
                state="configured"
                scope="channels this team watches"
                stats={[['mentions · 7d', '—']]}
                onClick={() => go('slack')}
              />
              {/* Drawn as coming soon deliberately — neither is a connection
                  yet, so neither has a colour to wear. */}
              <SourceCard
                name="Schedule"
                state="soon"
                scope="coming soon"
                note="Runs the factory on a cadence you set, with no event to trigger it."
              />
              <SourceCard
                name="Linear"
                state="soon"
                scope="coming soon"
                note="Issues and cycles, the way GitHub and Jira are wired today."
              />
            </div>
          </section>

          {/* Absent for a member, not disabled. */}
          {isAdmin && (
            <section className="ts-sec">
              <h2 className="ts-h">DANGER</h2>
              <div className="ts-danger">
                <div>
                  <div className="ts-danger-h">Delete this team</div>
                  <p className="ts-note">
                    The team, its tracked sources and its rules go. Tasks it already owns do not —
                    they are unassigned and stay in the org.
                  </p>
                </div>
                <button type="button" className="ts-danger-btn" onClick={() => setDeleting(true)}>
                  Delete team
                </button>
              </div>
            </section>
          )}
        </>
      ) : null}

      {view === 'github' ? (
        <section className="ts-sec">
          <h2 className="ts-h">WHAT THE REPOSITORIES SENT</h2>
          <p className="ts-note">
            The 14-day series behind this is the one new aggregation the page needs
            (backend-needs.md item 18). Until it exists there is no chart to draw, and an invented
            one would be worse than none.
          </p>
        </section>
      ) : null}

      {view === 'jira' ? (
        <section className="ts-sec">
          <h2 className="ts-h">STATUS RULES</h2>
          <p className="ts-note">
            Every status the connection reports lives in one of four columns or in the tray. Drag a
            status between columns, or to the tray to unmap it.
          </p>
          {/* A member may read the mapping but not change it — the board loses
              its drag, its staging and its grab cursor together. Nothing is
              greyed out: "where does our work come from" is information a
              member legitimately needs. */}
          <StatusRules interactive={isAdmin} showProjects={false} build />
        </section>
      ) : null}

      {view === 'slack' ? (
        <section className="ts-sec">
          <h2 className="ts-h">WHAT THE CHANNELS SENT</h2>
          <p className="ts-note">
            Per-channel mention counts need backend-needs.md item 16. The channel list itself is the
            existing panel until this view is wired to it.
          </p>
        </section>
      ) : null}

      <Dialog
        open={deleting}
        kind="destructive"
        title={'Delete ' + team.name + '?'}
        body="This cannot be undone."
        consequences={[
          { text: 'The team, its tracked repositories and its rules are removed', tone: 'loss' },
          { text: 'Tasks it owns are unassigned, not deleted', tone: 'keep' },
          { text: 'Members keep their organization accounts', tone: 'keep' },
        ]}
        confirmLabel="Delete team"
        cancelLabel="Keep it"
        onCancel={() => setDeleting(false)}
        onConfirm={() => setDeleting(false)}
      />
    </div>
  )
}
