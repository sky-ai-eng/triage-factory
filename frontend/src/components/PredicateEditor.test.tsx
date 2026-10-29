import { useState } from 'react'
import { describe, it, expect, vi, beforeEach } from 'vitest'
import { render, screen, fireEvent } from '@testing-library/react'
import PredicateEditor from './PredicateEditor'
import { jsonBody } from '../test/apiResponse'
import { LOCAL_DEFAULT_ORG_ID } from '../lib/githubApp'
import type { MeResponse, SlackChannelView, TeamMember } from '../types'

// The viewer has an identity on every source, each a different value, so a
// test can tell which column an allowlist was filled from.
const me: MeResponse = {
  id: 'user-1',
  github_username: 'octocat',
  github_user_id: '583231',
  jira_account_id: '557058:abc-me',
  jira_display_name: 'Me on Jira',
  linear_user_id: 'lin-me',
  linear_display_name: 'Me on Linear',
  orgs: [],
  teams: [],
  org_creation_enabled: true,
  can_create_org: true,
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
    github_user_id: null,
    jira_account_id: null,
    linear_user_id: null,
    linear_display_name: null,
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
    ['github:pr:opened', 'author_id_in', '583231'],
    ['github:pr:review_requested', 'requested_user_id_in', '583231'],
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

  it('lists team members by their Linear name and writes their Linear user id', async () => {
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
        linear_display_name: 'ada.l',
      }),
      member({ user_id: 'user-3', display_name: 'Bob', github_username: 'bob-gh' }),
    ]
    const onChange = vi.fn()
    render(<PredicateEditor eventType="linear:issue:assigned" value={{}} onChange={onChange} />)

    fireEvent.focus(await screen.findByRole('combobox'))

    expect(screen.getByText('ada.l')).toBeInTheDocument()
    expect(screen.getByText('lin-me')).toBeInTheDocument()
    expect(screen.queryByText('ada-gh')).not.toBeInTheDocument()
    expect(screen.getByText('— no Linear identity')).toBeInTheDocument()

    fireEvent.mouseDown(screen.getByText('ada.l'))
    expect(onChange).toHaveBeenCalledWith({ assignee_in: ['lin-ada'] })
  })
})

// stubField answers the schema read with one string_list field and every
// other read with route's [status, body].
function stubField(field: string, route: (url: string) => [number, unknown]) {
  const fetchMock = vi.fn((url: string) => {
    const [status, body] = url.startsWith('/api/event-schemas/')
      ? [200, { fields: [{ name: field, type: 'string_list' }] }]
      : route(url)
    return Promise.resolve({ ok: status < 400, status, ...jsonBody(body) })
  })
  vi.stubGlobal('fetch', fetchMock)
  return fetchMock
}

const accounts = `/api/orgs/${LOCAL_DEFAULT_ORG_ID}/github/users/`
// The accounts the org's GitHub answers for, by login and by id.
const githubAccounts = [
  { github_user_id: '583231', login: 'octocat' },
  { github_user_id: '1001', login: 'ada-gh' },
  { github_user_id: '29139614', login: 'renovate[bot]' },
]
const githubTeam = [
  member({
    user_id: 'user-1',
    display_name: 'Me',
    github_username: 'octocat',
    github_user_id: '583231',
    is_current_user: true,
  }),
  member({
    user_id: 'user-2',
    display_name: 'Ada',
    github_username: 'ada-gh',
    github_user_id: '1001',
  }),
  member({ user_id: 'user-3', display_name: 'Bob' }),
]

function renderAuthorField(value: Record<string, string[]> = {}, members = githubTeam) {
  roster.members = members
  const fetchMock = stubField('author_id_in', (url) => {
    const account = githubAccounts.find(
      (a) =>
        url === `${accounts}by-name/${encodeURIComponent(a.login)}` ||
        url === `${accounts}${a.github_user_id}`,
    )
    return account ? [200, account] : [404, { errors: [] }]
  })
  const onChange = vi.fn()
  render(<PredicateEditor eventType="github:pr:opened" value={value} onChange={onChange} />)
  return { fetchMock, onChange }
}

