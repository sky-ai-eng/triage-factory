import { describe, it, expect, vi, beforeEach } from 'vitest'
import { render, screen, fireEvent, waitFor } from '@testing-library/react'
import NotifyRuleEditor from './NotifyRuleEditor'
import { jsonBody } from '../test/apiResponse'

// The picker is a stand-in select over the two teams the viewer is on.
vi.mock('./TeamPicker', () => ({
  default: (props: { value: string; onChange: (id: string) => void }) => (
    <select aria-label="Team" value={props.value} onChange={(e) => props.onChange(e.target.value)}>
      <option value="team-a">Infra</option>
      <option value="team-b">Web</option>
    </select>
  ),
}))
// One snapshot, as the real store hands out: a fresh array per render would
// re-run the editor's default-team effect on every render.
const teamsStore = vi.hoisted(() => ({
  teams: [
    { id: 'team-a', name: 'Infra', slug: 'infra', role: 'admin' },
    { id: 'team-b', name: 'Web', slug: 'web', role: 'member' },
  ],
  lastActingTeamId: 'team-a',
  loaded: true,
}))
vi.mock('../hooks/useTeams', () => ({
  useTeams: () => teamsStore,
  pickerDefault: (_teams: unknown, last: string) => last,
  noteWrittenTeam: () => {},
}))
vi.mock('../hooks/useEventSources', () => ({
  useEventSources: () => ({ canProduce: () => true }),
}))

const eventTypes = [
  {
    id: 'github:pr:ci_check_failed',
    source: 'github',
    category: 'pr',
    label: 'CI Check Failed',
    description: '',
    supports_watch: true,
  },
]

describe('NotifyRuleEditor', () => {
  beforeEach(() => {
    vi.stubGlobal(
      'fetch',
      vi.fn((url: string) =>
        Promise.resolve({
          ok: true,
          status: url === '/api/event-handlers/notify' ? 201 : 200,
          ...jsonBody(url === '/api/event-types' ? eventTypes : {}),
        }),
      ),
    )
  })

  it('creates a rule on a team the viewer is a member of, naming no audience', async () => {
    const onSaved = vi.fn()
    render(<NotifyRuleEditor open rule={null} onClose={() => {}} onSaved={onSaved} />)

    fireEvent.change(screen.getByLabelText('Team'), { target: { value: 'team-b' } })
    await screen.findByRole('option', { name: /CI Check Failed/ })
    fireEvent.change(screen.getByLabelText('Event type'), {
      target: { value: 'github:pr:ci_check_failed' },
    })
    fireEvent.change(screen.getByLabelText('Name'), { target: { value: 'CI failed' } })
    fireEvent.click(screen.getByRole('button', { name: 'Create' }))

    await waitFor(() => expect(onSaved).toHaveBeenCalled())
    const create = vi
      .mocked(fetch)
      .mock.calls.find(([url]) => url === '/api/event-handlers/notify') as [string, RequestInit]
    expect(JSON.parse(String(create[1].body))).toEqual({
      event_type: 'github:pr:ci_check_failed',
      name: 'CI failed',
      team_id: 'team-b',
    })
  })
})
