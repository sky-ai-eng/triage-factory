import { describe, it, expect, vi, beforeEach } from 'vitest'
import { useState } from 'react'
import { render, screen, fireEvent, waitFor } from '@testing-library/react'
import PromptPicker from './PromptPicker'
import { listBody } from '../test/apiResponse'

// A delegate's blueprint belongs to the team the task lands on, so the picker
// that chooses the blueprint offers only the teams that can take the task and
// lists the chosen team's blueprints. The viewer here is on three teams; the
// task can land on two of them.

vi.mock('../hooks/useTeams', () => ({
  useTeams: () => ({
    teams: [
      { id: 't-alpha', name: 'Alpha', slug: 'alpha' },
      { id: 't-beta', name: 'Beta', slug: 'beta' },
      { id: 't-gamma', name: 'Gamma', slug: 'gamma' },
    ],
    lastActingTeamId: '',
    multi: true,
    loaded: true,
  }),
}))

const fetchMock = vi.fn()

const byTeam: Record<string, { id: string; name: string }[]> = {
  't-alpha': [{ id: 'b-alpha', name: 'Alpha review' }],
  't-beta': [{ id: 'b-beta', name: 'Beta fix' }],
}

beforeEach(() => {
  fetchMock.mockReset()
  vi.stubGlobal('fetch', fetchMock)
  // The server narrows the list to the team the read names.
  fetchMock.mockImplementation(async (path: string, init?: RequestInit) => {
    const body = JSON.parse(String(init?.body ?? '{}')) as { team_id?: string }
    if (path === '/api/blueprints/list') {
      const rows = (byTeam[body.team_id ?? ''] ?? []).map((b) => ({
        ...b,
        source: 'user',
        usage_count: 0,
        created_at: '',
        updated_at: '',
      }))
      return { ok: true, status: 200, ...listBody(rows) }
    }
    return { ok: true, status: 200, ...listBody([]) }
  })
})

function blueprintReads(): (string | undefined)[] {
  return fetchMock.mock.calls
    .filter((c) => c[0] === '/api/blueprints/list')
    .map((c) => (JSON.parse(String((c[1] as RequestInit).body)) as { team_id?: string }).team_id)
}

function Harness({ options }: { options: string[] }) {
  const [team, setTeam] = useState(options[0])
  return (
    <PromptPicker
      open
      source="blueprints"
      onSelect={() => {}}
      onClose={() => {}}
      teamValue={team}
      onTeamChange={setTeam}
      teamOptions={options}
    />
  )
}

describe('PromptPicker team options', () => {
  it('offers only the given teams, and lists the chosen team’s blueprints', async () => {
    render(<Harness options={['t-alpha', 't-beta']} />)

    // The list is read for the team the picker opened on, and shows its rows.
    expect(await screen.findByRole('option', { name: /Alpha review/ })).toBeInTheDocument()
    expect(screen.queryByRole('option', { name: /Beta fix/ })).not.toBeInTheDocument()
    expect(blueprintReads()).toEqual(['t-alpha'])

    // The team selector offers the two options and not the viewer's third team.
    fireEvent.click(screen.getByRole('button', { name: /Alpha/ }))
    expect(screen.getByRole('button', { name: 'Beta' })).toBeInTheDocument()
    expect(screen.queryByRole('button', { name: 'Gamma' })).not.toBeInTheDocument()

    // Choosing the other team reads that team's blueprints and drops the first's.
    fireEvent.click(screen.getByRole('button', { name: 'Beta' }))
    expect(await screen.findByRole('option', { name: /Beta fix/ })).toBeInTheDocument()
    await waitFor(() =>
      expect(screen.queryByRole('option', { name: /Alpha review/ })).not.toBeInTheDocument(),
    )
    expect(blueprintReads()).toEqual(['t-alpha', 't-beta'])
  })

  it('shows no team selector for one option, and still lists that team’s blueprints', async () => {
    render(<Harness options={['t-beta']} />)

    expect(await screen.findByRole('option', { name: /Beta fix/ })).toBeInTheDocument()
    expect(screen.queryByText('Team')).not.toBeInTheDocument()
    expect(blueprintReads()).toEqual(['t-beta'])
  })
})
