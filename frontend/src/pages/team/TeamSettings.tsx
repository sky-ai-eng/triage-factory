import { useCallback, useEffect, useMemo, useState } from 'react'
import { useNavigate, useSearchParams } from 'react-router'
import Table from '../../ui/table/Table'
import type { TableCandidate, TableRow } from '../../ui/table/Table'
import SourceCard from '../../ui/sourcecard/SourceCard'
import Dialog from '../../ui/dialog/Dialog'
import type { DialogConsequence } from '../../ui/dialog/Dialog'
import GitHubSource from './GitHubSource'
import JiraSource from './JiraSource'
import SlackSource from './SlackSource'
import type { SourceKind } from './SourceFrame'
import { usePageHeader } from '../../contexts/ChromeContext'
import { useActiveOrgId } from '../../contexts/OrgContext'
import { useOrgHref } from '../../hooks/useOrgHref'
import { useOrgRole } from '../../hooks/useOrgRole'
import { useTeams } from '../../hooks/useTeams'
import { useTeamRole } from '../../hooks/useTeamRole'
import { useOptionalAuth } from '../../contexts/AuthContext'
import { useMemberRoster, displayNameFor } from '../../hooks/useMemberRoster'
import type { MemberRosterAdapter, RosterMember } from '../../hooks/useMemberRoster'
import { useTeamActivity, activitySource } from '../../hooks/useTeamActivity'
import { useEventSources } from '../../hooks/useEventSources'
import { sourceUnavailableReason } from '../../lib/eventSources'
import { useTeamUsage, fmtSpendUSD } from '../../hooks/useTeamUsage'
import { archiveTeam, fetchArchivePreview } from '../../lib/teamLifecycle'
import type { ArchivePreview } from '../../lib/teamLifecycle'
import { toast } from '../../components/Toast/toastStore'
import { apiFetch, apiList } from '../../lib/apiClient'
import { fetchTeamRoster } from '../../lib/teamRoster'
import { fetchTeamRepos } from '../settings/teamConfig'
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

/** What this deployment can run — a static sketch, not a read. */
// TODO(TFAC-879): replace this array with the team models read
// (GET /api/teams/{team_id}/models), which now serves the team's effective
// enable-set, and delete the hardcoded prices with it.
const MODELS = [
  { name: 'Claude Opus 5', tag: '(default)', price: '$25 / M' },
  { name: 'Claude Sonnet 5', tag: '', price: '$15 / M' },
  { name: 'Claude Haiku 4.5', tag: '', price: '$4 / M' },
]

/** The three live sources, in the order the band and the card grid list them. */
const SOURCES: SourceKind[] = ['github', 'jira', 'slack']

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
function Meter({ frac, thin }: { frac: number | null; thin?: boolean }) {
  return (
    <span className="ts-meter" data-thin={thin ? '' : undefined}>
      {frac == null ? null : <span className="ts-meter-fill" style={{ width: frac * 100 + '%' }} />}
    </span>
  )
}

