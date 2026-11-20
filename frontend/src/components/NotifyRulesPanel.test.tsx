import { describe, it, expect, vi, beforeEach } from 'vitest'
import { render, screen, waitFor, fireEvent } from '@testing-library/react'
import NotifyRulesPanel from './NotifyRulesPanel'
import { jsonBody, listBody } from '../test/apiResponse'
import { reportFailedThroughError } from '../test/toastDouble'

// The editor is a stand-in that shows which rule it was opened on.
vi.mock('./NotifyRuleEditor', () => ({
  default: (props: { open: boolean; rule: { id: string } | null }) =>
    props.open ? <p>editor {props.rule?.id ?? 'new'}</p> : null,
}))
const toastMock = vi.hoisted(() => ({ error: vi.fn(), failed: vi.fn() }))
vi.mock('./Toast/toastStore', () => ({ toast: toastMock }))
reportFailedThroughError(toastMock)

const notifyRule = (id: string) => ({
  id,
  kind: 'notify',
  event_type: 'github:pr:ci_check_failed',
  name: `Rule ${id}`,
  creator_user_id: 'someone',
  enabled: true,
  source: 'user',
  team_id: 'team-a',
  scope_predicate_json: null,
})

type Body = Record<string, unknown>

// stubRules answers the list read with two rules, a single read by id, and a
// PATCH with patchAnswer.
function stubRules(patchAnswer?: { status: number; body: unknown }) {
  const rules = [notifyRule('first'), notifyRule('second')]
  const bodies: Body[] = []
  const fetchMock = vi.fn().mockImplementation((url: string, init?: RequestInit) => {
    const u = String(url)
    if (u.endsWith('/list')) {
      bodies.push(JSON.parse(String(init?.body ?? '{}')) as Body)
      return Promise.resolve({ ok: true, status: 200, ...listBody(rules) })
    }
    if (init?.method === 'PATCH' && patchAnswer) {
      const ok = patchAnswer.status < 300
      return Promise.resolve({ ok, status: patchAnswer.status, ...jsonBody(patchAnswer.body) })
    }
    const id = decodeURIComponent(u.split('/').pop() ?? '')
    const rule = rules.find((r) => r.id === id)
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

  it("reads the viewer's teams' notify rules as one list", async () => {
    const { bodies } = stubRules()
    render(<NotifyRulesPanel open onClose={() => {}} />)

    await waitFor(() => expect(screen.getByText('Rule first')).toBeTruthy())
    expect(screen.getByText('Rule second')).toBeTruthy()
    expect(bodies).toEqual([{ kind: 'notify', page_size: 50 }])
  })

  it("shows the server's refusal of a toggle and puts the switch back", async () => {
    const message = 'You cannot change event handlers on this team.'
    stubRules({ status: 403, body: { errors: [{ reason: 'FORBIDDEN', message }] } })
    render(<NotifyRulesPanel open onClose={() => {}} />)

    const toggle = await screen.findByLabelText('Enable Rule first')
    expect(toggle.hasAttribute('disabled')).toBe(false)
    fireEvent.click(toggle)

    await waitFor(() => expect(toastMock.error).toHaveBeenCalledWith(message))
    expect(toggle.getAttribute('aria-checked')).toBe('true')
  })

  it('opens the linked rule by its own read', async () => {
    const { fetchMock } = stubRules()
    render(<NotifyRulesPanel open onClose={() => {}} openRuleId="second" />)

    await waitFor(() => expect(screen.getByText('editor second')).toBeTruthy())
    expect(fetchMock).toHaveBeenCalledWith('/api/event-handlers/second', expect.anything())
    expect(toastMock.error).not.toHaveBeenCalled()
  })

  it('says so when the linked rule is gone', async () => {
    stubRules()
    render(<NotifyRulesPanel open onClose={() => {}} openRuleId="gone" />)

    await waitFor(() => expect(toastMock.error).toHaveBeenCalledTimes(1))
    expect(screen.queryByText(/^editor/)).toBeNull()
  })
})
