import { describe, it, expect, vi } from 'vitest'
import { render, screen, waitFor } from '@testing-library/react'
import TriggerConfigPanel from './TriggerConfigPanel'
import type { TriggerHandler } from '../types'
import { jsonBody } from '../test/apiResponse'

// What's under test is the header's blueprint badge, so the predicate editor
// (which reads its own schema) is a stand-in.
vi.mock('./PredicateEditor', () => ({ default: () => null }))
vi.mock('../hooks/useEventSources', () => ({
  useEventSources: () => ({ stateOf: () => 'available', canProduce: () => true }),
}))

const trigger: TriggerHandler = {
  id: 'trigger-1',
  kind: 'trigger',
  team_id: 'team-a',
  event_type: 'github:pr:ci_check_failed',
  scope_predicate_json: null,
  enabled: true,
  source: 'user',
  applies_to_unowned: false,
  created_at: '2026-11-02T00:00:00Z',
  updated_at: '2026-11-02T00:00:00Z',
  blueprint_id: 'bp/1',
  trigger_type: 'event',
  breaker_threshold: 4,
  min_autonomy_suitability: 0,
}

describe('TriggerConfigPanel blueprint badge', () => {
  it('shows the bound blueprint name read from GET /api/blueprints/{id}', async () => {
    const fetchMock = vi.fn((url: string) => {
      if (url === '/api/blueprints/bp%2F1') {
        return Promise.resolve({
          ok: true,
          status: 200,
          ...jsonBody({ id: 'bp/1', name: 'Fix CI' }),
        })
      }
      if (url === '/api/event-types') {
        return Promise.resolve({ ok: true, status: 200, ...jsonBody([]) })
      }
      return Promise.resolve({ ok: false, status: 404, ...jsonBody({ errors: [] }) })
    })
    vi.stubGlobal('fetch', fetchMock)

    render(
      <TriggerConfigPanel
        open
        trigger={trigger}
        onClose={() => {}}
        onSaved={() => {}}
        onDeleted={() => {}}
      />,
    )

    await waitFor(() => expect(screen.getByText('Fix CI')).toBeTruthy())
    expect(fetchMock).toHaveBeenCalledWith('/api/blueprints/bp%2F1', expect.anything())
    expect(fetchMock.mock.calls.map(([url]) => url)).not.toContain('/api/blueprints')
  })
})
