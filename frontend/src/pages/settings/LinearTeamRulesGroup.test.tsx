// The team's Linear surface. Watching a Linear team also maps it — the
// workflow's state types say which states are pickup, in progress and done —
// so what is pinned here is that one click arrives at a mapped team when the
// types allow it, an unmapped one when they don't, and that a half mapping is
// called out rather than saved.
import { describe, it, expect, vi, beforeEach } from 'vitest'
import { render, screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { useState } from 'react'

import LinearTeamRulesGroup from './LinearTeamRulesGroup'
import {
  linearTeamIsArmed,
  linearTeamRulesValid,
  unmappedLinearTeam,
  type LinearTeamConfig,
} from './teamConfig'
import { jsonBody } from '../../test/apiResponse'

const CATALOG = [
  { id: 'team-eng', key: 'ENG', name: 'Engineering', private: false },
  { id: 'team-ops', key: 'OPS', name: 'Operations', private: true },
]

const WORKFLOW = [
  { id: 's-todo', name: 'Todo', type: 'unstarted', position: 1 },
  { id: 's-doing', name: 'In Progress', type: 'started', position: 2 },
  { id: 's-done', name: 'Done', type: 'completed', position: 3 },
]

// OPS's workflow has no started state, so its types cannot map it whole.
const NO_STARTED = [
  { id: 'o-todo', name: 'Todo', type: 'unstarted', position: 1 },
  { id: 'o-done', name: 'Done', type: 'completed', position: 2 },
]

const ORG = 'org-1'
const LINEAR = `/api/orgs/${ORG}/linear`
const statesPath = (team: string) => `${LINEAR}/teams/${team}/states/list`

function stubFetch() {
  const fetchMock = vi.fn(async (input: RequestInfo | URL) => {
    const url = String(input)
    if (url === `${LINEAR}/teams/list`) {
      return { ok: true, status: 200, ...jsonBody({ items: CATALOG, next_page_token: '' }) }
    }
    // Served out of board order, so a list that forgot to sort shows it.
    if (url === statesPath('team-eng')) {
      const items = [...WORKFLOW].reverse()
      return { ok: true, status: 200, ...jsonBody({ items, next_page_token: '' }) }
    }
    if (url === statesPath('team-ops')) {
      return { ok: true, status: 200, ...jsonBody({ items: NO_STARTED, next_page_token: '' }) }
    }
    throw new Error(`unexpected fetch: ${url || '(no url)'}`)
  })
  vi.stubGlobal('fetch', fetchMock)
  return fetchMock
}

function Harness({ seed = [] as LinearTeamConfig[], readOnly = false }) {
  const [value, setValue] = useState<LinearTeamConfig[]>(seed)
  return (
    <>
      <LinearTeamRulesGroup
        orgId={ORG}
        value={value}
        onChange={setValue}
        connected
        readOnly={readOnly}
        bare
      />
      <output data-testid="watched">{value.map((t) => t.key).join(',')}</output>
      <output data-testid="armed">
        {value
          .filter(linearTeamIsArmed)
          .map((t) => t.key)
          .join(',')}
      </output>
      <output data-testid="valid">{value.every(linearTeamRulesValid) ? 'yes' : 'no'}</output>
      <output data-testid="done-target">{value[0]?.done.canonical?.id ?? ''}</output>
    </>
  )
}

const rowFor = (name: string): HTMLElement => screen.getByText(name).closest('div')!.parentElement!

beforeEach(() => {
  stubFetch()
})

describe('LinearTeamRulesGroup · watching', () => {
  it('lists the catalog, marking a private team', async () => {
    render(<Harness />)
    expect(await screen.findByText('Engineering')).toBeInTheDocument()
    expect(within(rowFor('Operations')).getByText('private')).toBeInTheDocument()
  })

  it('maps a watched team from its state types', async () => {
    const user = userEvent.setup()
    render(<Harness />)
    await screen.findByText('Engineering')

    await user.click(within(rowFor('Engineering')).getByRole('button', { name: 'Watch' }))

    expect(screen.getByTestId('watched')).toHaveTextContent('ENG')
    await waitFor(() => expect(screen.getByTestId('armed')).toHaveTextContent('ENG'))
    expect(screen.getByTestId('done-target')).toHaveTextContent('s-done')
    expect(screen.getByText('Ready')).toBeInTheDocument()
  })

  it('leaves a team its types cannot map watched and unmapped', async () => {
    const user = userEvent.setup()
    const fetchMock = stubFetch()
    render(<Harness />)
    await screen.findByText('Operations')

    await user.click(within(rowFor('Operations')).getByRole('button', { name: 'Watch' }))

    await waitFor(() =>
      expect(fetchMock).toHaveBeenCalledWith(statesPath('team-ops'), expect.anything()),
    )
    expect(screen.getByTestId('watched')).toHaveTextContent('OPS')
    expect(screen.getByTestId('armed')).toHaveTextContent('')
    expect(screen.getByTestId('valid')).toHaveTextContent('yes')
    expect(screen.getByText('States not mapped')).toBeInTheDocument()
  })

  it('offers no type mapping on a team already mapped', async () => {
    // A mapping someone chose — here one the types would never produce — is
    // theirs; the editor does not offer to replace it with the pre-fill.
    const handMapped: LinearTeamConfig = {
      ...unmappedLinearTeam('team-eng', 'ENG', 'Engineering'),
      pickup: { members: [{ id: 's-doing', name: 'In Progress', type: 'started' }] },
      in_progress: {
        members: [{ id: 's-todo', name: 'Todo', type: 'unstarted' }],
        canonical: { id: 's-todo', name: 'Todo', type: 'unstarted' },
      },
      done: {
        members: [{ id: 's-done', name: 'Done', type: 'completed' }],
        canonical: { id: 's-done', name: 'Done', type: 'completed' },
      },
    }
    const user = userEvent.setup()
    render(<Harness seed={[handMapped]} />)
    await user.click(screen.getByRole('button', { name: 'Expand ENG' }))
    await screen.findByText('3 states available')
    expect(screen.queryByRole('button', { name: 'Map from state types' })).toBeNull()
  })

  it('stops watching from the board', async () => {
    const user = userEvent.setup()
    render(<Harness seed={[unmappedLinearTeam('team-eng', 'ENG', 'Engineering')]} />)
    await user.click(screen.getByRole('button', { name: 'Stop watching ENG' }))
    expect(screen.getByTestId('watched')).toHaveTextContent('')
  })

  it('keeps a watched team the catalog no longer offers, so it can be dropped', async () => {
    render(<Harness seed={[unmappedLinearTeam('team-gone', 'GONE', 'Gone Team')]} />)
    await screen.findByText('Engineering')
    expect(screen.getByText('not visible to this credential')).toBeInTheDocument()
  })
})

describe('LinearTeamRulesGroup · mapping', () => {
  it('offers the type mapping on an unmapped team', async () => {
    const user = userEvent.setup()
    render(<Harness seed={[unmappedLinearTeam('team-eng', 'ENG', 'Engineering')]} />)

    await user.click(screen.getByRole('button', { name: 'Expand ENG' }))
    await user.click(await screen.findByRole('button', { name: 'Map from state types' }))

    expect(screen.getByTestId('armed')).toHaveTextContent('ENG')
  })

  it('calls out half a mapping', async () => {
    const user = userEvent.setup()
    render(<Harness seed={[unmappedLinearTeam('team-eng', 'ENG', 'Engineering')]} />)
    await user.click(screen.getByRole('button', { name: 'Expand ENG' }))
    await screen.findByText('3 states available')

    const pickup = screen
      .getByText(/Poll for unassigned issues/)
      .closest('div.space-y-2') as HTMLElement
    await user.click(within(pickup).getByRole('button', { name: 'Todo' }))

    expect(screen.getByTestId('valid')).toHaveTextContent('no')
    expect(screen.getByText('Partly mapped')).toBeInTheDocument()
    expect(screen.getByText(/or clear all three to keep ENG watched/)).toBeInTheDocument()
  })
})

describe('LinearTeamRulesGroup · read-only', () => {
  const mapped: LinearTeamConfig = {
    ...unmappedLinearTeam('team-eng', 'ENG', 'Engineering'),
    pickup: { members: [{ id: 's-todo', name: 'Todo', type: 'unstarted' }] },
    in_progress: {
      members: [{ id: 's-doing', name: 'In Progress', type: 'started' }],
      canonical: { id: 's-doing', name: 'In Progress', type: 'started' },
    },
    done: {
      members: [{ id: 's-done', name: 'Done', type: 'completed' }],
      canonical: { id: 's-done', name: 'Done', type: 'completed' },
    },
  }

  it('shows the mapping with no verbs and no catalog', async () => {
    const user = userEvent.setup()
    const fetchMock = stubFetch()
    render(<Harness seed={[mapped]} readOnly />)

    await user.click(screen.getByRole('button', { name: 'Expand ENG' }))
    expect(await screen.findByText('3 states available')).toBeInTheDocument()
    expect(screen.getByText('In Progress ★')).toBeInTheDocument()
    expect(screen.getByText('Done ★')).toBeInTheDocument()

    expect(screen.queryByRole('button', { name: 'Stop watching ENG' })).toBeNull()
    expect(screen.queryByRole('button', { name: 'Watch' })).toBeNull()
    expect(screen.queryByRole('searchbox', { name: 'Search Linear teams' })).toBeNull()
    expect(fetchMock).not.toHaveBeenCalledWith(`${LINEAR}/teams/list`, expect.anything())
  })
})
