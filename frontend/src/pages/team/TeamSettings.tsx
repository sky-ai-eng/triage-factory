import { useCallback, useEffect, useMemo, useState } from 'react'
import { useNavigate, useSearchParams } from 'react-router'
import Table from '../../ui/table/Table'
import type { TableCandidate, TableRow } from '../../ui/table/Table'
import SourceCard from '../../ui/sourcecard/SourceCard'
import GitHubSource from './GitHubSource'
import JiraSource from './JiraSource'
import SlackSource from './SlackSource'
import type { SourceKind } from './SourceFrame'
import { usePageHeader } from '../../contexts/ChromeContext'
import { useActiveOrgId } from '../../contexts/OrgContext'
import { useOrgHref } from '../../hooks/useOrgHref'
import { useTeams } from '../../hooks/useTeams'
import { useTeamRole } from '../../hooks/useTeamRole'
import { useMemberRoster, displayNameFor } from '../../hooks/useMemberRoster'
import type { MemberRosterAdapter, RosterMember } from '../../hooks/useMemberRoster'
import { apiFetch, apiJSON } from '../../lib/apiClient'
import './team-settings.css'

// Team settings — the overview, and the three source pages behind it.
//
// ONE ROUTE, FOUR VIEWS. A source page is a drill-in, not a destination: the
// rail's Team row stays lit, the crumbs grow a level, and `?source=` is what
// makes the drill-in survive a reload and a shared link. Leaving one goes back
// to the sources panel — up one level, which is where the cards live — rather
// than to the roster.
//
// The page is a header group and one REGION. The header group is the team's
// title block and a two-panel band; the region below it holds one of three
// things, and the band is what switches them:
//
//   roster   the default, and what the page is mostly for
//   sources  what CONFIGURED SOURCES opens into — the five cards
//   models   what CONFIGURED MODELS opens into
//
// All three are absolutely positioned inside the region, which is why the
// danger card sits on the page's bottom edge rather than wherever the roster
// happens to end: the table takes the slack (`flex: 1`) and the card does not.
//
// The member view is a strict subset of the same page, never a different page.
// Nothing is greyed out: a control a member may never use is absent, because a
// verb that 403s is not information. The gate is `team.role === 'admin'` and
// nothing else — an org admin is deliberately not a team admin here.
//
// Figures read `—` where they need an aggregation the API does not have
// (backend-needs.md 17-19). A zero would be a claim about a team's week. The
// member count and the roster are real, and so is adding and removing people.

type Region = 'roster' | 'sources' | 'models'

const SOURCES_BY_KEY: Record<string, SourceKind> = {
  github: 'github',
  jira: 'jira',
  slack: 'slack',
}
const SOURCE_NAMES: Record<SourceKind, string> = {
  github: 'GitHub',
  jira: 'Jira',
  slack: 'Slack',
}

