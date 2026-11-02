import { act, render, screen, waitFor } from '@testing-library/react'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { cancelInvoke, requestInvoke, useNotificationActions } from './useNotificationActions'
import { NotificationConfirm, NotificationStrip } from '../components/ShellNotifications'
import { jsonBody } from '../test/apiResponse'
import { installFakeAnimations } from '../ui/shared/fakeAnimation'
import type { NotificationAction, NotificationItem } from '../lib/notifications'

// What is pinned is what the viewer is promised wherever an invoke is offered:
// it is out at most once per row, the outcome they are told is the action's own
// words at the level its result earns, and a confirmation is asked once, in the
// org it was asked in.

const toasts = { success: vi.fn(), warning: vi.fn(), info: vi.fn(), error: vi.fn() }
vi.mock('../components/Toast/toastStore', () => ({
  toast: {
    success: (m: string) => toasts.success(m),
    warning: (m: string) => toasts.warning(m),
    info: (m: string) => toasts.info(m),
    error: (m: string) => toasts.error(m),
  },
}))
let orgId = 'org-1'
vi.mock('./useApiOrgId', () => ({ useApiOrgId: () => orgId }))

function note(id: string, over: Partial<NotificationItem> = {}): NotificationItem {
  const now = new Date().toISOString()
  return {
    id,
    kind: 'memory.generation_failing',
    lifecycle: 'condition',
    priority: 'urgent',
    audience: { scope: 'org', role: 'admins', team_id: null, user_id: null },
    title: `title ${id}`,
    body: '',
    dismissible: false,
    first_seen_at: now,
    last_seen_at: now,
    seen_count: 1,
    resolved_at: null,
    resolved_reason: null,
    resolution: null,
    expires_at: null,
    actions: [],
    read: false,
    dismissed: false,
    sent: true,
    ...over,
  }
}

const retry: NotificationAction = {
  id: 'retry',
  type: 'invoke',
  label: 'Retry now',
  running_label: 'Retrying…',
  allowed: true,
}
const untrack: NotificationAction = {
  id: 'untrack',
  type: 'invoke',
  label: 'Stop tracking SEC',
  running_label: 'Stopping…',
  confirm: 'This team stops tracking SEC.',
  allowed: true,
}

/** The action route answers with `answer`, once `gate` lets it. */
let answer: () => { status: number; body: unknown }
let gate: Promise<void>
const fetchMock = vi.fn(async () => {
  await gate
  const { status, body } = answer()
  return { ok: status < 400, status, headers: new Headers(), ...jsonBody(body) }
})

function Running() {
  const { running } = useNotificationActions()
  return <span data-testid="running">{[...running.keys()].join(',')}</span>
}

beforeEach(() => {
  orgId = 'org-1'
  gate = Promise.resolve()
  answer = () => ({ status: 200, body: { notification: note('a'), outcome: 'Still failing.' } })
  fetchMock.mockClear()
  for (const t of Object.values(toasts)) t.mockClear()
  vi.stubGlobal('fetch', fetchMock)
})
afterEach(() => {
  cancelInvoke()
  vi.restoreAllMocks()
})

