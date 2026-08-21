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

    await waitFor(() => expect(screen.getByLabelText('GitHub')).toBeTruthy())
    // Available and disabled are the two states a pause moves between.
    expect(screen.getByLabelText('GitHub').getAttribute('aria-checked')).toBe('true')
    expect(screen.getByLabelText('Jira').getAttribute('aria-checked')).toBe('false')
    // Unconfigured, unlicensed and wip are already off for reasons this switch
    // does not control — a toggle there would change nothing visible.
    expect(screen.queryByLabelText('Slack')).toBeNull()
    expect(screen.queryByLabelText('Linear')).toBeNull()
  })

  it('says a turned-off source is off for agents too, and still connected', async () => {
    stub(everyState)
    render(<EventSourcesGroup orgId={LOCAL_DEFAULT_ORG_ID} canEdit />)

    await waitFor(() => expect(screen.getByText(/Off —/)).toBeTruthy())
    // The row has to name all three effects. An admin who reads only "no new
    // tasks" would not learn that the agents lost their access too, which is
    // the thing they most need to know before flipping it.
    expect(screen.getByText(/not polled/)).toBeTruthy()
    expect(screen.getByText(/agents cannot reach it/)).toBeTruthy()
    // And it is still a pause, not a disconnect — otherwise the switch reads
    // as the destructive action it exists to replace.
    expect(screen.getByText(/credential is still stored/)).toBeTruthy()
    expect(screen.getByText(/Slack is not connected yet/)).toBeTruthy()
  })

  it('renders no switch at all for a non-admin', async () => {
    stub(everyState)
    render(<EventSourcesGroup orgId={LOCAL_DEFAULT_ORG_ID} canEdit={false} />)

    await waitFor(() => expect(screen.getByText(/Off —/)).toBeTruthy())
    expect(screen.queryByRole('switch')).toBeNull()
  })

  it('PATCHes the source it was flipped on', async () => {
    const fetchMock = stub(everyState)
    render(<EventSourcesGroup orgId={LOCAL_DEFAULT_ORG_ID} canEdit />)

    await waitFor(() => expect(screen.getByLabelText('Jira')).toBeTruthy())
    await userEvent.click(screen.getByLabelText('Jira'))

    await waitFor(() =>
      expect(fetchMock).toHaveBeenCalledWith(
        `${sourcesPath}/jira`,
        expect.objectContaining({ method: 'PATCH', body: JSON.stringify({ disabled: false }) }),
      ),
    )
  })
})
