import { describe, it, expect, vi, beforeEach } from 'vitest'
import { render, screen, fireEvent } from '@testing-library/react'
import NotifyRuleEditor from './NotifyRuleEditor'
import type { NotifyHandler } from '../types'
import { jsonBody } from '../test/apiResponse'

// The picker is a stand-in select over the two teams the viewer is on: an
// admin of Infra, a member of Web.
vi.mock('./TeamPicker', () => ({
  default: (props: { value: string; onChange: (id: string) => void }) => (
    <select aria-label="Team" value={props.value} onChange={(e) => props.onChange(e.target.value)}>
      <option value="team-a">Infra</option>
      <option value="team-b">Web</option>
    </select>
  ),
}))
// The store's current snapshot, replaced whole as the real store replaces it:
// a fresh array per render would re-run the editor's default-team effect on
// every render. The picker default is the last team written to.
const teamsStore = vi.hoisted(() => {
  const initial = {
    teams: [
      { id: 'team-a', name: 'Infra', slug: 'infra', role: 'admin' },
      { id: 'team-b', name: 'Web', slug: 'web', role: 'member' },
    ],
    lastActingTeamId: 'team-a',
    loaded: true,
  }
  return { initial, current: initial }
})
vi.mock('../hooks/useTeams', () => ({
  useTeams: () => teamsStore.current,
  pickerDefault: (_teams: unknown, last: string) => last,
  noteWrittenTeam: () => {},
}))
vi.mock('../hooks/useEventSources', () => ({
  useEventSources: () => ({ canProduce: () => true }),
}))
const meStore = vi.hoisted(() => ({ current: { id: 'me' } as { id: string } | null }))
vi.mock('../hooks/useDeploymentConfig', () => ({
  useMe: () => ({ me: meStore.current, loading: meStore.current === null, error: null }),
}))

const props = { open: true, writable: true, onClose: () => {}, onSaved: () => {} }

describe('NotifyRuleEditor', () => {
  beforeEach(() => {
    teamsStore.current = teamsStore.initial
    meStore.current = { id: 'me' }
    vi.stubGlobal('fetch', vi.fn().mockResolvedValue({ ok: true, status: 200, ...jsonBody([]) }))
  })

  it("takes a team audience back when the picked team is one the viewer doesn't administer", () => {
    render(<NotifyRuleEditor {...props} rule={null} />)
    const everyone = screen.getByLabelText('Everyone on the team') as HTMLInputElement
    fireEvent.click(everyone)
    expect(everyone.checked).toBe(true)

    fireEvent.change(screen.getByLabelText('Team'), { target: { value: 'team-b' } })
    expect(everyone.checked).toBe(false)
    expect(everyone.disabled).toBe(true)
    expect((screen.getByLabelText('Just me') as HTMLInputElement).checked).toBe(true)
  })

  it('drops a team audience when a teams refresh moves the default to a non-admin team', () => {
    const { rerender } = render(<NotifyRuleEditor {...props} rule={null} />)
    const everyone = screen.getByLabelText('Everyone on the team') as HTMLInputElement
    fireEvent.click(everyone)
    expect(everyone.checked).toBe(true)

    teamsStore.current = { ...teamsStore.initial, lastActingTeamId: 'team-b' }
    rerender(<NotifyRuleEditor {...props} rule={null} />)
    expect(everyone.checked).toBe(false)
    expect((screen.getByLabelText('Just me') as HTMLInputElement).checked).toBe(true)
  })

  const personalRule = (creator: string): NotifyHandler =>
    ({
      id: 'r1',
      kind: 'notify',
      event_type: 'github:pr:ci_check_failed',
      name: 'A rule',
      audience: 'creator',
      creator_user_id: creator,
      enabled: true,
      source: 'user',
      team_id: 'team-a',
      scope_predicate_json: null,
    }) as NotifyHandler

  it('disables every form control for a reader, and leaves Close usable', async () => {
    // A schema with one field of each widget kind, so the predicate editor's
    // pills, select and text input are under test alongside the editor's own.
    const schema = {
      fields: [
        { name: 'is_draft', type: 'bool' },
        { name: 'conclusion', type: 'string', enum_values: ['failure', 'timed_out'] },
        { name: 'check_name', type: 'string' },
      ],
    }
    vi.stubGlobal(
      'fetch',
      vi.fn((url: string) =>
        Promise.resolve({
          ok: true,
          status: 200,
          ...jsonBody(url.startsWith('/api/event-schemas/') ? schema : []),
        }),
      ),
    )
    render(<NotifyRuleEditor {...props} writable={false} rule={personalRule('bob')} />)
    await screen.findByRole('button', { name: 'Yes' })

    const controls = [
      ...screen.getAllByRole('textbox'),
      ...screen.getAllByRole('combobox'),
      ...screen.getAllByRole('radio'),
      ...['Any', 'Yes', 'No'].map((name) => screen.getByRole('button', { name })),
    ]
    // The rule's name and check_name; the event type and conclusion; the two
    // audiences; the bool's three pills.
    expect(controls).toHaveLength(9)
    for (const control of controls) expect(control).toBeDisabled()
    expect(screen.getByRole('button', { name: 'Close' })).toBeEnabled()
    expect(screen.queryByRole('button', { name: 'Delete' })).toBeNull()
  })

  it('reads "Just me" on a personal rule while the viewer is still loading', () => {
    meStore.current = null
    render(<NotifyRuleEditor {...props} rule={personalRule('me')} />)
    expect(screen.getByLabelText('Just me')).toBeTruthy()
  })

  it("names the creator rather than the viewer on someone else's personal rule", () => {
    const theirs: NotifyHandler = {
      id: 'r1',
      kind: 'notify',
      event_type: 'github:pr:ci_check_failed',
      name: "Bob's rule",
      audience: 'creator',
      creator_user_id: 'bob',
      enabled: true,
      source: 'user',
      team_id: 'team-a',
      scope_predicate_json: null,
    } as NotifyHandler
    render(<NotifyRuleEditor {...props} rule={theirs} />)
    expect(screen.getByLabelText('Only the person who made it')).toBeTruthy()
    expect(screen.queryByLabelText('Just me')).toBeNull()
  })
})
