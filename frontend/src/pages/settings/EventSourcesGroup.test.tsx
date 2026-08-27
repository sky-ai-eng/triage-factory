import { describe, it, expect, vi, beforeEach } from 'vitest'
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
  { kind: 'github', state: 'available', disableable: false },
  { kind: 'jira', state: 'disabled', disableable: true },
  { kind: 'slack', state: 'unconfigured', disableable: true },
  { kind: 'linear', state: 'wip', disableable: true },
]

function stub(sources: EventSourceAvailability[]) {
  const fetchMock = vi.fn((input: unknown, init?: { method?: string }) => {
    const path = String(input).split('?')[0]
    if (path === sourcesPath) return Promise.resolve({ ok: true, ...jsonBody({ sources }) })
    if (path.startsWith(`${sourcesPath}/`) && init?.method === 'PATCH') {
      return Promise.resolve({
        ok: true,
        ...jsonBody({ kind: 'jira', state: 'available', disableable: true }),
      })
    }
    return Promise.resolve({ ok: false, status: 404, ...jsonBody({}) })
  })
  vi.stubGlobal('fetch', fetchMock)
  return fetchMock
}

beforeEach(() => resetEventSourcesForTest())

// Two sources an org can actually turn off. Every REGISTERED source is
// disableable — required is a core-only property — so an ee build renders more
// than one switch today.
const twoDisableable: EventSourceAvailability[] = [
  { kind: 'github', state: 'available', disableable: false },
  { kind: 'jira', state: 'available', disableable: true },
  { kind: 'slack', state: 'available', disableable: true },
]

describe('EventSourcesGroup', () => {
  it('offers a switch only where there is something to turn off', async () => {
    stub(everyState)
    render(<EventSourcesGroup orgId={LOCAL_DEFAULT_ORG_ID} canEdit />)

    await waitFor(() => expect(screen.getByLabelText('Jira')).toBeTruthy())
    // Available and disabled are the two states the switch moves between.
    expect(screen.getByLabelText('Jira').getAttribute('aria-checked')).toBe('false')
    // GitHub is available and still has no control: the server says it is not
    // disableable, so offering one would be offering a write the PATCH refuses.
    expect(screen.queryByLabelText('GitHub')).toBeNull()
    // Unconfigured, unlicensed and wip are already off for reasons this switch
    // does not control — a toggle there would change nothing visible.
    expect(screen.queryByLabelText('Slack')).toBeNull()
    expect(screen.queryByLabelText('Linear')).toBeNull()
  })

  it('explains why the required source has no switch', async () => {
    stub(everyState)
    render(<EventSourcesGroup orgId={LOCAL_DEFAULT_ORG_ID} canEdit />)

    // Without this the missing control reads as a bug, or as a permission the
    // admin does not have — both wrong, and both lead somewhere unhelpful.
    await waitFor(() => expect(screen.getByText(/Always on\./)).toBeTruthy())
    expect(screen.getByText(/no off switch/)).toBeTruthy()
  })

  it('says a turned-off source is off for agents too, and still connected', async () => {
    stub(everyState)
    render(<EventSourcesGroup orgId={LOCAL_DEFAULT_ORG_ID} canEdit />)

    await waitFor(() => expect(screen.getByText(/Off —/)).toBeTruthy())
    // The row has to name all three effects. An admin who reads only "no new
    // tasks" would not learn that the agents lost their access too, which is
    // the thing they most need to know before flipping it.
    expect(screen.getByText(/not polled/)).toBeTruthy()
    expect(screen.getByText(/creating no tasks/)).toBeTruthy()
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

  it('disables only the switch whose PATCH is in flight', async () => {
    // A single shared `pending` would re-enable Jira's control the moment
    // Slack was clicked, letting a second PATCH go out on Jira while its first
    // was still open.
    const settle: Array<() => void> = []
    vi.stubGlobal(
      'fetch',
      vi.fn((input: unknown, init?: { method?: string }) => {
        const path = String(input).split('?')[0]
        if (path === sourcesPath) {
          return Promise.resolve({ ok: true, ...jsonBody({ sources: twoDisableable }) })
        }
        if (init?.method === 'PATCH') {
          // Park the write open so both rows are observable mid-flight.
          return new Promise((resolve) => {
            settle.push(() =>
              resolve({ ok: true, ...jsonBody({ kind: 'jira', state: 'disabled' }) }),
            )
          })
        }
        return Promise.resolve({ ok: false, status: 404, ...jsonBody({}) })
      }),
    )
    render(<EventSourcesGroup orgId={LOCAL_DEFAULT_ORG_ID} canEdit />)

    await waitFor(() => expect(screen.getByLabelText('Jira')).toBeTruthy())
    await userEvent.click(screen.getByLabelText('Jira'))
    await waitFor(() => expect(settle).toHaveLength(1))

    // Jira is busy; Slack is untouched and still clickable.
    expect(screen.getByLabelText('Jira').hasAttribute('disabled')).toBe(true)
    expect(screen.getByLabelText('Slack').hasAttribute('disabled')).toBe(false)

    await userEvent.click(screen.getByLabelText('Slack'))
    await waitFor(() => expect(settle).toHaveLength(2))

    // Slack going busy must not have released Jira.
    expect(screen.getByLabelText('Jira').hasAttribute('disabled')).toBe(true)
    expect(screen.getByLabelText('Slack').hasAttribute('disabled')).toBe(true)
    settle.forEach((fn) => fn())
  })

  it('keeps the section rendered while the refetch after a toggle is in flight', async () => {
    // The toggle invalidates; if that dropped the cache the whole section would
    // collapse to "Loading event sources…" on every flip.
    stub(everyState)
    render(<EventSourcesGroup orgId={LOCAL_DEFAULT_ORG_ID} canEdit />)

    await waitFor(() => expect(screen.getByLabelText('Jira')).toBeTruthy())
    await userEvent.click(screen.getByLabelText('Jira'))

    expect(screen.queryByText(/Loading event sources/)).toBeNull()
    expect(screen.getByLabelText('Jira')).toBeTruthy()
  })
})
