import { useCallback, useMemo, useState } from 'react'
import { useNavigate } from 'react-router'
import Table from '../../ui/table/Table'
import type { TableRow } from '../../ui/table/Table'
import SourceCard from '../../ui/sourcecard/SourceCard'
import { usePageHeader } from '../../contexts/ChromeContext'
import { useOrgHref } from '../../hooks/useOrgHref'
import { useTeams } from '../../hooks/useTeams'
import { useTeamRole } from '../../hooks/useTeamRole'
import { useMemberRoster, displayNameFor } from '../../hooks/useMemberRoster'
import type { MemberRosterAdapter, RosterMember } from '../../hooks/useMemberRoster'
import { apiFetch, apiJSON } from '../../lib/apiClient'
import './team-settings.css'

// Team settings.
//
// The overview is a title block, a two-panel band, the roster, and the danger
// card. THE BAND IS TWO CONTROLS: clicking Configured Models or Event Sources
// opens that panel in the region the roster occupies, rather than stacking
// another section underneath. That is why the source cards are not on the page
// by default — they are what Event Sources opens into, and a page that shows
// them unprompted has answered a question nobody asked.
//
// The member view is a strict subset of the same page, never a different page.
// Nothing is greyed out: a control a member may never use is absent, because a
// verb that 403s is not information. The gate is `team.role === 'admin'` and
// nothing else — an org admin is deliberately not a team admin here.
//
// What reads `—`: every figure needing an aggregation the API does not have
// (backend-needs.md 17-19). A zero would be a claim about a team's week. The
// member count is real, because the roster is.

type Region = 'roster' | 'models' | 'sources'

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

/** What this deployment can run. Org-level today; the per-team override is not built. */
const MODELS = [
  { name: 'Claude Opus 5', tag: '(default)', price: '$25 / M' },
  { name: 'Claude Sonnet 5', tag: '', price: '$15 / M' },
  { name: 'Claude Haiku 4.5', tag: '', price: '$4 / M' },
]

const SOURCES = ['GitHub', 'Jira', 'Slack']

/** A headline figure. `null` reads as a dash — the question has not been answered. */
function Figure({
  value,
  label,
  tone,
  onClick,
}: {
  value: number | string | null
  label: string
  tone?: 'warm'
  onClick?: () => void
}) {
  const body = (
    <>
      <span className="ts-fig-v" data-tone={tone}>
        {value ?? '—'}
        {onClick ? <i className="ts-chev" aria-hidden="true" /> : null}
      </span>
      <span className="ts-fig-l">{label}</span>
    </>
  )
  if (!onClick) return <div className="ts-fig">{body}</div>
  return (
    <button type="button" className="ts-fig ts-fig-go" onClick={onClick}>
      {body}
    </button>
  )
}

/** A hatched track with a fill. No data means no fill — the track keeps the row's height. */
function Meter({ frac }: { frac: number | null }) {
  return (
    <span className="ts-meter">
      {frac == null ? null : <span className="ts-meter-fill" style={{ width: frac * 100 + '%' }} />}
    </span>
  )
}

