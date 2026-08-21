import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest'
import { render, screen, waitFor } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import EventSourcesGroup from './EventSourcesGroup'
import { resetEventSourcesForTest, type EventSourceAvailability } from '../../hooks/useEventSources'
import { LOCAL_DEFAULT_ORG_ID } from '../../lib/githubApp'
import { jsonBody } from '../../test/apiResponse'

const sourcesPath = `/api/orgs/${LOCAL_DEFAULT_ORG_ID}/sources`

// Every source state at once, so one render covers which rows offer a control
// and which only explain themselves.
const everyState: EventSourceAvailability[] = [
  { kind: 'github', state: 'available' },
  { kind: 'jira', state: 'disabled' },
  { kind: 'slack', state: 'unconfigured' },
  { kind: 'linear', state: 'wip' },
]

function stub(sources: EventSourceAvailability[]) {
  const fetchMock = vi.fn((input: unknown, init?: { method?: string }) => {
    const path = String(input).split('?')[0]
    if (path === sourcesPath) return Promise.resolve({ ok: true, ...jsonBody({ sources }) })
    if (path.startsWith(`${sourcesPath}/`) && init?.method === 'PATCH') {
      return Promise.resolve({ ok: true, ...jsonBody({ kind: 'jira', state: 'available' }) })
    }
    return Promise.resolve({ ok: false, status: 404, ...jsonBody({}) })
  })
  vi.stubGlobal('fetch', fetchMock)
  return fetchMock
}

beforeEach(() => resetEventSourcesForTest())
afterEach(() => vi.unstubAllGlobals())

describe('EventSourcesGroup', () => {
  it('offers a switch only where there is something to pause', async () => {
    stub(everyState)
    render(<EventSourcesGroup orgId={LOCAL_DEFAULT_ORG_ID} canEdit />)

    await waitFor(() => expect(screen.getByLabelText('GitHub events')).toBeTruthy())
    // Available and disabled are the two states a pause moves between.
    expect(screen.getByLabelText('GitHub events').getAttribute('aria-checked')).toBe('true')
    expect(screen.getByLabelText('Jira events').getAttribute('aria-checked')).toBe('false')
    // Unconfigured, unlicensed and wip are already off for reasons this switch
    // does not control — a toggle there would change nothing visible.
    expect(screen.queryByLabelText('Slack events')).toBeNull()
    expect(screen.queryByLabelText('Linear events')).toBeNull()
  })

  it('says a paused source is paused, not unconnected', async () => {
    stub(everyState)
    render(<EventSourcesGroup orgId={LOCAL_DEFAULT_ORG_ID} canEdit />)

    await waitFor(() => expect(screen.getByText(/Paused\./)).toBeTruthy())
    // The distinction the whole ticket rests on: a pause leaves the credential
    // bound, so agent access is unaffected.
    expect(screen.getByText(/agents can still read and write/)).toBeTruthy()
    expect(screen.getByText(/Slack is not connected yet/)).toBeTruthy()
  })

  it('renders no switch at all for a non-admin', async () => {
    stub(everyState)
    render(<EventSourcesGroup orgId={LOCAL_DEFAULT_ORG_ID} canEdit={false} />)

    await waitFor(() => expect(screen.getByText(/Paused\./)).toBeTruthy())
    expect(screen.queryByRole('switch')).toBeNull()
  })

  it('PATCHes the source it was flipped on', async () => {
    const fetchMock = stub(everyState)
    render(<EventSourcesGroup orgId={LOCAL_DEFAULT_ORG_ID} canEdit />)

    await waitFor(() => expect(screen.getByLabelText('Jira events')).toBeTruthy())
    await userEvent.click(screen.getByLabelText('Jira events'))

    await waitFor(() =>
      expect(fetchMock).toHaveBeenCalledWith(
        `${sourcesPath}/jira`,
        expect.objectContaining({ method: 'PATCH', body: JSON.stringify({ disabled: false }) }),
      ),
    )
  })
})
