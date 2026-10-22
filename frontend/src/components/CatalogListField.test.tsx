import { describe, it, expect, vi } from 'vitest'
import { render, screen, fireEvent, within } from '@testing-library/react'
import PredicateEditor from './PredicateEditor'
import { jsonBody } from '../test/apiResponse'
import { LOCAL_DEFAULT_ORG_ID } from '../lib/githubApp'

// The acting team the field falls back to when no teamId is passed.
vi.mock('../hooks/useTeams', () => ({
  useTeams: () => ({ teams: [{ id: 'team-acting' }], lastActingTeamId: '', loaded: true }),
  pickerDefault: (teams: { id: string }[]) => teams[0]?.id ?? '',
}))
vi.mock('../hooks/useDeploymentConfig', () => ({
  useMe: () => ({ me: null, loading: false, error: null }),
  useTeamMembers: () => ({ members: [], bot: null, loading: false, error: null }),
}))

const org = `/api/orgs/${LOCAL_DEFAULT_ORG_ID}`
const rule = { members: [], canonical: null }

// The team tracks SKY and OPS on Jira, whose workflows share To Do, and ENG on
// Linear. OPS-old has not been resolved against Jira, so it has no Jira id.
const settings = {
  jira_projects: [
    { id: 'jp-sky', jira_id: '100', key: 'SKY', name: 'Sky' },
    { id: 'jp-ops', jira_id: '200', key: 'OPS', name: 'Operations' },
    { id: 'jp-unresolved', key: 'OLD', name: '' },
  ].map((p) => ({ ...p, pickup: rule, in_progress: rule, in_review: rule, done: rule })),
  linear_teams: [{ id: 'lt-eng', key: 'ENG', name: 'Engineering' }],
}

const lists: Record<string, unknown[]> = {
  [`${org}/jira/projects/100/statuses/list`]: [
    { id: '10000', name: 'To Do' },
    { id: '10003', name: 'In Review' },
  ],
  [`${org}/jira/projects/200/statuses/list`]: [
    { id: '10000', name: 'To Do' },
    { id: '20000', name: 'Triage' },
  ],
  [`${org}/jira/projects/100/issue-types/list`]: [{ id: '10004', name: 'Bug' }],
  [`${org}/jira/projects/200/issue-types/list`]: [{ id: '10100', name: 'Incident' }],
  [`${org}/jira/priorities/list`]: [
    { id: '1', name: 'Highest' },
    { id: '2', name: 'High' },
  ],
  [`${org}/linear/teams/lt-eng/states/list`]: [
    { id: 'st-review', name: 'In Review', type: 'started', position: 2 },
    { id: 'st-todo', name: 'Todo', type: 'unstarted', position: 1 },
  ],
  [`${org}/linear/teams/lt-eng/labels/list`]: [
    { id: 'lb-bug', name: 'Bug', group: null, is_group: false },
    { id: 'lb-diff', name: 'Difficulty', group: null, is_group: true },
    { id: 'lb-hard', name: 'Hard', group: { id: 'lb-diff', name: 'Difficulty' }, is_group: false },
  ],
}

// stubCatalog answers the schema read with one string_list field, the team
// settings read with settings, and each catalog list with its rows; failing
// names a list that answers 500.
function stubCatalog(field: string, failing = '') {
  const fetchMock = vi.fn((url: string) => {
    if (url.startsWith('/api/event-schemas/')) {
      return Promise.resolve({
        ok: true,
        status: 200,
        ...jsonBody({ fields: [{ name: field, type: 'string_list' }] }),
      })
    }
    if (url.startsWith('/api/teams/')) {
      return Promise.resolve({ ok: true, status: 200, ...jsonBody(settings) })
    }
    if (url in lists && url !== failing) {
      return Promise.resolve({
        ok: true,
        status: 200,
        ...jsonBody({ items: lists[url], next_page_token: '', total_count: null }),
      })
    }
    return Promise.resolve({ ok: false, status: 500, ...jsonBody({ errors: [] }) })
  })
  vi.stubGlobal('fetch', fetchMock)
  return fetchMock
}

