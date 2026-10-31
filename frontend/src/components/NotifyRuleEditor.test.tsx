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
// One snapshot, as the real hook's store hands out: a fresh array per render
// would re-run the editor's default-team effect on every render.
const teamsSnapshot = vi.hoisted(() => ({
  teams: [
    { id: 'team-a', name: 'Infra', slug: 'infra', role: 'admin' },
    { id: 'team-b', name: 'Web', slug: 'web', role: 'member' },
  ],
  lastActingTeamId: 'team-a',
  loaded: true,
}))
vi.mock('../hooks/useTeams', () => ({
  useTeams: () => teamsSnapshot,
  pickerDefault: () => 'team-a',
  noteWrittenTeam: () => {},
}))
vi.mock('../hooks/useEventSources', () => ({
  useEventSources: () => ({ canProduce: () => true }),
}))
vi.mock('../hooks/useDeploymentConfig', () => ({
  useMe: () => ({ me: { id: 'me' }, loading: false, error: null }),
}))

const props = { open: true, writable: true, onClose: () => {}, onSaved: () => {} }

describe('NotifyRuleEditor', () => {
  beforeEach(() => {
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
