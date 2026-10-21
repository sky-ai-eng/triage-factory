import { describe, it, expect, vi, beforeEach } from 'vitest'
import { render, screen, fireEvent } from '@testing-library/react'
import PredicateEditor from './PredicateEditor'
import { jsonBody } from '../test/apiResponse'
import type { MeResponse, SlackChannelView, TeamMember } from '../types'

// The viewer has an identity on every source, each a different value, so a
// test can tell which column an allowlist was filled from.
const me: MeResponse = {
  id: 'user-1',
  github_username: 'octocat',
  jira_account_id: '557058:abc-me',
  jira_display_name: 'Me on Jira',
  linear_user_id: 'lin-me',
  linear_display_name: 'Me on Linear',
  orgs: [],
  teams: [],
  org_creation_enabled: true,
  is_operator: false,
}

const roster = vi.hoisted(() => ({ members: [] as TeamMember[] }))
vi.mock('../hooks/useDeploymentConfig', () => ({
  useMe: () => ({ me, loading: false, error: null }),
  useTeamMembers: () => ({ members: roster.members, bot: null, loading: false, error: null }),
}))

// The acting team the channel field falls back to when no teamId is passed.
vi.mock('../hooks/useTeams', () => ({
  useTeams: () => ({ teams: [{ id: 'team-acting' }], lastActingTeamId: '', loaded: true }),
  pickerDefault: (teams: { id: string }[]) => teams[0]?.id ?? '',
}))

function member(over: Partial<TeamMember>): TeamMember {
  return {
    user_id: 'user-x',
    display_name: '',
    github_username: null,
    jira_account_id: null,
    linear_user_id: null,
    role: 'member',
    is_current_user: false,
    ...over,
  }
}

function stubSchema(name: string) {
  vi.stubGlobal(
    'fetch',
    vi.fn().mockResolvedValue({
      ok: true,
      status: 200,
      ...jsonBody({ fields: [{ name, type: 'string_list' }] }),
    }),
  )
}

describe('PredicateEditor identity allowlists', () => {
  beforeEach(() => {
    roster.members = []
  })

  it.each([
    ['linear:issue:assigned', 'assignee_in', 'lin-me'],
    ['linear:issue:available', 'creator_in', 'lin-me'],
    ['linear:issue:commented', 'commenter_in', 'lin-me'],
    ['linear:issue:label_added', 'assignee_in', 'lin-me'],
    ['linear:issue:label_removed', 'assignee_in', 'lin-me'],
    ['jira:issue:assigned', 'reporter_in', '557058:abc-me'],
    ['jira:issue:commented', 'commenter_in', '557058:abc-me'],
    ['github:pr:opened', 'author_in', 'octocat'],
  ])(
    'the solo toggle on %s %s writes the viewer’s identity for that source',
    async (eventType, field, want) => {
      stubSchema(field)
      const onChange = vi.fn()
      render(<PredicateEditor eventType={eventType} value={{}} onChange={onChange} />)

      fireEvent.click(await screen.findByRole('button', { pressed: false }))

      expect(onChange).toHaveBeenCalledWith({ [field]: [want] })
    },
  )

  it('lists team members by their Linear user id on a Linear event', async () => {
    stubSchema('assignee_in')
    roster.members = [
      member({
        user_id: 'user-1',
        display_name: 'Me',
        github_username: 'octocat',
        linear_user_id: 'lin-me',
        is_current_user: true,
      }),
      member({
        user_id: 'user-2',
        display_name: 'Ada',
        github_username: 'ada-gh',
        linear_user_id: 'lin-ada',
      }),
      member({ user_id: 'user-3', display_name: 'Bob', github_username: 'bob-gh' }),
    ]
    const onChange = vi.fn()
    render(<PredicateEditor eventType="linear:issue:assigned" value={{}} onChange={onChange} />)

    fireEvent.focus(await screen.findByRole('combobox'))

    expect(screen.getByText('lin-ada')).toBeInTheDocument()
    expect(screen.queryByText('ada-gh')).not.toBeInTheDocument()
    expect(screen.getByText('— no Linear identity')).toBeInTheDocument()

    fireEvent.mouseDown(screen.getByText('lin-ada'))
    expect(onChange).toHaveBeenCalledWith({ assignee_in: ['lin-ada'] })
  })
})