describe('PredicateEditor catalog fields', () => {
  it("offers the statuses of the team's tracked Jira projects, grouped by project, and writes ids", async () => {
    const fetchMock = stubCatalog('status_id_in')
    const onChange = vi.fn()
    render(
      <PredicateEditor
        eventType="jira:issue:assigned"
        teamId="team-b"
        value={{}}
        onChange={onChange}
      />,
    )

    const ops = await screen.findByRole('group', { name: 'OPS · Operations' })
    fireEvent.click(within(ops).getByRole('checkbox', { name: 'Triage' }))
    expect(onChange).toHaveBeenCalledWith({ status_id_in: ['20000'] })
    expect(
      within(screen.getByRole('group', { name: 'SKY · Sky' })).getAllByRole('checkbox'),
    ).toHaveLength(2)
    expect(screen.queryByRole('group', { name: /OLD/ })).not.toBeInTheDocument()
    expect(fetchMock).toHaveBeenCalledWith('/api/teams/team-b/settings', expect.anything())
  })

  it('checks a status every project shares wherever it appears', async () => {
    stubCatalog('new_status_id_in')
    render(
      <PredicateEditor
        eventType="jira:issue:status_changed"
        teamId="team-b"
        value={{ new_status_id_in: ['10000'] }}
        onChange={vi.fn()}
      />,
    )

    const boxes = await screen.findAllByRole('checkbox', { name: 'To Do' })
    expect(boxes).toHaveLength(2)
    boxes.forEach((box) => expect(box).toBeChecked())
  })

  it("offers the Jira instance's priorities and issue types of the tracked projects", async () => {
    stubCatalog('priority_id_in')
    const onChange = vi.fn()
    const { unmount } = render(
      <PredicateEditor
        eventType="jira:issue:available"
        teamId="team-b"
        value={{}}
        onChange={onChange}
      />,
    )
    fireEvent.click(await screen.findByRole('checkbox', { name: 'High' }))
    expect(onChange).toHaveBeenCalledWith({ priority_id_in: ['2'] })
    unmount()

    stubCatalog('issue_type_id_in')
    render(
      <PredicateEditor
        eventType="jira:issue:available"
        teamId="team-b"
        value={{}}
        onChange={onChange}
      />,
    )
    fireEvent.click(await screen.findByRole('checkbox', { name: 'Incident' }))
    expect(onChange).toHaveBeenCalledWith({ issue_type_id_in: ['10100'] })
  })

  it("offers the states of the team's tracked Linear teams on a Linear event", async () => {
    stubCatalog('status_id_in')
    const onChange = vi.fn()
    render(<PredicateEditor eventType="linear:issue:assigned" value={{}} onChange={onChange} />)

    const eng = await screen.findByRole('group', { name: 'ENG · Engineering' })
    fireEvent.click(within(eng).getByRole('checkbox', { name: 'In Review' }))
    expect(onChange).toHaveBeenCalledWith({ status_id_in: ['st-review'] })
  })

  it('offers labels and label groups apart, a grouped label under its group', async () => {
    stubCatalog('label_id_in')
    const onChange = vi.fn()
    const { unmount } = render(
      <PredicateEditor
        eventType="linear:issue:label_added"
        teamId="team-b"
        value={{}}
        onChange={onChange}
      />,
    )
    fireEvent.click(await screen.findByRole('checkbox', { name: 'Difficulty / Hard' }))
    expect(onChange).toHaveBeenCalledWith({ label_id_in: ['lb-hard'] })
    expect(screen.queryByRole('checkbox', { name: 'Difficulty' })).not.toBeInTheDocument()
    unmount()

    stubCatalog('label_group_id_in')
    render(
      <PredicateEditor
        eventType="linear:issue:label_added"
        teamId="team-b"
        value={{}}
        onChange={onChange}
      />,
    )
    fireEvent.click(await screen.findByRole('checkbox', { name: 'Difficulty' }))
    expect(onChange).toHaveBeenCalledWith({ label_group_id_in: ['lb-diff'] })
    expect(screen.queryByRole('checkbox', { name: 'Bug' })).not.toBeInTheDocument()
  })

  it('keeps a stored id nothing offers any more, removable, and clears the field when emptied', async () => {
    stubCatalog('status_id_in')
    const onChange = vi.fn()
    render(
      <PredicateEditor
        eventType="jira:issue:assigned"
        teamId="team-b"
        value={{ status_id_in: ['10003', '99999'] }}
        onChange={onChange}
      />,
    )

    expect(await screen.findByRole('checkbox', { name: 'In Review' })).toBeChecked()
    expect(screen.getByText('Unknown status')).toHaveAttribute(
      'title',
      expect.stringContaining('99999'),
    )
    fireEvent.click(screen.getByRole('button', { name: 'Remove unknown status' }))
    expect(onChange).toHaveBeenCalledWith({ status_id_in: ['10003'] })

    fireEvent.click(screen.getByRole('checkbox', { name: 'In Review' }))
    expect(onChange).toHaveBeenCalledWith({ status_id_in: ['99999'] })
  })

  it('says so when a catalog cannot be read', async () => {
    stubCatalog('status_id_in', `${org}/jira/projects/200/statuses/list`)
    render(
      <PredicateEditor
        eventType="jira:issue:assigned"
        teamId="team-b"
        value={{}}
        onChange={vi.fn()}
      />,
    )

    expect(await screen.findByRole('alert')).toHaveTextContent(
      'Could not load the statuses this team can filter on.',
    )
  })

  it('leaves a Jira field that names no catalog to its own editor', async () => {
    stubCatalog('assignee_in')
    render(
      <PredicateEditor
        eventType="jira:issue:assigned"
        teamId="team-b"
        value={{}}
        onChange={vi.fn()}
      />,
    )

    // The identity editor, which has no identity to offer here.
    expect(await screen.findByText(/load your identity/)).toBeInTheDocument()
    expect(screen.queryByRole('group')).not.toBeInTheDocument()
  })
})