type MemberApiRow = {
  user_id: string
  display_name: string
  github_username: string | null
  jira_account_id: string | null
  role: string
  is_current_user: boolean
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
  tone?: 'warm' | 'faint'
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
  const orgId = useActiveOrgId()
  const [region, setRegion] = useState<Region>('roster')
  const [filter, setFilter] = useState('')
  const [params, setParams] = useSearchParams()
  const source = SOURCES_BY_KEY[params.get('source') ?? ''] ?? null

  const openSource = useCallback(
    (key: SourceKind) => {
      const next = new URLSearchParams(params)
      next.set('source', key)
      setParams(next)
    },
    [params, setParams],
  )
  // Back to the sources panel, not to the roster: a source page's title
  // chevron goes up one level, and the panel's own back goes home, so two
  // presses land where you started.
  const closeSource = useCallback(() => {
    const next = new URLSearchParams(params)
    next.delete('source')
    setRegion('sources')
    setParams(next)
  }, [params, setParams])

  const { teams, lastActingTeamId, loaded } = useTeams()
  const team = loaded ? (teams.find((t) => t.id === lastActingTeamId) ?? teams[0] ?? null) : null
  const teamId = team?.id ?? ''
  const { roleForTeam } = useTeamRole()
  const isAdmin = roleForTeam(teamId) === 'admin'

  // The three writes this page makes, defined once. `keepalive` keeps the
  // request alive past the document on the unload path; sendBeacon is not an
  // option, being POST-only where these are a PATCH and a DELETE.
  const sendRole = useCallback(
    (userId: string, role: string, keepalive = false) =>
      apiFetch(`/api/teams/${teamId}/members/${userId}`, {
        method: 'PATCH',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ role }),
        keepalive,
      }),
    [teamId],
  )
  const sendRemove = useCallback(
    (userId: string, keepalive = false) =>
      apiFetch(`/api/teams/${teamId}/members/${userId}`, { method: 'DELETE', keepalive }),
    [teamId],
  )
  const sendAdd = useCallback(
    (userId: string, role: string) =>
      apiFetch(`/api/teams/${teamId}/members`, {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ user_id: userId, role }),
      }),
    [teamId],
  )

  const adapter = useMemo<MemberRosterAdapter>(
    () => ({
      roles: TEAM_ROLES,
      protectedRole: 'admin',
      roleLabels: ROLE_LABELS,
      async fetchMembers(): Promise<RosterMember[]> {
        // Never build the URL without an id: `/api/teams//members` matches no
        // route, and comes back as "endpoint not found".
        if (!teamId) return []
        const data = await apiJSON<{ members: MemberApiRow[] }>(`/api/teams/${teamId}/members`)
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
        await sendRole(userId, role)
      },
      async remove(userId) {
        await sendRemove(userId)
      },
    }),
    [teamId, sendRole, sendRemove],
  )

  const { members, loading, error } = useMemberRoster(adapter)

  // The org's people, so `+ add teammate` has candidates. Anyone already on the
  // team is listed and disabled rather than hidden — a name missing from a
  // directory reads as the directory being wrong.
  const [orgPeople, setOrgPeople] = useState<MemberApiRow[]>([])
  useEffect(() => {
    if (!orgId || !isAdmin) return
    let live = true
    void apiJSON<{ members: MemberApiRow[] }>(`/api/orgs/${orgId}/members`)
      .then((d) => {
        if (live) setOrgPeople(d.members)
      })
      .catch(() => {
        // A directory that cannot be read leaves the draft page with nothing to
        // offer, which its own placeholder already says.
      })
    return () => {
      live = false
    }
  }, [orgId, isAdmin])

  const candidates: TableCandidate[] = useMemo(() => {
    const on = new Set(members.map((m) => m.userId))
    return orgPeople.map((p) => ({
      id: p.user_id,
      name: p.display_name,
      disabled: on.has(p.user_id),
      disabledNote: on.has(p.user_id) ? 'already on this team' : undefined,
    }))
  }, [orgPeople, members])

  // The band states the team, and a source hangs off it: the name is the crumb
  // you return by, so it stays visible everywhere in the team rather than only
  // on its first screen.
  const header = useMemo(
    () =>
      source
        ? {
            crumbs: [
              { name: 'Team', onClick: closeSource },
              { name: team?.name ?? 'Team', onClick: closeSource },
            ],
            title: SOURCE_NAMES[source],
          }
        : { crumbs: [{ name: 'Team' }], title: team?.name ?? 'Team' },
    [team?.name, source, closeSource],
  )
  usePageHeader(header)

  const rows: TableRow[] = useMemo(() => {
    const q = filter.trim().toLowerCase()
    return members
      .map((m) => ({
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
      .filter((r) => !q || r.name.toLowerCase().includes(q) || r.github.toLowerCase().includes(q))
  }, [members, filter])

  if (!team) {
    return <p className="ts-empty">{loaded ? 'No team resolved for this org.' : 'Loading…'}</p>
  }

  if (source) {
    const body = { teamId, teamName: team.name, isAdmin, onBack: closeSource }
    if (source === 'github') return <GitHubSource {...body} />
    if (source === 'jira') return <JiraSource {...body} />
    return <SlackSource {...body} />
  }

  return (
    <div className="ts">
      {/* The header group: the title block and the band, which stay put while
          the region below them changes. */}
      <div className="ts-headgroup">
        <div className="ts-top">
          <div className="ts-id">
            <h1 className="ts-name">{team.name}</h1>
            <span className="ts-created">event sources and people</span>
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

        {/* The band. Two controls, not two headings: each opens its own panel
            in the region below rather than adding a section to the page. */}
        <div className="ts-band">
          <button
            type="button"
            className="ts-panel"
            aria-expanded={region === 'models'}
            onClick={() => setRegion((r) => (r === 'models' ? 'roster' : 'models'))}
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
                  {/* No share-of-runs aggregation, so the track holds its
                      height and draws no fill rather than inventing one. */}
                  <Meter frac={null} />
                </span>
              ))}
            </span>
          </button>

          <button
            type="button"
            className="ts-panel ts-panel-right"
            aria-expanded={region === 'sources'}
            onClick={() => setRegion((r) => (r === 'sources' ? 'roster' : 'sources'))}
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
            <span className="ts-panel-foot">last 7 days · repositories, projects, channels</span>
          </button>
        </div>
      </div>

      {/* One region, three occupants, all of them filling it. That is what puts
          the danger card on the page's bottom edge. */}
      <div className="ts-region">
        {region === 'roster' ? (
          <div className="ts-rosterpanel">
            <div className="ts-tablewrap">
              {error ? (
                <p className="ts-error">{error}</p>
              ) : (
                <Table
                  build
                  label="MEMBERS"
                  columns={[
                    { key: 'name', label: 'NAME', type: 'identity' },
                    { key: 'github', label: 'GITHUB', color: () => 'var(--color-ink-3)' },
                    {
                      key: 'role',
                      label: 'ROLE',
                      align: 'end',
                      color: (r) =>
                        r.role === 'admin' ? 'var(--color-warm)' : 'var(--color-ink-4)',
                    },
                    { key: 'repos', label: 'REPOS TOUCHED', align: 'end', drop: 2 },
                    { key: 'tasks', label: 'TASKS · 14D', align: 'end', drop: 2 },
                    { key: 'spend', label: 'SPEND · 14D', align: 'end', drop: 1 },
                    { key: 'seen', label: 'SEEN', align: 'end', drop: 1 },
                  ]}
                  rows={rows}
                  pageSize="auto"
                  sortKey="name"
                  barPosition="absolute"
                  // Selection exists for the verbs, so it goes with them: a
                  // member sees no row checkboxes and no bar at all.
                  selectable={isAdmin}
                  headerRight={
                    <input
                      className="ts-filter"
                      value={filter}
                      onChange={(e) => setFilter(e.target.value)}
                      placeholder="filter"
                      aria-label="Filter members"
                    />
                  }
                  add={
                    isAdmin
                      ? {
                          label: 'add teammate',
                          placeholder: 'name — three characters',
                          options: candidates,
                          fields: [{ key: 'role', options: ['member', 'admin'], value: 'member' }],
                          onAdd: (person, values) => {
                            if (person.id) void sendAdd(person.id, values.role || 'member')
                          },
                        }
                      : null
                  }
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
                              message: (n) =>
                                n + (n === 1 ? ' member' : ' members') + ' removed from the team',
                            },
                          },
                        }
                      : null
                  }
                  mutate={(row, id, pick) => {
                    if (id === 'remove') return null
                    return { ...row, role: pick ? pick.id : row.role }
                  }}
                  // The only place a request is made. Everything the table can
                  // apply has to be sent from here, or the screen shows a
                  // change the server never hears about.
                  onCommit={(id, ids, { pick, reason }) => {
                    const alive = reason === 'unload'
                    const list = ids.map(String)
                    if (id === 'remove') list.forEach((u) => void sendRemove(u, alive))
                    else if (id === 'role' && pick)
                      list.forEach((u) => void sendRole(u, pick.id, alive))
                  }}
                />
              )}
            </div>

            {/* Team admins only. A member's page ends at the pager — the card
                is not rendered rather than rendered disabled, since a verb you
                may never use is not information. Archiving is also an org
                admin's to do from Organization, which is where a member is
                pointed if they ask. */}
            {isAdmin ? (
              <div className="ts-danger">
                <div className="ts-danger-h">DANGER</div>
                <div className="ts-danger-b">
                  <div className="ts-danger-c">
                    <span className="ts-danger-t">Archive this team</span>
                    <span className="ts-danger-p">
                      Its repositories stop being watched and {loading ? '—' : members.length}{' '}
                      {members.length === 1 ? 'member loses' : 'members lose'} access. Run history
                      is kept; an org admin can restore it.
                    </span>
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
        ) : null}

        {region === 'sources' ? (
          <div className="ts-panelview">
            <div className="ts-panelview-head">
              <button type="button" className="ts-back" onClick={() => setRegion('roster')}>
                <i className="ts-back-chev" aria-hidden="true" />
                <span>EVENT SOURCES</span>
              </button>
              <span className="ts-lead-flex" />
              <span className="ts-panelview-n">{SOURCES.length} sources · — events in 7 days</span>
            </div>

            <div className="ts-panelview-body">
              <div className="ts-intro">
                <div className="ts-intro-c">
                  <span className="ts-intro-t">Pick a source to configure</span>
                  <span className="ts-intro-p">
                    Every task this team runs starts as an event from one of these sources. Choose a
                    source to set what it watches: which repositories send events, which Jira
                    projects and statuses count, and which Slack channels the factory reads.
                  </span>
                </div>
                <div className="ts-intro-figs">
                  <div className="ts-intro-fig">
                    <span className="ts-intro-fig-v">—</span>
                    <span className="ts-intro-fig-l">events · 7d</span>
                  </div>
                  <div className="ts-intro-fig">
                    <span className="ts-intro-fig-v" data-tone="warm">
                      —
                    </span>
                    <span className="ts-intro-fig-l">became tasks</span>
                  </div>
                </div>
              </div>

              <div className="ts-srcgrid">
                <SourceCard
                  name="GitHub"
                  source="github"
                  state="configured"
                  scope="repositories this team tracks"
                  stats={[
                    ['events · 7d', '—'],
                    ['became tasks', '—'],
                  ]}
                  onClick={() => openSource('github')}
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
                  onClick={() => openSource('jira')}
                />
                <SourceCard
                  name="Slack"
                  source="slack"
                  state="configured"
                  scope="channels this team watches"
                  stats={[['mentions · 7d', '—']]}
                  onClick={() => openSource('slack')}
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
                  note="Issues and cycles, for teams that track work in Linear instead of Jira."
                />
              </div>
            </div>

            <div className="ts-panelview-foot">
              <span>{SOURCES.length} sources connected · last event —</span>
            </div>
          </div>
        ) : null}

        {region === 'models' ? (
          <div className="ts-panelview">
            <div className="ts-panelview-head">
              <button type="button" className="ts-back" onClick={() => setRegion('roster')}>
                <i className="ts-back-chev" aria-hidden="true" />
                <span>CONFIGURED MODELS</span>
              </button>
              <span className="ts-lead-flex" />
              <span className="ts-panelview-n">{MODELS.length} models</span>
            </div>
            <div className="ts-panelview-body">
              <p className="ts-note">
                Model configuration is org-level today, so this reads rather than writes — the
                per-team override is not built.
              </p>
            </div>
          </div>
        ) : null}
      </div>
    </div>
  )
}
