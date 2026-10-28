// A start that fails after beforeStart ran is reported as a toast as well as
// inline: beforeStart can change what the page renders and unmount the form,
// taking the inline error with it.
import { describe, it, expect, vi, beforeEach } from 'vitest'
import { render, screen, fireEvent, waitFor } from '@testing-library/react'

const ghMocks = vi.hoisted(() => ({ startManagedGitHubConnectAccount: vi.fn() }))
vi.mock('../../lib/githubApp', () => ({
  startManagedGitHubConnectAccount: ghMocks.startManagedGitHubConnectAccount,
}))

const toasts = vi.hoisted(() => ({
  toast: { success: vi.fn(), error: vi.fn(), info: vi.fn(), warning: vi.fn() },
}))
vi.mock('../../components/Toast/toastStore', () => toasts)

import ConnectGitHubAccount from './ConnectGitHubAccount'

function connect(beforeStart?: (account: string) => Promise<boolean>) {
  render(<ConnectGitHubAccount orgId="org-1" label="Connect GitHub…" beforeStart={beforeStart} />)
  fireEvent.click(screen.getByRole('button', { name: 'Connect GitHub…' }))
  fireEvent.change(screen.getByRole('textbox'), { target: { value: 'acme' } })
  fireEvent.click(screen.getByRole('button', { name: 'Connect' }))
}

beforeEach(() => {
  ghMocks.startManagedGitHubConnectAccount.mockReset()
  toasts.toast.error.mockReset()
})

describe('ConnectGitHubAccount · a refused start', () => {
  it('toasts the message when beforeStart ran first', async () => {
    ghMocks.startManagedGitHubConnectAccount.mockRejectedValue(
      new Error('This organization is pointed at https://ghe.example.com.'),
    )
    connect(async () => true)

    await waitFor(() => {
      expect(toasts.toast.error).toHaveBeenCalledWith(
        'This organization is pointed at https://ghe.example.com.',
      )
    })
    expect(
      screen.getByText('This organization is pointed at https://ghe.example.com.'),
    ).toBeInTheDocument()
  })

  it('shows the message inline only when there is no beforeStart', async () => {
    ghMocks.startManagedGitHubConnectAccount.mockRejectedValue(new Error('refused'))
    connect()

    expect(await screen.findByText('refused')).toBeInTheDocument()
    expect(toasts.toast.error).not.toHaveBeenCalled()
  })

  it('shows a beforeStart failure inline only', async () => {
    connect(async () => {
      throw new Error('teardown refused')
    })

    expect(await screen.findByText('teardown refused')).toBeInTheDocument()
    expect(ghMocks.startManagedGitHubConnectAccount).not.toHaveBeenCalled()
    expect(toasts.toast.error).not.toHaveBeenCalled()
  })
})
