import { describe, it, expect, vi } from 'vitest'
import { cleanup, render, screen, fireEvent, within } from '@testing-library/react'
import PredicateEditor from './PredicateEditor'
import { jsonBody, listBody } from '../test/apiResponse'
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
// Linear. OLD has not been resolved against Jira, so it has no Jira id.
const settings = {
  jira_projects: [
    { id: 'jp-sky', jira_id: '100', key: 'SKY', name: 'Sky' },
    { id: 'jp-ops', jira_id: '200', key: 'OPS', name: 'Operations' },
    { id: 'jp-unresolved', key: 'OLD', name: '' },
  ].map((p) => ({ ...p, pickup: rule, in_progress: rule, in_review: rule, done: rule })),
  linear_teams: [{ id: 'lt-eng', key: 'ENG', name: 'Engineering' }],
}

const labelsPath = `${org}/linear/teams/lt-eng/labels/list`

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
    { id: 'st-todo', name: 'Todo', type: 'unstarted', position: 1 },
    { id: 'st-review', name: 'In Review', type: 'started', position: 2 },
  ],
  // The labels list answers the narrowing its body names, as the server does;
  // a read that names none has no answer here.
  [`${labelsPath}#is_group=false`]: [
    { id: 'lb-bug', name: 'Bug', group: null, is_group: false },
    { id: 'lb-hard', name: 'Hard', group: { id: 'lb-diff', name: 'Difficulty' }, is_group: false },
  ],
  [`${labelsPath}#is_group=true`]: [
    { id: 'lb-diff', name: 'Difficulty', group: null, is_group: true },
  ],
}

// renderField mounts the editor on one string_list field, with every catalog
// list answering its rows (or its rows in opts.lists) except failing, which
// answers 500.
function renderField(
  field: string,
  eventType: string,
  opts: {
    teamId?: string
    value?: Record<string, string[]>
    failing?: string
    lists?: Record<string, unknown[]>
  } = {},
) {
  const answers = { ...lists, ...opts.lists }
  const fetchMock = vi.fn((url: string, init?: { body?: string }) => {
    const key =
      url === labelsPath ? `${url}#is_group=${JSON.parse(init?.body ?? '{}').is_group}` : url
    const body = url.startsWith('/api/event-schemas/')
      ? jsonBody({ fields: [{ name: field, type: 'string_list' }] })
      : url.startsWith('/api/teams/')
        ? jsonBody(settings)
        : key in answers && url !== opts.failing
          ? listBody(answers[key])
          : null
    if (body) return Promise.resolve({ ok: true, status: 200, ...body })
    return Promise.resolve({ ok: false, status: 500, ...jsonBody({ errors: [] }) })
  })
  vi.stubGlobal('fetch', fetchMock)
  const onChange = vi.fn()
  render(
    <PredicateEditor
      eventType={eventType}
      teamId={opts.teamId}
      value={opts.value ?? {}}
      onChange={onChange}
    />,
  )
  return { fetchMock, onChange }
}

const offered = () =>
  screen.getAllByRole('checkbox').map((box) => box.closest('label')?.textContent)