describe('PredicateEditor GitHub identities', () => {
  it('lists team members by login and writes their GitHub account id', async () => {
    const { onChange } = renderAuthorField()

    fireEvent.focus(await screen.findByRole('combobox'))
    expect(screen.getByText('— no GitHub identity')).toBeInTheDocument()
    fireEvent.mouseDown(screen.getByText('ada-gh'))
    expect(onChange).toHaveBeenCalledWith({ author_id_in: ['1001'] })
  })

  it('looks a typed login up on GitHub and writes its account id', async () => {
    const { fetchMock, onChange } = renderAuthorField()

    fireEvent.change(await screen.findByRole('combobox'), { target: { value: 'renovate[bot]' } })
    fireEvent.mouseDown(screen.getByText(/as GitHub account/))
    await vi.waitFor(() => expect(onChange).toHaveBeenCalledWith({ author_id_in: ['29139614'] }))
    expect(fetchMock).toHaveBeenCalledWith(
      `${accounts}by-name/renovate%5Bbot%5D`,
      expect.anything(),
    )
  })

  it('refuses a login no GitHub account holds, beside the field', async () => {
    const { onChange } = renderAuthorField()

    const input = await screen.findByRole('combobox')
    fireEvent.change(input, { target: { value: 'nobody-here' } })
    fireEvent.keyDown(input, { key: 'Enter' })
    expect(await screen.findByRole('alert')).toHaveTextContent(
      'No GitHub account is called nobody-here.',
    )
    expect(onChange).not.toHaveBeenCalled()
  })

  it('names a stored account id by member, else by GitHub, else by the id', async () => {
    renderAuthorField({ author_id_in: ['1001', '29139614', '42'] })

    expect(await screen.findByText('Ada · ada-gh')).toBeInTheDocument()
    expect(await screen.findByText('renovate[bot]')).toBeInTheDocument()
    expect(screen.getByText('42')).toBeInTheDocument()
  })

  it('adds a looked-up login to the value as it stands when GitHub answers', async () => {
    roster.members = githubTeam
    let answer = (_account: unknown) => {}
    vi.stubGlobal(
      'fetch',
      vi.fn((url: string) => {
        if (url.startsWith('/api/event-schemas/')) {
          const fields = [{ name: 'author_id_in', type: 'string_list' }]
          return Promise.resolve({ ok: true, status: 200, ...jsonBody({ fields }) })
        }
        if (url === `${accounts}by-name/renovate%5Bbot%5D`) {
          return new Promise((resolve) => {
            answer = (account) => resolve({ ok: true, status: 200, ...jsonBody(account) })
          })
        }
        return Promise.resolve({ ok: false, status: 404, ...jsonBody({ errors: [] }) })
      }),
    )
    const onChange = vi.fn()
    function Editor() {
      const [value, setValue] = useState<Record<string, unknown>>({ author_id_in: ['1001'] })
      return (
        <PredicateEditor
          eventType="github:pr:opened"
          value={value}
          onChange={(v) => {
            onChange(v)
            setValue(v)
          }}
        />
      )
    }
    render(<Editor />)

    const input = await screen.findByRole('combobox')
    fireEvent.change(input, { target: { value: 'renovate[bot]' } })
    fireEvent.keyDown(input, { key: 'Enter' })
    // Ada is removed while GitHub is being asked about renovate[bot].
    fireEvent.click(screen.getByRole('button', { name: 'Remove ada-gh' }))
    answer({ github_user_id: '29139614', login: 'renovate[bot]' })
    await vi.waitFor(() =>
      expect(onChange).toHaveBeenLastCalledWith({ author_id_in: ['29139614'] }),
    )
  })

  it('looks the viewer up by login when their binding recorded no account id', async () => {
    const stored = me.github_user_id
    me.github_user_id = undefined
    try {
      const { onChange } = renderAuthorField({}, [])

      const toggle = await screen.findByRole('button', { pressed: false })
      await vi.waitFor(() => expect(toggle).toBeEnabled())
      fireEvent.click(toggle)
      expect(onChange).toHaveBeenCalledWith({ author_id_in: ['583231'] })
    } finally {
      me.github_user_id = stored
    }
  })
})