function channel(over: Partial<SlackChannelView>): SlackChannelView {
  return {
    channel_id: 'C0',
    name: '',
    workspace_id: 'T1',
    is_private: false,
    bot_is_member: true,
    tracked: true,
    is_primary: true,
    tracked_by: [],
    source: 'tracked',
    ...over,
  }
}

// The team's channel read: two tracked channels, one registry row the team
// does not track, and a live Slack channel TF holds no row for.
const teamChannels: SlackChannelView[] = [
  channel({ id: 'row-eng', channel_id: 'C1', name: 'eng' }),
  channel({ id: 'row-ops', channel_id: 'G2', name: 'ops', is_private: true }),
  channel({ id: 'row-old', channel_id: 'C3', name: 'old', tracked: false, source: 'sighting' }),
  channel({ channel_id: 'C4', name: 'random', tracked: false, source: 'slack' }),
]

// stubChannelField answers the schema read with a channel_in field and the
// team's channel read with channels, or a 500 when channels is null.
function stubChannelField(channels: SlackChannelView[] | null) {
  const fetchMock = vi.fn((url: string) => {
    if (url.startsWith('/api/event-schemas/')) {
      return Promise.resolve({
        ok: true,
        status: 200,
        ...jsonBody({ fields: [{ name: 'channel_in', type: 'string_list' }] }),
      })
    }
    if (channels === null) {
      return Promise.resolve({ ok: false, status: 500, ...jsonBody({ errors: [] }) })
    }
    return Promise.resolve({
      ok: true,
      status: 200,
      ...jsonBody({ role: 'member', warnings: [], channels }),
    })
  })
  vi.stubGlobal('fetch', fetchMock)
  return fetchMock
}

