import { describe, it, expect, vi, beforeEach } from 'vitest'
import { render, screen, waitFor } from '@testing-library/react'
import NotifyRulesPanel from './NotifyRulesPanel'
import { jsonBody, listBody } from '../test/apiResponse'
import { reportFailedThroughError } from '../test/toastDouble'

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
const toastMock = vi.hoisted(() => ({ error: vi.fn(), failed: vi.fn() }))
vi.mock('./Toast/toastStore', () => ({ toast: toastMock }))
reportFailedThroughError(toastMock)

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

type Body = Record<string, unknown>

// stubRules answers the two list reads by their audience, and a single read
// by id. The personal read's answer includes a rule another member authored:
// the server decides what each section holds, and the panel renders what it
// is given, so it shows up here even though it could not pass a client-side
// creator check.
function stubRules() {
  const rules = {
    mine: notifyRule('mine', 'creator', 'me'),
    alsoServed: notifyRule('also-served', 'creator', 'someone-else'),
    team: notifyRule('team', 'team', 'an-admin'),
  }
  const bodies: Body[] = []
  const fetchMock = vi.fn().mockImplementation((url: string, init?: RequestInit) => {
    const u = String(url)
    if (u.endsWith('/list')) {
      const body = JSON.parse(String(init?.body ?? '{}')) as Body
      bodies.push(body)
      const items = body.audience === 'team' ? [rules.team] : [rules.mine, rules.alsoServed]
      return Promise.resolve({ ok: true, status: 200, ...listBody(items) })
    }
    const id = decodeURIComponent(u.split('/').pop() ?? '')
    const rule = Object.values(rules).find((r) => r.id === id)
    if (!rule) {
      return Promise.resolve({
        ok: false,
        status: 404,
        ...jsonBody({ errors: [{ reason: 'NOT_FOUND', message: 'event handler not found' }] }),
      })
    }
    return Promise.resolve({ ok: true, status: 200, ...jsonBody(rule) })
  })
  vi.stubGlobal('fetch', fetchMock)
  return { bodies, fetchMock }
}

describe('NotifyRulesPanel', () => {
  beforeEach(() => {
    toastMock.error.mockReset()
  })

  it("reads the viewer's own rules and the team's as two narrowed lists", async () => {
    const { bodies } = stubRules()
    render(<NotifyRulesPanel open onClose={() => {}} />)

    await waitFor(() => expect(screen.getByText('Rule mine')).toBeTruthy())
    expect(screen.getByText('Rule team')).toBeTruthy()
    expect(screen.getByText('Rule also-served')).toBeTruthy()
    expect(screen.getByText('Yours')).toBeTruthy()
    expect(screen.getByText('Team')).toBeTruthy()

    // Each section's narrowing is in its body: the panel walks no pages and
    // filters nothing it holds.
    expect(bodies).toEqual(
      expect.arrayContaining([
        { kind: 'notify', audience: 'creator', creator_user_id: 'me', page_size: 50 },
        { kind: 'notify', audience: 'team', page_size: 50 },
      ]),
    )
    expect(bodies.every((b) => b.page_token === undefined)).toBe(true)
  })

  it('disables the switch on a team rule for a member who is not its team admin', async () => {
    stubRules()
    render(<NotifyRulesPanel open onClose={() => {}} />)

    await waitFor(() => expect(screen.getByText('Rule team')).toBeTruthy())
    expect(screen.getByLabelText('Enable Rule team').hasAttribute('disabled')).toBe(true)
    expect(screen.getByLabelText('Enable Rule mine').hasAttribute('disabled')).toBe(false)
  })

  it('opens the linked rule by its own read, read-only when the viewer cannot change it', async () => {
    const { fetchMock } = stubRules()
    render(<NotifyRulesPanel open onClose={() => {}} openRuleId="team" />)

    await waitFor(() => expect(screen.getByText('editor team read-only')).toBeTruthy())
    expect(fetchMock).toHaveBeenCalledWith('/api/event-handlers/team', expect.anything())
    expect(toastMock.error).not.toHaveBeenCalled()
  })

  it('says so when the linked rule is gone', async () => {
    stubRules()
    render(<NotifyRulesPanel open onClose={() => {}} openRuleId="gone" />)

    await waitFor(() => expect(toastMock.error).toHaveBeenCalledTimes(1))
    expect(screen.queryByText(/^editor/)).toBeNull()
  })
})