describe('PredicateEditor catalog fields', () => {
  it.each([
    ['status_id_in', 'jira:issue:assigned', ['To Do', 'In Review', 'To Do', 'Triage'], '20000'],
    ['priority_id_in', 'jira:issue:available', ['Highest', 'High'], '2'],
    ['issue_type_id_in', 'jira:issue:available', ['Bug', 'Incident'], '10100'],
    ['status_id_in', 'linear:issue:assigned', ['Todo', 'In Review'], 'st-review'],
    ['label_id_in', 'linear:issue:label_added', ['Bug', 'Difficulty / Hard'], 'lb-hard'],
    ['label_group_id_in', 'linear:issue:label_added', ['Difficulty'], 'lb-diff'],
  ])(
    '%s on %s offers its catalog and writes the last option by id',
    async (field, eventType, names, id) => {
      const { onChange } = renderField(field, eventType)

      fireEvent.click(await screen.findByRole('checkbox', { name: names[names.length - 1] }))
      expect(onChange).toHaveBeenCalledWith({ [field]: [id] })
      expect(offered()).toEqual(names)
    },
  )

  it.each([
    ['label_id_in', false],
    ['label_group_id_in', true],
  ])('%s asks the server for is_group %s', async (field, isGroup) => {
    const { fetchMock } = renderField(field, 'linear:issue:label_added')

    await screen.findAllByRole('checkbox')
    expect(fetchMock).toHaveBeenCalledWith(
      labelsPath,
      expect.objectContaining({ body: JSON.stringify({ is_group: isGroup, page_size: 200 }) }),
    )
  })

  it('offers the server’s answer as served, re-filtering and re-sorting nothing', async () => {
    // A group among the groups' answer that says it is none, and states out
    // of position order: the server decides both, so the field shows them.
    renderField('label_group_id_in', 'linear:issue:label_added', {
      lists: {
        [`${labelsPath}#is_group=true`]: [
          { id: 'lb-diff', name: 'Difficulty', group: null, is_group: true },
          { id: 'lb-size', name: 'Size', group: null, is_group: false },
        ],
      },
    })
    await screen.findByRole('checkbox', { name: 'Size' })
    expect(offered()).toEqual(['Difficulty', 'Size'])
    cleanup()

    renderField('status_id_in', 'linear:issue:assigned', {
      lists: {
        [`${org}/linear/teams/lt-eng/states/list`]: [
          { id: 'st-review', name: 'In Review', type: 'started', position: 2 },
          { id: 'st-todo', name: 'Todo', type: 'unstarted', position: 1 },
        ],
      },
    })
    await screen.findByRole('checkbox', { name: 'Todo' })
    expect(offered()).toEqual(['In Review', 'Todo'])
  })

  it('groups statuses by tracked project and treats a shared one as one id', async () => {
    const { fetchMock, onChange } = renderField('new_status_id_in', 'jira:issue:status_changed', {
      teamId: 'team-b',
      value: { new_status_id_in: ['10000'] },
    })

    for (const project of ['SKY · Sky', 'OPS · Operations']) {
      const group = await screen.findByRole('group', { name: project })
      expect(within(group).getByRole('checkbox', { name: 'To Do' })).toBeChecked()
    }
    fireEvent.click(screen.getAllByRole('checkbox', { name: 'To Do' })[1])
    expect(onChange).toHaveBeenCalledWith({})
    expect(fetchMock).toHaveBeenCalledWith('/api/teams/team-b/settings', expect.anything())
  })

  it('keeps a stored id nothing offers any more, removable', async () => {
    const { onChange } = renderField('status_id_in', 'jira:issue:assigned', {
      value: { status_id_in: ['10003', '99999'] },
    })

    const chip = await screen.findByText('Unknown status')
    expect(chip).toHaveAttribute('title', expect.stringContaining('99999'))
    fireEvent.click(screen.getByRole('checkbox', { name: 'In Review' }))
    expect(onChange).toHaveBeenCalledWith({ status_id_in: ['99999'] })
    fireEvent.click(screen.getByRole('button', { name: 'Remove unknown status' }))
    expect(onChange).toHaveBeenCalledWith({ status_id_in: ['10003'] })
  })

  it('says so when a catalog cannot be read', async () => {
    renderField('status_id_in', 'jira:issue:assigned', {
      failing: `${org}/jira/projects/200/statuses/list`,
    })

    expect(await screen.findByRole('alert')).toHaveTextContent(
      'Could not load the statuses this team can filter on.',
    )
  })

  it('leaves a Jira field that names no catalog to its own editor', async () => {
    renderField('assignee_in', 'jira:issue:assigned')

    // The identity editor, which has no identity to offer here.
    expect(await screen.findByText(/load your identity/)).toBeInTheDocument()
    expect(screen.queryByRole('group')).not.toBeInTheDocument()
  })
})
