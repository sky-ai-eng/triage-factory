import { describe, it, expect, vi, beforeEach } from 'vitest'
import { render, screen, waitFor } from '@testing-library/react'
import NotifyRulesPanel from './NotifyRulesPanel'
import { jsonBody } from '../test/apiResponse'

// The editor is a stand-in that shows which rule it was opened on, and
// whether the panel judged it writable.
vi.mock('./NotifyRuleEditor', () => ({
  default: (props: { open: boolean; rule: { id: string } | null; writable: boolean }) =>
    props.open ? (
      <p>
        editor {props.rule?.id ?? 'new'} {props.writable ? 'writable' : 'read-only'}
      </p>
    ) : null,
}))
vi.mock('../hooks/useDeploymentConfig', () => ({
  useMe: () => ({ me: { id: 'me' }, loading: false, error: null }),
}))
vi.mock('../hooks/useTeams', () => ({
  useTeams: () => ({ teams: [{ id: 'team-a', name: 'Infra', slug: 'infra', role: 'member' }] }),
}))
const toastMock = vi.hoisted(() => ({ error: vi.fn() }))
vi.mock('./Toast/toastStore', () => ({ toast: toastMock }))

const notifyRule = (id: string, audience: 'creator' | 'team', creator: string) => ({
  id,
  kind: 'notify',
  event_type: 'github:pr:ci_check_failed',
  name: `Rule ${id}`,
  audience,
  creator_user_id: creator,
  enabled: true,
  source: 'user',
  team_id: 'team-a',
  scope_predicate_json: null,
})

function stubRules() {
  const items = [
    notifyRule('mine', 'creator', 'me'),
    notifyRule('theirs', 'creator', 'someone-else'),
    notifyRule('team', 'team', 'an-admin'),
  ]
  vi.stubGlobal(
    'fetch',
    vi.fn().mockResolvedValue({
      ok: true,
      status: 200,
      ...jsonBody({ items, next_page_token: '', total_count: items.length }),
    }),
  )
}

describe('NotifyRulesPanel', () => {
  beforeEach(() => {
    toastMock.error.mockReset()
  })

  it("lists the viewer's own rules and the team's, not another member's personal rule", async () => {
    stubRules()
    render(<NotifyRulesPanel open onClose={() => {}} />)

    await waitFor(() => expect(screen.getByText('Rule mine')).toBeTruthy())
    expect(screen.getByText('Rule team')).toBeTruthy()
    expect(screen.queryByText('Rule theirs')).toBeNull()
    expect(screen.getByText('Yours')).toBeTruthy()
    expect(screen.getByText('Team')).toBeTruthy()
  })

  it('disables the switch on a team rule for a member who is not its team admin', async () => {
    stubRules()
    render(<NotifyRulesPanel open onClose={() => {}} />)

    await waitFor(() => expect(screen.getByText('Rule team')).toBeTruthy())
    expect(screen.getByLabelText('Enable Rule team').hasAttribute('disabled')).toBe(true)
    expect(screen.getByLabelText('Enable Rule mine').hasAttribute('disabled')).toBe(false)
  })

  it('opens the linked rule, read-only when the viewer cannot change it', async () => {
    stubRules()
    render(<NotifyRulesPanel open onClose={() => {}} openRuleId="team" />)

    await waitFor(() => expect(screen.getByText('editor team read-only')).toBeTruthy())
    expect(toastMock.error).not.toHaveBeenCalled()
  })

  it('says so when the linked rule is not in the list', async () => {
    stubRules()
    render(<NotifyRulesPanel open onClose={() => {}} openRuleId="gone" />)

    await waitFor(() => expect(toastMock.error).toHaveBeenCalledTimes(1))
    expect(screen.queryByText(/^editor/)).toBeNull()
  })
})
