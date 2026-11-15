import { describe, it, expect, vi } from 'vitest'
import { render, screen, fireEvent, waitFor, within } from '@testing-library/react'
import TaskRuleEditor from './TaskRuleEditor'
import { jsonBody } from '../test/apiResponse'
import type { EventType } from '../types'

// What's under test is the event-type picker and the watch flag; the
// predicate editor reads its own schema and is a stand-in here.
vi.mock('./PredicateEditor', () => ({ default: () => null }))
vi.mock('../hooks/useEventSources', () => ({
  useEventSources: () => ({ stateOf: () => 'available', canProduce: () => true }),
}))
vi.mock('../hooks/useTeams', () => ({
  useTeams: () => ({ teams: [{ id: 'team-a' }], lastActingTeamId: '', loaded: true }),
  pickerDefault: () => '',
  noteWrittenTeam: () => {},
}))

const catalog: EventType[] = [
  {
    id: 'github:pr:opened',
    source: 'github',
    category: 'pr',
    label: 'PR Opened',
    description: '',
    supports_watch: true,
  },
  {
    id: 'github:vulnerability:alert_opened',
    source: 'github',
    category: 'vulnerability',
    label: 'Vulnerability Alert Opened',
    description: '',
    supports_watch: false,
  },
  {
    id: 'jira:issue:assigned',
    source: 'jira',
    category: 'issue',
    label: 'Assigned',
    description: '',
    supports_watch: true,
  },
]

function stubAPI() {
  const fetchMock = vi.fn((url: string, _init?: RequestInit) => {
    if (url === '/api/event-types') {
      return Promise.resolve({ ok: true, status: 200, ...jsonBody(catalog) })
    }
    return Promise.resolve({ ok: true, status: 201, ...jsonBody({ id: 'rule-1' }) })
  })
  vi.stubGlobal('fetch', fetchMock)
  return fetchMock
}

describe('TaskRuleEditor on a vulnerability alert', () => {
  it('offers the event under GitHub, hides the watch toggle, and leaves the flag out of the create', async () => {
    const fetchMock = stubAPI()
    const onSaved = vi.fn()
    render(<TaskRuleEditor open rule={null} onClose={() => {}} onSaved={onSaved} />)

    const github = await screen.findByRole('group', { name: 'GitHub' })
    expect(
      within(github).getByRole('option', {
        name: 'Vulnerability Alert Opened (github:vulnerability:alert_opened)',
      }),
    ).toBeInTheDocument()
    expect(screen.getByRole('group', { name: 'Jira' })).toBeInTheDocument()

    // The toggle is there for an owner-ladder event, and gone for this one.
    fireEvent.change(screen.getByRole('combobox'), { target: { value: 'github:pr:opened' } })
    expect(screen.getByText('Apply to entities outside this team')).toBeInTheDocument()
    fireEvent.change(screen.getByRole('combobox'), {
      target: { value: 'github:vulnerability:alert_opened' },
    })
    expect(screen.queryByText('Apply to entities outside this team')).not.toBeInTheDocument()

    fireEvent.change(screen.getByPlaceholderText('e.g. CI failures on my PRs'), {
      target: { value: 'High severity alerts' },
    })
    fireEvent.click(screen.getByRole('button', { name: 'Create' }))

    await waitFor(() => expect(onSaved).toHaveBeenCalled())
    const create = fetchMock.mock.calls.find(([url]) => url === '/api/event-handlers/rules')
    expect(create).toBeDefined()
    const body = JSON.parse(create![1]!.body as string)
    expect(body.event_type).toBe('github:vulnerability:alert_opened')
    expect(body).not.toHaveProperty('applies_to_unowned')
  })
})