export default function TeamSettings() {
  const navigate = useNavigate()
  const orgHref = useOrgHref()
  const orgId = useActiveOrgId()
  const [region, setRegion] = useState<Region>('roster')

  // Why a source cannot reach this org, per kind — null when it can, and null
  // while the read is unresolved, because greying a card on an answer we do
  // not have is worse than greying it late.
  const { stateOf } = useEventSources()
  const sourceOff = useCallback(
    (kind: string) => sourceUnavailableReason(kind, stateOf(kind)),
    [stateOf],
  )
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

  const { teams, lastActingTeamId, loaded, refresh: refreshTeams } = useTeams()
  const team = loaded ? (teams.find((t) => t.id === lastActingTeamId) ?? teams[0] ?? null) : null
  const teamId = team?.id ?? ''
  const { roleForTeam } = useTeamRole()
  // Membership is a multi-mode idea. Local is one person who is everything, and
  // every team-member route answers 404 there — so the verbs are absent rather
  // than drawn over an endpoint that cannot serve them.
  const isLocal = useOptionalAuth() === null
  const isAdmin = !isLocal && roleForTeam(teamId) === 'admin'
  // Archiving is the org's verb, not the team's: the endpoint is org-admin only
  // and 404s for everyone else, so a team admin is shown no card rather than a
  // card whose one button cannot work.
  const { isAdmin: isOrgAdmin } = useOrgRole()

  // One 7-day node read feeds every flow figure — the header's task count,
  // the sources band, its meters and its stats — so they all agree by
  // construction. Spend is its own read over the fortnight its label names,
  // fired only for an admin because the endpoint refuses everyone else.
  const activity = useTeamActivity(teamId, 7)
  const usage = useTeamUsage(teamId, 14, isAdmin || isOrgAdmin)
  const flowEvents = activity ? activity.by_source.reduce((n, r) => n + (r.events ?? 0), 0) : null
  const flowTasks = activity ? activity.by_source.reduce((n, r) => n + r.tasks, 0) : null
  const spendByUser = useMemo(
    () => new Map((usage?.by_user ?? []).map((u) => [u.user_id, u.cost])),
    [usage],
  )

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
        // Never build the URL without an id: a blank one addresses no team, and
        // comes back as "endpoint not found".
        if (!teamId) return []
        const roster = await fetchTeamRoster(teamId)
        return roster.members.map((m) => ({
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

  // ── Archive ────────────────────────────────────────────────────────────
  // Four states rather than a boolean: the question cannot be asked until the
  // page knows what pressing it would stop, and must not be asked twice while
  // that is being done.
  const [archive, setArchive] = useState<'closed' | 'reading' | 'asking' | 'sending'>('closed')
  const [preview, setPreview] = useState<ArchivePreview | null>(null)
  const [watched, setWatched] = useState<number | null>(null)

  const openArchive = useCallback(() => {
    if (archive !== 'closed') return
    setArchive('reading')
    // BOTH READS LAND BEFORE THE DIALOG DOES, not underneath it. The build
    // measures the empty frame and then fills it, and a panel that grows a line
    // after that beat makes the beat a lie — so the waiting is done out here,
    // on the button, and the dialog opens already knowing what it says.
    void Promise.all([
      fetchArchivePreview(teamId),
      // The first consequence the design states, and the one number this page
      // does not already hold. Best-effort: an unread count is a line the
      // dialog leaves out, never a zero.
      fetchTeamRepos(teamId).then((repos) => repos?.length ?? null),
    ])
      .then(([live, repos]) => {
        setPreview(live)
        setWatched(repos)
        setArchive('asking')
      })
      .catch((e) => {
        // A confirm that cannot state its consequences is not a confirm, so the
        // question is withdrawn rather than asked with the answer left blank.
        setArchive('closed')
        toast.error(e instanceof Error ? e.message : 'Could not check this team for live work.')
      })
  }, [teamId, archive])

  const confirmArchive = useCallback(() => {
    if (!preview || archive === 'sending') return
    setArchive('sending')
    void archiveTeam(teamId)
      .then((res) => {
        setArchive('closed')
        const runs = res.cancelled_runs
        toast.success(
          `Team archived — stopped ${runs} ${runs === 1 ? 'delegation' : 'delegations'}.`,
        )
        // The team is gone from /api/teams, which drives this page to whichever
        // team is left — or to its own empty state.
        void refreshTeams()
      })
      .catch((e) => {
        setArchive('closed')
        toast.error(e instanceof Error ? e.message : 'Could not archive the team.')
      })
  }, [teamId, preview, archive, refreshTeams])

  // What will actually happen, one line each, and what stays alongside what
  // goes. Every number is read rather than assumed; a line nothing answered is
  // absent instead of guessed. Settled before the dialog opens, so the list it
  // builds is the list it ends with.
  const consequences = useMemo<DialogConsequence[]>(() => {
    const out: DialogConsequence[] = []
    if (watched !== null)
      out.push({
        text: `${watched} ${watched === 1 ? 'repository stops' : 'repositories stop'} being watched`,
        tone: 'loss',
      })
    out.push({
      text: `${members.length} ${members.length === 1 ? 'member loses' : 'members lose'} access`,
      tone: 'loss',
    })
    if (preview)
      out.push({
        text: `${preview.active_runs} ${preview.active_runs === 1 ? 'delegation' : 'delegations'} stop now`,
        tone: 'loss',
      })
    out.push({ text: 'Run history and cost records are kept', tone: 'keep' })
    return out
  }, [watched, members.length, preview])

  // The org's people, so `+ add teammate` has candidates. Anyone already on the
  // team is listed and disabled rather than hidden — a name missing from a
  // directory reads as the directory being wrong.
  const [orgPeople, setOrgPeople] = useState<MemberApiRow[]>([])
  useEffect(() => {
    if (!orgId || !isAdmin) return
    let live = true
    // page_size at the route max: the candidate set is the whole directory
    // minus the team, which is a comparison across both rosters rather than a
    // page of rows to read.
    void apiList<MemberApiRow>(`/api/orgs/${encodeURIComponent(orgId)}/members/list`, {
      page_size: 200,
    })
      .then((page) => {
        if (live) setOrgPeople(page.items)
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
        // repos / tasks / seen still need per-member aggregations the API
        // does not have. Spend answers once the admin-gated usage read does:
        // a member absent from by_user then genuinely spent nothing.
        repos: '—',
        tasks: '—',
        spend: usage ? fmtSpendUSD(spendByUser.get(m.userId) ?? 0) : '—',
        seen: '—',
      }))
      .filter((r) => !q || r.name.toLowerCase().includes(q) || r.github.toLowerCase().includes(q))
  }, [members, filter, usage, spendByUser])

  // A team keeps at least one admin, and the database is what enforces it — so
  // a gesture that would empty the seat is never offered, rather than sent and
  // refused. Counted over the whole roster, never `rows`: the filter box
  // narrows what is on screen, and an admin scrolled out of view still holds
  // the seat.
  const adminCount = useMemo(() => members.filter((m) => m.role === 'admin').length, [members])
  const strands = useCallback(
    (sel: TableRow[]) => adminCount - sel.filter((r) => r.role === 'admin').length < 1,
    [adminCount],
  )
  // Promotion is always safe — it can only add to the seat — so the picker
  // narrows to that one choice rather than closing, and the way out of the
  // refusal stays in the reader's hand.
  const roleOptions = useCallback(
    (sel: TableRow[]) =>
      TEAM_ROLES.filter((r) => r === 'admin' || !strands(sel)).map((r) => ({
        id: r,
        name: ROLE_LABELS[r],
      })),
    [strands],
  )

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
                  value={usage ? fmtSpendUSD(usage.total_cost_usd) : null}
                  label="spent · 14 days"
                  onClick={() => navigate(orgHref('/usage'))}
                />
              </>
            ) : null}
            <span className="ts-div" />
            <Figure value={flowTasks} label="tasks · 7 days" tone="warm" />
          </div>
        </div>

        {/* The band. Two cells, not two headings: each opens its own panel in
            the region below rather than adding a section to the page. The cell
            is the mouse's target and its head is the control — the cell cannot
            be a button itself, because the sources inside it are, and a control
            inside a control is neither. */}
        <div className="ts-band">
          <div
            className="ts-panel"
            onClick={() => setRegion((r) => (r === 'models' ? 'roster' : 'models'))}
          >
            <button
              type="button"
              className="ts-panel-head"
              aria-expanded={region === 'models'}
              onClick={(e) => {
                // The cell behind it toggles too, and two toggles are none.
                e.stopPropagation()
                setRegion((r) => (r === 'models' ? 'roster' : 'models'))
              }}
            >
              <span className="ts-panel-t">CONFIGURED MODELS</span>
              <span className="ts-lead" />
              <span className="ts-panel-n">{MODELS.length} models</span>
              <i className="ts-chev" aria-hidden="true" />
            </button>
            <div className="ts-rows">
              {MODELS.map((m) => (
                <div className="ts-row" key={m.name}>
                  <div className="ts-row-line">
                    <span className="ts-row-n">{m.name}</span>
                    {m.tag ? <span className="ts-row-tag">{m.tag}</span> : null}
                    <span className="ts-lead-flex" />
                    <span className="ts-row-v">{m.price}</span>
                  </div>
                  {/* No share-of-runs aggregation, so the track holds its
                      height and draws no fill rather than inventing one. */}
                  <Meter frac={null} />
                </div>
              ))}
            </div>
          </div>

          <div
            className="ts-panel ts-panel-right"
            onClick={() => setRegion((r) => (r === 'sources' ? 'roster' : 'sources'))}
          >
            <button
              type="button"
              className="ts-panel-head"
              aria-expanded={region === 'sources'}
              onClick={(e) => {
                e.stopPropagation()
                setRegion((r) => (r === 'sources' ? 'roster' : 'sources'))
              }}
            >
              <span className="ts-panel-t">EVENT SOURCES</span>
              <span className="ts-lead" />
              <span className="ts-panel-n">{flowEvents ?? '—'} events</span>
              <i className="ts-chev" aria-hidden="true" />
            </button>
            <div className="ts-flow">
              <div className="ts-flow-list">
                {/* Naming a source is how you get to it: the band works as
                    navigation between the three, not only as a way into the
                    panel that lists them. A source the org cannot reach is
                    the exception — its row is not a control, so the click
                    falls through to the cell, which opens the panel where
                    the card says why the source is off. */}
                {SOURCES.map((s) => {
                  // A null count is undefined for the source (Slack has no
                  // tracked set), so its row keeps the dash and draws no
                  // share — a zero would claim a quiet week.
                  const events = activitySource(activity, s)?.events ?? null
                  const frac = events !== null && flowEvents ? events / flowEvents : null
                  const line = (
                    <>
                      <span className="ts-flow-row-line">
                        <span className="ts-flow-row-n">{SOURCE_NAMES[s]}</span>
                        <span className="ts-lead-flex" />
                        <span className="ts-flow-row-v">{events ?? '—'}</span>
                      </span>
                      <Meter frac={frac} thin />
                    </>
                  )
                  return sourceOff(s) ? (
                    <div className="ts-flow-row" data-off key={s}>
                      {line}
                    </div>
                  ) : (
                    <button
                      type="button"
                      className="ts-flow-row"
                      key={s}
                      onClick={(e) => {
                        e.stopPropagation()
                        openSource(s)
                      }}
                    >
                      {line}
                    </button>
                  )
                })}
              </div>
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
              <div className="ts-flow-stats">
                <div className="ts-stat">
                  <span className="ts-stat-v">{flowEvents ?? '—'}</span>
                  <span className="ts-stat-l">events</span>
                </div>
                <div className="ts-stat">
                  <span className="ts-stat-v" data-tone="warm">
                    {flowTasks ?? '—'}
                  </span>
                  <span className="ts-stat-l">became tasks</span>
                </div>
              </div>
            </div>
            <span className="ts-panel-foot">last 7 days · repositories, projects, channels</span>
          </div>
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
                            options: roleOptions,
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
                              enabledFor: (sel) => !strands(sel),
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

            {/* Org admins only. Everyone else's page ends at the pager — the
                card is not rendered rather than rendered disabled, since a verb
                you may never use is not information, and this one is the org's
                rather than the team's. */}
            {isOrgAdmin ? (
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
                  {/* The waiting happens here rather than inside the dialog,
                      so what opens is already whole. */}
                  <button type="button" className="ts-danger-btn" onClick={openArchive}>
                    {archive === 'reading' ? 'Checking…' : 'Archive'}
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
              <span className="ts-panelview-n">
                {SOURCES.length} sources · {flowEvents ?? '—'} events in 7 days
              </span>
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
                    <span className="ts-intro-fig-v">{flowEvents ?? '—'}</span>
                    <span className="ts-intro-fig-l">events · 7d</span>
                  </div>
                  <div className="ts-intro-fig">
                    <span className="ts-intro-fig-v" data-tone="warm">
                      {flowTasks ?? '—'}
                    </span>
                    <span className="ts-intro-fig-l">became tasks</span>
                  </div>
                </div>
              </div>

              <div className="ts-srcgrid">
                <SourceCard
                  name="GitHub"
                  source="github"
                  state={sourceOff('github') ? 'unavailable' : 'configured'}
                  scope="repositories this team tracks"
                  stats={[
                    ['events · 7d', activitySource(activity, 'github')?.events ?? '—'],
                    ['became tasks', activitySource(activity, 'github')?.tasks ?? '—'],
                  ]}
                  note={sourceOff('github')}
                  onClick={() => openSource('github')}
                />
                <SourceCard
                  name="Jira"
                  source="jira"
                  state={sourceOff('jira') ? 'unavailable' : 'configured'}
                  scope="projects this team watches"
                  stats={[
                    ['events · 7d', activitySource(activity, 'jira')?.events ?? '—'],
                    ['became tasks', activitySource(activity, 'jira')?.tasks ?? '—'],
                  ]}
                  note={sourceOff('jira')}
                  onClick={() => openSource('jira')}
                />
                <SourceCard
                  name="Slack"
                  source="slack"
                  state={sourceOff('slack') ? 'unavailable' : 'configured'}
                  scope="channels this team watches"
                  stats={[['mentions · 7d', activitySource(activity, 'slack')?.events ?? '—']]}
                  note={sourceOff('slack')}
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
              <span>
                {SOURCES.filter((k) => !sourceOff(k)).length} sources connected · last event —
              </span>
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

      {/* The one question this page cannot answer for you. It states what will
          happen rather than asking whether you are sure, and every count in it
          was read from the server before it opened — an archive stops live
          work, and how much is the whole reason to interrupt someone. */}
      <Dialog
        open={archive === 'asking' || archive === 'sending'}
        kind="destructive"
        build="flash"
        title={`Archive ${team.name}?`}
        body="The team stops working. Nothing it produced is deleted."
        consequences={consequences}
        note="An org admin can restore an archived team."
        confirmLabel={archive === 'sending' ? 'Archiving…' : 'Archive'}
        onConfirm={confirmArchive}
        onCancel={() => setArchive('closed')}
      />
    </div>
  )
}