describe('PredicateEditor channel_in', () => {
  it("offers only the handler team's tracked channels and writes their registry ids", async () => {
    const fetchMock = stubChannelField(teamChannels)
    const onChange = vi.fn()
    render(
      <PredicateEditor eventType="slack:message" teamId="team-b" value={{}} onChange={onChange} />,
    )

    fireEvent.click(await screen.findByRole('checkbox', { name: /#ops/ }))

    expect(onChange).toHaveBeenCalledWith({ channel_in: ['row-ops'] })
    expect(screen.getAllByRole('checkbox')).toHaveLength(2)
    expect(screen.queryByText('#old')).not.toBeInTheDocument()
    expect(screen.queryByText('#random')).not.toBeInTheDocument()
    expect(fetchMock).toHaveBeenCalledWith('/api/slack/teams/team-b/channels', expect.anything())
  })

  it('reads an empty list as every channel the team tracks', async () => {
    stubChannelField(teamChannels)
    render(
      <PredicateEditor
        eventType="slack:message"
        teamId="team-b"
        value={{ channel_in: [] }}
        onChange={vi.fn()}
      />,
    )

    expect(
      await screen.findByText('None selected: matches every channel this team tracks.'),
    ).toBeInTheDocument()
    for (const box of screen.getAllByRole('checkbox')) expect(box).not.toBeChecked()
  })

  it('falls back to the acting team when the handler team is not known yet', async () => {
    const fetchMock = stubChannelField(teamChannels)
    render(<PredicateEditor eventType="slack:message" value={{}} onChange={vi.fn()} />)

    await screen.findByRole('checkbox', { name: /#eng/ })
    expect(fetchMock).toHaveBeenCalledWith(
      '/api/slack/teams/team-acting/channels',
      expect.anything(),
    )
  })

  it('keeps a stored channel the team no longer tracks, named and removable', async () => {
    stubChannelField(teamChannels)
    const onChange = vi.fn()
    render(
      <PredicateEditor
        eventType="slack:message"
        teamId="team-b"
        value={{ channel_in: ['row-eng', 'row-old', 'row-gone'] }}
        onChange={onChange}
      />,
    )

    expect(await screen.findByRole('checkbox', { name: /#eng/ })).toBeChecked()
    expect(screen.getByText('#old · not tracked')).toBeInTheDocument()
    // A row the read does not list at all shows its id.
    expect(screen.getByText('row-gone · not tracked')).toBeInTheDocument()

    fireEvent.click(screen.getByRole('button', { name: 'Remove #old' }))
    expect(onChange).toHaveBeenCalledWith({ channel_in: ['row-eng', 'row-gone'] })
  })

  it('clears the field when the last channel is deselected', async () => {
    stubChannelField(teamChannels)
    const onChange = vi.fn()
    render(
      <PredicateEditor
        eventType="slack:message"
        teamId="team-b"
        value={{ channel_in: ['row-eng'] }}
        onChange={onChange}
      />,
    )

    fireEvent.click(await screen.findByRole('checkbox', { name: /#eng/ }))
    expect(onChange).toHaveBeenCalledWith({})
  })

  it('says so when the team tracks no channels', async () => {
    stubChannelField([])
    render(
      <PredicateEditor eventType="slack:message" teamId="team-b" value={{}} onChange={vi.fn()} />,
    )

    expect(await screen.findByText('This team tracks no Slack channels.')).toBeInTheDocument()
  })

  it('says so when the channels cannot be read', async () => {
    stubChannelField(null)
    render(
      <PredicateEditor eventType="slack:message" teamId="team-b" value={{}} onChange={vi.fn()} />,
    )

    expect(await screen.findByRole('alert')).toHaveTextContent(
      'Could not load this team’s Slack channels.',
    )
  })
})

const teamRepos = [
  { id: 'repo-api', slug: 'acme/api' },
  { id: 'repo-web', slug: 'acme/web' },
]

// stubRepoField answers the schema read with a repo_in field, the team's
// repository list with repos (a 500 when null), and a row read with its slug
// when rows names it (a 404 otherwise).
function stubRepoField(
  repos: { id: string; slug: string }[] | null,
  rows: Record<string, string> = {},
) {
  const fetchMock = vi.fn((url: string) => {
    if (url.startsWith('/api/event-schemas/')) {
      return Promise.resolve({
        ok: true,
        status: 200,
        ...jsonBody({ fields: [{ name: 'repo_in', type: 'string_list' }] }),
      })
    }
    if (url === '/api/repos/list') {
      if (repos === null) {
        return Promise.resolve({ ok: false, status: 500, ...jsonBody({ errors: [] }) })
      }
      return Promise.resolve({
        ok: true,
        status: 200,
        ...jsonBody({ items: repos, next_page_token: '', total_count: repos.length }),
      })
    }
    const id = decodeURIComponent(url.slice('/api/repos/'.length))
    if (id in rows) {
      return Promise.resolve({ ok: true, status: 200, ...jsonBody({ id, slug: rows[id] }) })
    }
    return Promise.resolve({ ok: false, status: 404, ...jsonBody({ errors: [] }) })
  })
  vi.stubGlobal('fetch', fetchMock)
  return fetchMock
}

describe('PredicateEditor repo_in', () => {
  it("offers the handler team's tracked repositories and writes their registry ids", async () => {
    const fetchMock = stubRepoField(teamRepos)
    const onChange = vi.fn()
    render(
      <PredicateEditor
        eventType="github:pr:opened"
        teamId="team-b"
        value={{}}
        onChange={onChange}
      />,
    )

    fireEvent.click(await screen.findByRole('checkbox', { name: 'acme/web' }))

    expect(onChange).toHaveBeenCalledWith({ repo_in: ['repo-web'] })
    expect(screen.getAllByRole('checkbox')).toHaveLength(2)
    expect(fetchMock).toHaveBeenCalledWith(
      '/api/repos/list',
      expect.objectContaining({ body: expect.stringContaining('"team_id":"team-b"') }),
    )
  })

  it('falls back to the acting team when the handler team is not known yet', async () => {
    const fetchMock = stubRepoField(teamRepos)
    render(<PredicateEditor eventType="github:pr:opened" value={{}} onChange={vi.fn()} />)

    await screen.findByRole('checkbox', { name: 'acme/api' })
    expect(fetchMock).toHaveBeenCalledWith(
      '/api/repos/list',
      expect.objectContaining({ body: expect.stringContaining('"team_id":"team-acting"') }),
    )
  })

  it('keeps a stored repository the team no longer tracks, named and removable', async () => {
    stubRepoField(teamRepos, { 'repo-old': 'acme/old' })
    const onChange = vi.fn()
    render(
      <PredicateEditor
        eventType="github:pr:opened"
        teamId="team-b"
        value={{ repo_in: ['repo-api', 'repo-old', 'repo-gone'] }}
        onChange={onChange}
      />,
    )

    expect(await screen.findByRole('checkbox', { name: 'acme/api' })).toBeChecked()
    expect(await screen.findByText('acme/old · not tracked')).toBeInTheDocument()
    // A row the caller cannot read shows its id.
    expect(screen.getByText('repo-gone · not tracked')).toBeInTheDocument()

    fireEvent.click(screen.getByRole('button', { name: 'Remove acme/old' }))
    expect(onChange).toHaveBeenCalledWith({ repo_in: ['repo-api', 'repo-gone'] })
  })

  it('clears the field when the last repository is deselected', async () => {
    stubRepoField(teamRepos)
    const onChange = vi.fn()
    render(
      <PredicateEditor
        eventType="github:pr:opened"
        teamId="team-b"
        value={{ repo_in: ['repo-api'] }}
        onChange={onChange}
      />,
    )

    fireEvent.click(await screen.findByRole('checkbox', { name: 'acme/api' }))
    expect(onChange).toHaveBeenCalledWith({})
  })

  it('says so when the team tracks no repositories', async () => {
    stubRepoField([])
    render(
      <PredicateEditor
        eventType="github:pr:opened"
        teamId="team-b"
        value={{}}
        onChange={vi.fn()}
      />,
    )

    expect(await screen.findByText('This team tracks no GitHub repositories.')).toBeInTheDocument()
  })

  it('says so when the repositories cannot be read', async () => {
    stubRepoField(null)
    render(
      <PredicateEditor
        eventType="github:pr:opened"
        teamId="team-b"
        value={{}}
        onChange={vi.fn()}
      />,
    )

    expect(await screen.findByRole('alert')).toHaveTextContent(
      'Could not load this team’s repositories.',
    )
  })
})

const trackedLinearTeams = [
  { id: 'lt-eng', key: 'ENG', name: 'Engineering' },
  { id: 'lt-ops', key: 'OPS', name: 'Operations' },
]

// stubLinearTeamField answers the schema read with the named Linear team
// fields, the team-settings read with linearTeams as its linear_teams (a 500
// when null), and a Linear team read with its key when known names it (a 404
// otherwise).
function stubLinearTeamField(
  linearTeams: { id: string; key: string; name: string }[] | null,
  known: Record<string, string> = {},
  fields: string[] = ['linear_team_id_in'],
) {
  const fetchMock = vi.fn((url: string) => {
    if (url.startsWith('/api/event-schemas/')) {
      return Promise.resolve({
        ok: true,
        status: 200,
        ...jsonBody({ fields: fields.map((name) => ({ name, type: 'string_list' })) }),
      })
    }
    if (url.startsWith('/api/teams/')) {
      if (linearTeams === null) {
        return Promise.resolve({ ok: false, status: 500, ...jsonBody({ errors: [] }) })
      }
      return Promise.resolve({
        ok: true,
        status: 200,
        ...jsonBody({ jira_projects: [], linear_teams: linearTeams }),
      })
    }
    const id = decodeURIComponent(url.slice(url.lastIndexOf('/') + 1))
    if (url.includes('/linear/teams/') && id in known) {
      return Promise.resolve({
        ok: true,
        status: 200,
        ...jsonBody({ id, key: known[id], name: known[id], private: false }),
      })
    }
    return Promise.resolve({ ok: false, status: 404, ...jsonBody({ errors: [] }) })
  })
  vi.stubGlobal('fetch', fetchMock)
  return fetchMock
}

describe('PredicateEditor linear_team_id_in', () => {
  it("offers the handler team's tracked Linear teams and writes their ids", async () => {
    const fetchMock = stubLinearTeamField(trackedLinearTeams)
    const onChange = vi.fn()
    render(
      <PredicateEditor
        eventType="linear:issue:assigned"
        teamId="team-b"
        value={{}}
        onChange={onChange}
      />,
    )

    fireEvent.click(await screen.findByRole('checkbox', { name: 'OPS Operations' }))

    expect(onChange).toHaveBeenCalledWith({ linear_team_id_in: ['lt-ops'] })
    expect(screen.getAllByRole('checkbox')).toHaveLength(2)
    expect(fetchMock).toHaveBeenCalledWith('/api/teams/team-b/settings', expect.anything())
  })

  it('falls back to the acting team when the handler team is not known yet', async () => {
    const fetchMock = stubLinearTeamField(trackedLinearTeams)
    render(<PredicateEditor eventType="linear:issue:assigned" value={{}} onChange={vi.fn()} />)

    await screen.findByRole('checkbox', { name: 'ENG Engineering' })
    expect(fetchMock).toHaveBeenCalledWith('/api/teams/team-acting/settings', expect.anything())
  })

  it('offers the same teams for the team an issue left', async () => {
    const onChange = vi.fn()
    stubLinearTeamField(trackedLinearTeams, {}, ['old_linear_team_id_in'])
    render(
      <PredicateEditor
        eventType="linear:issue:identifier_changed"
        teamId="team-b"
        value={{}}
        onChange={onChange}
      />,
    )

    fireEvent.click(await screen.findByRole('checkbox', { name: 'ENG Engineering' }))
    expect(onChange).toHaveBeenCalledWith({ old_linear_team_id_in: ['lt-eng'] })
  })

  it('keeps a stored Linear team the team no longer tracks, named and removable', async () => {
    stubLinearTeamField(trackedLinearTeams, { 'lt-old': 'OLD' })
    const onChange = vi.fn()
    render(
      <PredicateEditor
        eventType="linear:issue:assigned"
        teamId="team-b"
        value={{ linear_team_id_in: ['lt-eng', 'lt-old', 'lt-gone'] }}
        onChange={onChange}
      />,
    )

    expect(await screen.findByRole('checkbox', { name: 'ENG Engineering' })).toBeChecked()
    expect(await screen.findByText('OLD · not tracked')).toBeInTheDocument()
    // A team Linear does not answer for shows its id.
    expect(screen.getByText('lt-gone · not tracked')).toBeInTheDocument()

    fireEvent.click(screen.getByRole('button', { name: 'Remove OLD' }))
    expect(onChange).toHaveBeenCalledWith({ linear_team_id_in: ['lt-eng', 'lt-gone'] })
  })

  it('clears the field when the last Linear team is deselected', async () => {
    stubLinearTeamField(trackedLinearTeams)
    const onChange = vi.fn()
    render(
      <PredicateEditor
        eventType="linear:issue:assigned"
        teamId="team-b"
        value={{ linear_team_id_in: ['lt-eng'] }}
        onChange={onChange}
      />,
    )

    fireEvent.click(await screen.findByRole('checkbox', { name: 'ENG Engineering' }))
    expect(onChange).toHaveBeenCalledWith({})
  })

  it('says so when the team tracks no Linear teams', async () => {
    stubLinearTeamField([])
    render(
      <PredicateEditor
        eventType="linear:issue:assigned"
        teamId="team-b"
        value={{}}
        onChange={vi.fn()}
      />,
    )

    expect(await screen.findByText('This team tracks no Linear teams.')).toBeInTheDocument()
  })

  it('says so when the Linear teams cannot be read', async () => {
    stubLinearTeamField(null)
    render(
      <PredicateEditor
        eventType="linear:issue:assigned"
        teamId="team-b"
        value={{}}
        onChange={vi.fn()}
      />,
    )

    expect(await screen.findByRole('alert')).toHaveTextContent(
      'Could not load this team’s Linear teams.',
    )
  })
})

const trackedJiraProjects = [
  { id: 'jp-sky', key: 'SKY', name: 'Sky' },
  { id: 'jp-ops', key: 'OPS', name: 'Operations' },
]

// stubJiraProjectField answers the schema read with the named Jira project
// fields and the team-settings read with projects as its jira_projects (a 500
// when null). Each project carries the rules the read renders beside it.
function stubJiraProjectField(
  projects: { id: string; key: string; name: string }[] | null,
  fields: string[] = ['jira_project_id_in'],
) {
  const rule = { members: [], canonical: null }
  const fetchMock = vi.fn((url: string) => {
    if (url.startsWith('/api/event-schemas/')) {
      return Promise.resolve({
        ok: true,
        status: 200,
        ...jsonBody({ fields: fields.map((name) => ({ name, type: 'string_list' })) }),
      })
    }
    if (projects === null) {
      return Promise.resolve({ ok: false, status: 500, ...jsonBody({ errors: [] }) })
    }
    return Promise.resolve({
      ok: true,
      status: 200,
      ...jsonBody({
        jira_projects: projects.map((p) => ({
          ...p,
          pickup: rule,
          in_progress: rule,
          in_review: rule,
          done: rule,
        })),
        linear_teams: [],
      }),
    })
  })
  vi.stubGlobal('fetch', fetchMock)
  return fetchMock
}

describe('PredicateEditor jira_project_id_in', () => {
  it("offers the handler team's tracked Jira projects and writes their registry ids", async () => {
    const fetchMock = stubJiraProjectField(trackedJiraProjects)
    const onChange = vi.fn()
    render(
      <PredicateEditor
        eventType="jira:issue:assigned"
        teamId="team-b"
        value={{}}
        onChange={onChange}
      />,
    )

    fireEvent.click(await screen.findByRole('checkbox', { name: 'OPS Operations' }))

    expect(onChange).toHaveBeenCalledWith({ jira_project_id_in: ['jp-ops'] })
    expect(screen.getAllByRole('checkbox')).toHaveLength(2)
    expect(fetchMock).toHaveBeenCalledWith('/api/teams/team-b/settings', expect.anything())
  })

  it('falls back to the acting team when the handler team is not known yet', async () => {
    const fetchMock = stubJiraProjectField(trackedJiraProjects)
    render(<PredicateEditor eventType="jira:issue:assigned" value={{}} onChange={vi.fn()} />)

    await screen.findByRole('checkbox', { name: 'SKY Sky' })
    expect(fetchMock).toHaveBeenCalledWith('/api/teams/team-acting/settings', expect.anything())
  })

  it('offers the same projects for the project an issue left', async () => {
    const onChange = vi.fn()
    stubJiraProjectField(trackedJiraProjects, ['old_jira_project_id_in'])
    render(
      <PredicateEditor
        eventType="jira:issue:key_changed"
        teamId="team-b"
        value={{}}
        onChange={onChange}
      />,
    )

    fireEvent.click(await screen.findByRole('checkbox', { name: 'SKY Sky' }))
    expect(onChange).toHaveBeenCalledWith({ old_jira_project_id_in: ['jp-sky'] })
  })

  it('keeps a stored project the team no longer tracks, removable', async () => {
    stubJiraProjectField(trackedJiraProjects)
    const onChange = vi.fn()
    render(
      <PredicateEditor
        eventType="jira:issue:assigned"
        teamId="team-b"
        value={{ jira_project_id_in: ['jp-sky', 'jp-old'] }}
        onChange={onChange}
      />,
    )

    expect(await screen.findByRole('checkbox', { name: 'SKY Sky' })).toBeChecked()
    const chip = screen.getByText('Untracked Jira project')
    expect(chip).toHaveAttribute('title', expect.stringContaining('jp-old'))

    fireEvent.click(screen.getByRole('button', { name: 'Remove untracked Jira project' }))
    expect(onChange).toHaveBeenCalledWith({ jira_project_id_in: ['jp-sky'] })
  })

  it('clears the field when the last project is deselected', async () => {
    stubJiraProjectField(trackedJiraProjects)
    const onChange = vi.fn()
    render(
      <PredicateEditor
        eventType="jira:issue:assigned"
        teamId="team-b"
        value={{ jira_project_id_in: ['jp-sky'] }}
        onChange={onChange}
      />,
    )

    fireEvent.click(await screen.findByRole('checkbox', { name: 'SKY Sky' }))
    expect(onChange).toHaveBeenCalledWith({})
  })

  it('says so when the team tracks no Jira projects', async () => {
    stubJiraProjectField([])
    render(
      <PredicateEditor
        eventType="jira:issue:assigned"
        teamId="team-b"
        value={{}}
        onChange={vi.fn()}
      />,
    )

    expect(await screen.findByText('This team tracks no Jira projects.')).toBeInTheDocument()
  })

  it('says so when the Jira projects cannot be read', async () => {
    stubJiraProjectField(null)
    render(
      <PredicateEditor
        eventType="jira:issue:assigned"
        teamId="team-b"
        value={{}}
        onChange={vi.fn()}
      />,
    )

    expect(await screen.findByRole('alert')).toHaveTextContent(
      'Could not load this team’s Jira projects.',
    )
  })
})
