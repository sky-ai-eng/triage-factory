import { describe, it, expect, vi, beforeEach } from 'vitest'
import { render, screen, waitFor } from '@testing-library/react'
import TaskRulesPanel from './TaskRulesPanel'
import { jsonBody } from '../test/apiResponse'

// What's under test is opening a linked rule: the editor is a stand-in that
// shows which rule it was opened on.
vi.mock('./TaskRuleEditor', () => ({
  default: (props: { open: boolean; rule: { id: string } | null }) =>
    props.open ? <p>editor {props.rule?.id ?? 'new'}</p> : null,
}))
vi.mock('../hooks/useEventSources', () => ({
  useEventSources: () => ({ stateOf: () => 'available', canProduce: () => true }),
}))
const toastMock = vi.hoisted(() => ({ error: vi.fn() }))
vi.mock('./Toast/toastStore', () => ({ toast: toastMock }))

const rule = (id: string) => ({
  id,
  kind: 'rule',
  event_type: 'jira:issue:assigned',
  name: `Rule ${id}`,
  enabled: true,
  source: 'user',
  team_id: 'team-a',
  scope_predicate_json: null,
  default_priority: 0.5,
  sort_order: 0,
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

describe('TaskRulesPanel linked rule', () => {
  beforeEach(() => {
    toastMock.error.mockReset()
  })

  it('opens the editor on the rule the link names', async () => {
    stubRules('rule-1', 'rule-2')
    render(<TaskRulesPanel open onClose={() => {}} openRuleId="rule-2" />)

    await waitFor(() => expect(screen.getByText('editor rule-2')).toBeTruthy())
    expect(toastMock.error).not.toHaveBeenCalled()
  })

  it('says so when the linked rule is not in the list', async () => {
    stubRules('rule-1')
    render(<TaskRulesPanel open onClose={() => {}} openRuleId="rule-gone" />)

    await waitFor(() => expect(toastMock.error).toHaveBeenCalledTimes(1))
    expect(screen.queryByText(/^editor/)).toBeNull()
  })

  it('opens no editor without a link', async () => {
    const fetchMock = stubRules('rule-1')
    render(<TaskRulesPanel open onClose={() => {}} />)

    await waitFor(() => expect(fetchMock).toHaveBeenCalled())
    await waitFor(() => expect(screen.getByText('Rule rule-1')).toBeTruthy())
    expect(screen.queryByText(/^editor/)).toBeNull()
  })
})