describe('the invoke runner', () => {
  it('runs a row’s invoke once while it is out, and reports it running', async () => {
    let open!: () => void
    gate = new Promise((r) => (open = r))
    render(<Running />)

    const then = vi.fn()
    act(() => requestInvoke('org-1', note('a'), retry, then))
    act(() => requestInvoke('org-1', note('a'), retry, then))
    expect(fetchMock).toHaveBeenCalledTimes(1)
    expect(screen.getByTestId('running')).toHaveTextContent('a')

    await act(async () => open())
    await waitFor(() => expect(screen.getByTestId('running')).toHaveTextContent(''))
    expect(then).toHaveBeenCalledTimes(1)
  })

  it.each([
    [
      'resolved the row',
      { notification: note('a', { resolved_at: 'now' }), outcome: 'Fixed.' },
      'success',
    ],
    ['left it open', { notification: note('a'), outcome: 'Retrying now.' }, 'info'],
    ['took it out of reach', { notification: null, outcome: 'Done.' }, 'info'],
  ] as const)('tells the outcome in the action’s words when it %s', async (_, body, level) => {
    answer = () => ({ status: 200, body })
    const then = vi.fn()
    await act(async () => requestInvoke('org-1', note('a'), retry, then))
    await waitFor(() => expect(toasts[level]).toHaveBeenCalledWith(body.outcome))
    expect(then).toHaveBeenCalledWith(body.notification)
  })

  it.each([
    [409, 'info'],
    [403, 'error'],
    [500, 'error'],
  ] as const)(
    'tells a %d in the server’s words, and offers the action again',
    async (status, level) => {
      answer = () => ({
        status,
        body: { errors: [{ reason: 'X', message: `answered ${status}` }] },
      })
      const then = vi.fn()
      await act(async () => requestInvoke('org-1', note('a'), retry, then))
      await waitFor(() => expect(toasts[level]).toHaveBeenCalledWith(`answered ${status}`))
      expect(then).toHaveBeenCalledWith(undefined)

      // Not out any more: a second press runs it again.
      await act(async () => requestInvoke('org-1', note('a'), retry))
      expect(fetchMock).toHaveBeenCalledTimes(2)
    },
  )
})

describe('NotificationConfirm', () => {
  it('asks before an invoke that asks first, as a destructive decision', async () => {
    render(<NotificationConfirm />)
    act(() => requestInvoke('org-1', note('a'), untrack))
    expect(await screen.findByText('Stop tracking SEC?')).toBeInTheDocument()
    expect(screen.getByText('This team stops tracking SEC.')).toBeInTheDocument()
    expect(fetchMock).not.toHaveBeenCalled()

    act(() => screen.getByRole('button', { name: 'Stop tracking SEC' }).click())
    await waitFor(() =>
      expect(fetchMock).toHaveBeenCalledWith(
        '/api/me/notifications/a/actions/untrack',
        expect.objectContaining({ method: 'POST' }),
      ),
    )
  })

  it('sends nothing when cancelled', async () => {
    render(<NotificationConfirm />)
    act(() => requestInvoke('org-1', note('a'), untrack))
    act(() => screen.getByRole('button', { name: 'Cancel' }).click())
    await waitFor(() => expect(screen.queryByText('Stop tracking SEC?')).not.toBeInTheDocument())
    expect(fetchMock).not.toHaveBeenCalled()
  })

  it('drops an invoke asked in another org, and does not bring it back', async () => {
    const { rerender } = render(<NotificationConfirm />)
    act(() => requestInvoke('org-1', note('a'), untrack))
    expect(await screen.findByText('Stop tracking SEC?')).toBeInTheDocument()

    orgId = 'org-2'
    rerender(<NotificationConfirm />)
    await waitFor(() => expect(screen.queryByText('Stop tracking SEC?')).not.toBeInTheDocument())
    orgId = 'org-1'
    rerender(<NotificationConfirm />)
    expect(screen.queryByText('Stop tracking SEC?')).not.toBeInTheDocument()
  })
})

describe('NotificationStrip', () => {
  it('shows a row’s invoke running while it is out, from wherever it was started', async () => {
    vi.spyOn(document, 'hasFocus').mockReturnValue(true)
    installFakeAnimations()
    let open!: () => void
    gate = new Promise((r) => (open = r))
    const item = note('a', { actions: [retry] })
    const feed = {
      count: 1,
      urgent: [item],
      arrival: null,
      arrivalDone: vi.fn(),
      open: vi.fn(),
      act: vi.fn(),
    }
    render(<NotificationStrip feed={feed} teams={[]} held={false} />)
    expect(screen.getByRole('button', { name: 'Retry now' })).not.toHaveAttribute('aria-disabled')

    // Started elsewhere: the page's own button.
    act(() => requestInvoke('org-1', item, retry))
    const busy = screen.getByRole('button', { name: 'Retrying…' })
    expect(busy).toHaveAttribute('aria-disabled', 'true')
    act(() => busy.click())
    expect(feed.act).not.toHaveBeenCalled()

    await act(async () => open())
    expect(await screen.findByRole('button', { name: 'Retry now' })).toBeInTheDocument()
  })
})