// renderTeamField mounts the editor on requested_team_id_in for team-b, whose
// GitHub-team mappings read answers groups, or 500 when null.
function renderTeamField(groups: unknown[] | null, value: Record<string, string[]> = {}) {
  const fetchMock = stubField('requested_team_id_in', () =>
    groups ? [200, { groups, candidates: [] }] : [500, { errors: [] }],
  )
  const onChange = vi.fn()
  render(
    <PredicateEditor
      eventType="github:pr:review_requested"
      teamId="team-b"
      value={value}
      onChange={onChange}
    />,
  )
  return { fetchMock, onChange }
}

const teamGroups = [
  { github_team_id: '11', github_org_id: '7', org_login: 'acme', team_slug: 'backend' },
  { github_team_id: '', github_org_id: '', org_login: 'acme', team_slug: 'legacy' },
]

const linearUsers = `/api/orgs/${LOCAL_DEFAULT_ORG_ID}/linear/users/`
// The people of the org's Linear workspace, as the users reads answer them.
const linearPeople = [
  {
    id: '11111111-1111-4111-8111-111111111111',
    name: 'Ada Lovelace',
    display_name: 'ada',
    active: true,
  },
  {
    id: '22222222-2222-4222-8222-222222222222',
    name: 'Adam Smith',
    display_name: 'asmith',
    active: true,
  },
  {
    id: '33333333-3333-4333-8333-333333333333',
    name: 'Grace Hopper',
    display_name: 'grace',
    active: true,
  },
  {
    id: '44444444-4444-4444-8444-444444444444',
    name: 'Adabot',
    display_name: 'adabot',
    active: true,
    app: true,
  },
].map((u) => ({ app: false, ...u }))
const [linAda, linAdam, linGrace, linBot] = linearPeople.map((u) => u.id)
const linearTeam = [
  member({
    user_id: 'user-1',
    display_name: 'Me',
    linear_user_id: 'lin-me',
    linear_display_name: 'me',
    is_current_user: true,
  }),
  member({
    user_id: 'user-2',
    display_name: 'Bob',
    linear_user_id: 'lin-bob',
    linear_display_name: 'bob',
  }),
]

function renderLinearField(
  value: Record<string, string[]> = {},
  field = 'assignee_in',
  eventType = 'linear:issue:assigned',
) {
  roster.members = linearTeam
  const fetchMock = vi.fn((url: string, init?: { body?: string }) => {
    const answer = (status: number, body: unknown) =>
      Promise.resolve({ ok: status < 400, status, ...jsonBody(body) })
    if (url.startsWith('/api/event-schemas/')) {
      return answer(200, { fields: [{ name: field, type: 'string_list' }] })
    }
    if (url === `${linearUsers}list`) {
      const q = (JSON.parse(init?.body ?? '{}').q as string).toLowerCase()
      const items = linearPeople.filter(
        (u) => u.name.toLowerCase().includes(q) || u.display_name.includes(q),
      )
      return answer(200, { items, next_page_token: '', total_count: null })
    }
    const user = linearPeople.find((u) => url === `${linearUsers}${u.id}`)
    return user ? answer(200, user) : answer(404, { errors: [] })
  })
  vi.stubGlobal('fetch', fetchMock)
  const onChange = vi.fn()
  render(<PredicateEditor eventType={eventType} value={value} onChange={onChange} />)
  return { fetchMock, onChange }
}