export default function TeamSettings() {
  const navigate = useNavigate()
  const orgHref = useOrgHref()
  const [region, setRegion] = useState<Region>('roster')

  const { teams, lastActingTeamId, loaded } = useTeams()
  // The team the viewer is acting as, not whichever came back first.
  const team = loaded ? (teams.find((t) => t.id === lastActingTeamId) ?? teams[0] ?? null) : null
  const teamId = team?.id ?? ''
  const { roleForTeam } = useTeamRole()
  const isAdmin = roleForTeam(teamId) === 'admin'

  const adapter = useMemo<MemberRosterAdapter>(
    () => ({
      roles: TEAM_ROLES,
      protectedRole: 'admin',
      roleLabels: ROLE_LABELS,
      async fetchMembers(): Promise<RosterMember[]> {
        // Never build the URL without an id: `/api/teams//members` matches no
        // route, and comes back as "endpoint not found".
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

  // The page owns its own title block, so the shell's band carries the path
  // rather than repeating the name at a second size.
  const header = useMemo(
    () => ({ crumbs: [{ name: 'Team' }], title: team?.name ?? 'Team' }),
    [team?.name],
  )
  usePageHeader(header)

  const openRegion = useCallback((r: Region) => setRegion((cur) => (cur === r ? 'roster' : r)), [])

  const rows: TableRow[] = members.map((m) => ({
    id: m.userId,
    name: displayNameFor(m),
    github: m.githubUsername ?? '—',
    role: m.role,
    // Every measurement below needs an aggregation the API does not have.
    repos: '—',
    tasks: '—',
    spend: '—',
    seen: '—',
  }))

  if (!team) {
    return <p className="ts-empty">{loaded ? 'No team resolved for this org.' : 'Loading…'}</p>
  }

  return (
    <div className="ts">
      {/* The team's own title block. The name is 24px here because this is
          where you configure it; the shell's band only says where you are. */}
      <div className="ts-top">
        <div className="ts-id">
          <h1 className="ts-name">{team.name}</h1>
          <span className="ts-created">this team</span>
        </div>
        <div className="ts-figs">
          <Figure value={loading ? null : members.length} label="members" />
          {isAdmin ? (
            <>
              <span className="ts-div" />
              <Figure
                value={null}
                label="spent · 14 days"
                onClick={() => navigate(orgHref('/usage'))}
              />
            </>
          ) : null}
          <span className="ts-div" />
          <Figure value={null} label="tasks · 7 days" tone="warm" />
        </div>
      </div>

      {/* The band. Two controls, not two headings: each opens its own panel in
          the region below rather than adding a section to the page. */}
      <div className="ts-band">
        <button
          type="button"
          className="ts-panel"
          aria-expanded={region === 'models'}
          onClick={() => openRegion('models')}
        >
          <span className="ts-panel-head">
            <span className="ts-panel-t">CONFIGURED MODELS</span>
            <span className="ts-lead" />
            <span className="ts-panel-n">{MODELS.length} models</span>
            <i className="ts-chev" aria-hidden="true" />
          </span>
          <span className="ts-rows">
            {MODELS.map((m) => (
              <span className="ts-row" key={m.name}>
                <span className="ts-row-line">
                  <span className="ts-row-n">{m.name}</span>
                  {m.tag ? <span className="ts-row-tag">{m.tag}</span> : null}
                  <span className="ts-lead-flex" />
                  <span className="ts-row-v">{m.price}</span>
                </span>
                {/* No share-of-runs aggregation, so the track holds its height
                    and draws no fill rather than inventing a proportion. */}
                <Meter frac={null} />
              </span>
            ))}
          </span>
        </button>

        <button
          type="button"
          className="ts-panel ts-panel-right"
          aria-expanded={region === 'sources'}
          onClick={() => openRegion('sources')}
        >
          <span className="ts-panel-head">
            <span className="ts-panel-t">EVENT SOURCES</span>
            <span className="ts-lead" />
            <span className="ts-panel-n">— events</span>
            <i className="ts-chev" aria-hidden="true" />
          </span>
          <span className="ts-flow">
            <span className="ts-flow-list">
              {SOURCES.map((s) => (
                <span className="ts-row" key={s}>
                  <span className="ts-row-line">
                    <span className="ts-row-n">{s}</span>
                    <span className="ts-lead-flex" />
                    <span className="ts-row-v">—</span>
                  </span>
                  <Meter frac={null} />
                </span>
              ))}
            </span>
            {/* Three sources converging on one stream. Structural rather than
                plotted — there is no series behind it yet. */}
            <svg
              className="ts-flow-svg"
              viewBox="0 0 140 118"
              preserveAspectRatio="none"
              aria-hidden="true"
            >
              <path d="M8,14 C68,14 78,59 136,59" />
              <path d="M8,59 L136,59" opacity="0.6" />
              <path d="M8,104 C68,104 78,59 136,59" opacity="0.42" />
            </svg>
            <span className="ts-flow-stats">
              <span className="ts-stat">
                <span className="ts-stat-v">—</span>
                <span className="ts-stat-l">events</span>
              </span>
              <span className="ts-stat">
                <span className="ts-stat-v" data-tone="warm">
                  —
                </span>
                <span className="ts-stat-l">became tasks</span>
              </span>
            </span>
          </span>
        </button>
      </div>

      {/* One region, three occupants. The roster holds it unless a band panel
          has been opened. */}
      {region === 'roster' ? (
        error ? (
          <p className="ts-error">{error}</p>
        ) : (
          <Table
            build
            label={loading ? 'MEMBERS' : 'MEMBERS · ' + rows.length}
            columns={[
              { key: 'name', label: 'NAME', type: 'identity' },
              { key: 'github', label: 'GITHUB', color: () => 'var(--color-ink-3)' },
              {
                key: 'role',
                label: 'ROLE',
                align: 'end',
                color: (r) => (r.role === 'admin' ? 'var(--color-warm)' : 'var(--color-ink-4)'),
              },
              { key: 'repos', label: 'REPOS TOUCHED', align: 'end', drop: 2 },
              { key: 'tasks', label: 'TASKS · 14D', align: 'end', drop: 2 },
              { key: 'spend', label: 'SPEND · 14D', align: 'end', drop: 1 },
              { key: 'seen', label: 'SEEN', align: 'end', drop: 1 },
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
              return { ...row, role: pick ? pick.id : row.role }
            }}
            onCommit={(id, ids) => {
              if (id === 'remove') ids.map(String).forEach((u) => void adapter.remove(u))
            }}
          />
        )
      ) : null}

      {region === 'models' ? (
        <div className="ts-region">
          <p className="ts-note">
            Model configuration is org-level today, so this reads rather than writes — the per-team
            override is not built.
          </p>
        </div>
      ) : null}

      {region === 'sources' ? (
        <div className="ts-region ts-sources">
          <SourceCard
            name="GitHub"
            source="github"
            state="configured"
            scope="repositories this team tracks"
            stats={[
              ['events · 7d', '—'],
              ['became tasks', '—'],
            ]}
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
          />
          <SourceCard
            name="Slack"
            source="slack"
            state="configured"
            scope="channels this team watches"
            stats={[['mentions · 7d', '—']]}
          />
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
      ) : null}

      {/* Team admins only. A member's page ends at the pager — the card is not
          rendered rather than rendered disabled, since a verb you may never use
          is not information. Archiving is also an org admin's to do from
          Organization, which is where a member is pointed if they ask. */}
      {isAdmin ? (
        <div className="ts-danger">
          <div className="ts-danger-h">DANGER</div>
          <div className="ts-danger-b">
            <div>
              <div className="ts-danger-t">Archive this team</div>
              <p className="ts-danger-p">
                Its repositories stop being watched and {loading ? '—' : members.length}{' '}
                {members.length === 1 ? 'member loses' : 'members lose'} access. Run history is
                kept; an org admin can restore it.
              </p>
            </div>
            <button
              type="button"
              className="ts-danger-btn"
              onClick={() => navigate(orgHref('/settings'))}
            >
              Archive
            </button>
          </div>
        </div>
      ) : null}
    </div>
  )
}
