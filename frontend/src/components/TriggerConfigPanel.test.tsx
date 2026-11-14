import { describe, it, expect, vi } from 'vitest'
import { render, screen, waitFor, fireEvent } from '@testing-library/react'
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

describe('TriggerConfigPanel on an event the watch flag cannot widen', () => {
  it('hides the toggle and leaves applies_to_unowned out of the save', async () => {
    const fetchMock = vi.fn((url: string, init?: RequestInit) => {
      if (url === '/api/event-types') {
        return Promise.resolve({
          ok: true,
          status: 200,
          ...jsonBody([
            {
              id: 'github:vulnerability:alert_opened',
              source: 'github',
              category: 'vulnerability',
              label: 'Vulnerability Alert Opened',
              description: '',
              supports_watch: false,
            },
          ]),
        })
      }
      if (init?.method === 'PATCH') {
        return Promise.resolve({ ok: true, status: 200, ...jsonBody({}) })
      }
      return Promise.resolve({ ok: true, status: 200, ...jsonBody({ name: 'Upgrade' }) })
    })
    vi.stubGlobal('fetch', fetchMock)
    const onSaved = vi.fn()

    render(
      <TriggerConfigPanel
        open
        trigger={{ ...trigger, event_type: 'github:vulnerability:alert_opened' }}
        onClose={() => {}}
        onSaved={onSaved}
        onDeleted={() => {}}
      />,
    )

    await waitFor(() =>
      expect(fetchMock.mock.calls.map(([url]) => url)).toContain('/api/event-types'),
    )
    await waitFor(() =>
      expect(screen.queryByText('Apply to entities outside this team')).not.toBeInTheDocument(),
    )
    fireEvent.click(screen.getByRole('button', { name: 'Save' }))

    await waitFor(() => expect(onSaved).toHaveBeenCalled())
    const patch = fetchMock.mock.calls.find(([, init]) => init?.method === 'PATCH')
    const body = JSON.parse(patch![1]!.body as string)
    expect(body).not.toHaveProperty('applies_to_unowned')
    expect(body).toHaveProperty('breaker_threshold', 4)
  })
})