describe('PredicateEditor Linear identities', () => {
  it('finds a typed name in Linear and writes the id of the person picked', async () => {
    const { fetchMock, onChange } = renderLinearField()

    const input = await screen.findByRole('combobox')
    fireEvent.change(input, { target: { value: 'ada' } })
    expect(screen.getByText(/Find “ada” in Linear/)).toBeInTheDocument()
    fireEvent.keyDown(input, { key: 'Enter' })

    expect(await screen.findByText('Adam Smith')).toBeInTheDocument()
    expect(screen.getByText('Ada Lovelace')).toBeInTheDocument()
    expect(screen.queryByText('Bob')).not.toBeInTheDocument()
    // An integration's app user is no assignee.
    expect(screen.queryByText('Adabot')).not.toBeInTheDocument()
    expect(fetchMock).toHaveBeenCalledWith(
      `${linearUsers}list`,
      expect.objectContaining({ body: JSON.stringify({ q: 'ada', page_size: 10 }) }),
    )
    expect(onChange).not.toHaveBeenCalled()

    fireEvent.keyDown(input, { key: 'Enter' })
    expect(onChange).toHaveBeenCalledWith({ assignee_in: [linAda] })
  })

  it('refuses a name no Linear user matches, beside the field', async () => {
    const { onChange } = renderLinearField()

    const input = await screen.findByRole('combobox')
    fireEvent.change(input, { target: { value: 'zed' } })
    fireEvent.keyDown(input, { key: 'Enter' })
    expect(await screen.findByRole('alert')).toHaveTextContent('No Linear user matches “zed”.')
    expect(onChange).not.toHaveBeenCalled()
  })

  it('offers an app user, marked, where the field can name one', async () => {
    const { onChange } = renderLinearField({}, 'creator_in', 'linear:issue:available')

    const input = await screen.findByRole('combobox')
    fireEvent.change(input, { target: { value: 'adabot' } })
    fireEvent.keyDown(input, { key: 'Enter' })
    expect(await screen.findByText('adabot · app')).toBeInTheDocument()
    fireEvent.mouseDown(screen.getByText('Adabot'))
    expect(onChange).toHaveBeenCalledWith({ creator_in: [linBot] })
  })

  it('says an assignee filter takes people when only apps match', async () => {
    const { onChange } = renderLinearField()

    const input = await screen.findByRole('combobox')
    fireEvent.change(input, { target: { value: 'adabot' } })
    fireEvent.keyDown(input, { key: 'Enter' })
    expect(await screen.findByRole('alert')).toHaveTextContent(
      'Only Linear apps match “adabot”, and this filter takes people.',
    )
    expect(onChange).not.toHaveBeenCalled()
  })

  it('reads a typed Linear user id and writes it', async () => {
    const { onChange } = renderLinearField()

    const input = await screen.findByRole('combobox')
    fireEvent.change(input, { target: { value: linGrace } })
    fireEvent.keyDown(input, { key: 'Enter' })
    await vi.waitFor(() => expect(onChange).toHaveBeenCalledWith({ assignee_in: [linGrace] }))
  })

  it('names a stored id by member, else by Linear, else by the id', async () => {
    renderLinearField({ assignee_in: ['lin-bob', linAdam, 'alice'] })

    expect(await screen.findByText('Bob · bob')).toBeInTheDocument()
    expect(await screen.findByText('asmith')).toBeInTheDocument()
    expect(screen.getByText('alice')).toBeInTheDocument()
  })
})

describe('PredicateEditor requested_team_id_in', () => {
  it("offers the handler team's GitHub-team mappings and writes their GitHub team ids", async () => {
    const { fetchMock, onChange } = renderTeamField(teamGroups)

    fireEvent.click(await screen.findByRole('checkbox', { name: 'acme/backend' }))
    expect(onChange).toHaveBeenCalledWith({ requested_team_id_in: ['11'] })
    // A mapping without its ids yet has nothing to filter by.
    expect(screen.getAllByRole('checkbox')).toHaveLength(1)
    expect(fetchMock).toHaveBeenCalledWith('/api/teams/team-b/github-groups', expect.anything())
  })

  it('keeps a stored team the team no longer maps, removable', async () => {
    const { onChange } = renderTeamField(teamGroups, { requested_team_id_in: ['11', '99'] })

    expect(await screen.findByRole('checkbox', { name: 'acme/backend' })).toBeChecked()
    expect(screen.getByText('99 · not mapped')).toBeInTheDocument()
    fireEvent.click(screen.getByRole('button', { name: 'Remove 99' }))
    expect(onChange).toHaveBeenCalledWith({ requested_team_id_in: ['11'] })
  })

  it('says so when the mappings cannot be read', async () => {
    renderTeamField(null)

    expect(await screen.findByRole('alert')).toHaveTextContent(
      'Could not load this team’s GitHub teams.',
    )
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
