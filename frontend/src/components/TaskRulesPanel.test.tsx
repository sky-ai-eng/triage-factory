import { describe, it, expect, vi, beforeEach } from 'vitest'
import { fireEvent, render, screen, waitFor } from '@testing-library/react'
import TaskRulesPanel from './TaskRulesPanel'
import { jsonBody } from '../test/apiResponse'

// What's under test is opening a linked handler and the row's mode control:
// the editor is a stand-in that shows which handler it was opened on.
vi.mock('./TaskRuleEditor', () => ({
  default: (props: { open: boolean; rule: { id: string } | null; onClose: () => void }) =>
    props.open ? (
      <p>
        editor {props.rule?.id ?? 'new'}
        <button onClick={props.onClose}>close editor</button>
      </p>
    ) : null,
}))
vi.mock('../hooks/useEventSources', () => ({
  useEventSources: () => ({ stateOf: () => 'available', canProduce: () => true }),
}))
const toastMock = vi.hoisted(() => ({ error: vi.fn(), failed: vi.fn() }))
vi.mock('./Toast/toastStore', () => ({ toast: toastMock }))

const rule = (id: string) => ({
  id,
  mode: 'watch',
  event_type: 'jira:issue:assigned',
  name: `Rule ${id}`,
  source: 'user',
  team_id: 'team-a',
  scope_predicate_json: null,
  default_priority: 0.5,
  sort_order: 0,
  blueprint_id: null,
  breaker_threshold: 4,
  min_autonomy_suitability: 0,
  applies_to_unowned: false,
})

function stubRules(...ids: string[]) {
  const items = ids.map(rule)
  const fetchMock = vi.fn().mockResolvedValue({
    ok: true,
    status: 200,
    ...jsonBody({ items, next_page_token: '', total_count: items.length }),
  })
  vi.stubGlobal('fetch', fetchMock)
  return fetchMock
}

describe('TaskRulesPanel linked event handler', () => {
  beforeEach(() => {
    toastMock.error.mockReset()
  })

  it('opens the editor on the rule the link names', async () => {
    stubRules('rule-1', 'rule-2')
    render(<TaskRulesPanel open onClose={() => {}} openRuleId="rule-2" />)

    await waitFor(() => expect(screen.getByText(/editor rule-2/)).toBeTruthy())
    expect(toastMock.error).not.toHaveBeenCalled()
  })

  it('says so when the linked rule is not in the list', async () => {
    stubRules('rule-1')
    render(<TaskRulesPanel open onClose={() => {}} openRuleId="rule-gone" />)

    await waitFor(() => expect(toastMock.error).toHaveBeenCalledTimes(1))
    expect(screen.queryByText(/^editor/)).toBeNull()
  })

  it('opens the rule again when the same link is followed again', async () => {
    stubRules('rule-1')
    const { rerender } = render(<TaskRulesPanel open onClose={() => {}} openRuleId="rule-1" />)
    await waitFor(() => expect(screen.getByText(/editor rule-1/)).toBeTruthy())
    fireEvent.click(screen.getByText('close editor'))
    expect(screen.queryByText(/editor rule-1/)).toBeNull()

    rerender(<TaskRulesPanel open={false} onClose={() => {}} openRuleId={null} />)
    rerender(<TaskRulesPanel open onClose={() => {}} openRuleId="rule-1" />)
    await waitFor(() => expect(screen.getByText(/editor rule-1/)).toBeTruthy())
  })

  it('opens no editor without a link', async () => {
    const fetchMock = stubRules('rule-1')
    render(<TaskRulesPanel open onClose={() => {}} />)

    await waitFor(() => expect(fetchMock).toHaveBeenCalled())
    await waitFor(() => expect(screen.getByText('Rule rule-1')).toBeTruthy())
    expect(screen.queryByText(/^editor/)).toBeNull()
  })
})

describe('TaskRulesPanel mode control', () => {
  it('PATCHes the mode a row picks, and only the mode', async () => {
    const fetchMock = stubRules('rule-1')
    render(<TaskRulesPanel open onClose={() => {}} />)
    await waitFor(() => expect(screen.getByText('Rule rule-1')).toBeTruthy())

    fireEvent.click(screen.getByRole('radio', { name: 'Off' }))

    await waitFor(() =>
      expect(fetchMock).toHaveBeenCalledWith(
        '/api/event-handlers/rule-1',
        expect.objectContaining({ method: 'PATCH' }),
      ),
    )
    const patch = fetchMock.mock.calls.find(([, init]) => init?.method === 'PATCH')
    expect(JSON.parse(patch![1]!.body as string)).toEqual({ mode: 'off' })
    expect(screen.getByRole('radio', { name: 'Off' })).toHaveAttribute('aria-checked', 'true')
  })

  it('strikes automate on a handler with no blueprint', async () => {
    stubRules('rule-1')
    render(<TaskRulesPanel open onClose={() => {}} />)
    await waitFor(() => expect(screen.getByText('Rule rule-1')).toBeTruthy())

    expect(screen.getByRole('radio', { name: 'Automate' })).toHaveAttribute('aria-disabled', 'true')
    expect(screen.getByRole('radio', { name: 'Notify' })).not.toHaveAttribute('aria-disabled')
  })
})
