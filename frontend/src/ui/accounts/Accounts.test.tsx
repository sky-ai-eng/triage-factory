import { render, screen, fireEvent, waitFor, within } from '@testing-library/react'
import { describe, expect, it, vi } from 'vitest'
import Accounts from './Accounts'
import type { Account } from './Accounts'

// The verb rule and the band's bodies are the design; what a screenshot cannot
// show is that the body follows the ORG's method rather than the reader's
// choice, that a refusal keeps what was typed, and that a success collapses
// the band and marks the value once.

const bound: Account[] = [
  { id: 'gh', kind: 'github', account: '@aallchin', host: 'github.com', method: 'app' },
  {
    id: 'jira',
    kind: 'jira',
    account: 'aidan@allchin.com',
    host: 'sky-ai.atlassian.net',
    method: 'cloud',
  },
]

describe('Accounts — the verb rule', () => {
  it('offers Change for a bound GitHub, Reconnect for a bound Jira, Connect for neither', () => {
    render(<Accounts accounts={bound} />)
    expect(screen.getByRole('button', { name: 'Change' })).toBeInTheDocument()
    expect(screen.getByRole('button', { name: 'Reconnect' })).toBeInTheDocument()

    render(<Accounts accounts={bound.map((a) => ({ ...a, account: null }))} />)
    const connects = screen.getAllByRole('button', { name: 'Connect' })
    expect(connects).toHaveLength(2)
    // Absence is the ask: Connect is the only warm verb.
    expect(connects[0]).toHaveAttribute('data-tone', 'warm')
    expect(screen.getByText('not connected for github.com')).toBeInTheDocument()
  })

  it('removes the verbs, never disables them, for a readout or offline', () => {
    const { rerender } = render(<Accounts accounts={bound} interactive={false} />)
    expect(screen.queryByRole('button')).not.toBeInTheDocument()

    rerender(<Accounts accounts={bound} offline />)
    expect(screen.queryByRole('button')).not.toBeInTheDocument()
    // A stale account is worse than none.
    expect(screen.getAllByText('--')).toHaveLength(3)
  })

  it('holds its height while loading, with nothing announced', () => {
    const { container } = render(<Accounts accounts={[]} loading />)
    expect(container.querySelector('.ac')).toHaveAttribute('aria-busy', 'true')
    expect(container.querySelectorAll('.ac-skel')).toHaveLength(2)
  })
})

describe('Accounts — the band', () => {
  it("follows the org's method: an App offers Continue with GitHub, with the token path as a link", () => {
    const onConnect = vi.fn()
    render(<Accounts accounts={bound} onConnect={onConnect} />)
    fireEvent.click(screen.getByRole('button', { name: 'Change' }))

    // The verb became Cancel where it stood.
    expect(screen.getByRole('button', { name: 'Cancel' })).toBeInTheDocument()
    fireEvent.click(screen.getByRole('button', { name: 'Continue with GitHub' }))
    expect(onConnect).toHaveBeenCalledWith('gh')

    // The token path is under it, not beside it.
    fireEvent.click(screen.getByRole('button', { name: 'paste a personal access token' }))
    expect(screen.getByLabelText('PERSONAL ACCESS TOKEN · GITHUB.COM')).toBeInTheDocument()
  })

  it('opens the fields at once for a PAT org, and Verify waits for them', async () => {
    const onVerify = vi.fn().mockResolvedValue('@corp-login')
    const onChange = vi.fn()
    render(
      <Accounts
        accounts={[{ ...bound[0], method: 'pat' }]}
        onVerify={onVerify}
        onChange={onChange}
      />,
    )
    fireEvent.click(screen.getByRole('button', { name: 'Change' }))
    const verify = screen.getByRole('button', { name: 'Verify' })
    expect(verify).toBeDisabled()

    fireEvent.change(screen.getByLabelText('PERSONAL ACCESS TOKEN · GITHUB.COM'), {
      target: { value: 'ghp_x' },
    })
    expect(verify).toBeEnabled()
    fireEvent.click(verify)
    expect(onVerify).toHaveBeenCalledWith('gh', { token: 'ghp_x' })

    // Success: the band collapses, the value line changes and is marked once.
    await waitFor(() =>
      expect(screen.queryByRole('button', { name: 'Cancel' })).not.toBeInTheDocument(),
    )
    const value = screen.getByText('@corp-login · github.com')
    expect(value).toHaveClass('ac-tick')
    expect(onChange).toHaveBeenCalledWith('gh', '@corp-login')
  })

  it("prints a refusal in the server's words and keeps the field's contents", async () => {
    const onVerify = vi
      .fn()
      .mockRejectedValue(new Error('401 from github.com — the token was refused'))
    render(<Accounts accounts={[{ ...bound[0], method: 'pat' }]} onVerify={onVerify} />)
    fireEvent.click(screen.getByRole('button', { name: 'Change' }))
    const field = screen.getByLabelText('PERSONAL ACCESS TOKEN · GITHUB.COM')
    fireEvent.change(field, { target: { value: 'bad-token' } })
    fireEvent.keyDown(field, { key: 'Enter' })

    expect(
      await screen.findByText('401 from github.com — the token was refused'),
    ).toBeInTheDocument()
    expect(field).toHaveValue('bad-token')
    expect(field).toHaveAttribute('aria-invalid', 'true')
    // Still open — the reader corrects and tries again.
    expect(screen.getByRole('button', { name: 'Cancel' })).toBeInTheDocument()
  })

  it('takes two fields for Atlassian Cloud and one for Data Center', () => {
    const { rerender } = render(<Accounts accounts={[bound[1]]} />)
    fireEvent.click(screen.getByRole('button', { name: 'Reconnect' }))
    expect(screen.getByLabelText('ATLASSIAN ACCOUNT EMAIL')).toBeInTheDocument()
    expect(screen.getByLabelText('API TOKEN')).toBeInTheDocument()

    rerender(<Accounts accounts={[{ ...bound[1], id: 'jira2', method: 'dc' }]} />)
    fireEvent.click(screen.getByRole('button', { name: 'Reconnect' }))
    expect(screen.getByLabelText('PERSONAL ACCESS TOKEN')).toBeInTheDocument()
    expect(screen.queryByLabelText('ATLASSIAN ACCOUNT EMAIL')).not.toBeInTheDocument()
  })

  it('closes on Escape and on Cancel without sending', () => {
    const onVerify = vi.fn()
    const { container } = render(<Accounts accounts={bound} onVerify={onVerify} />)
    fireEvent.click(screen.getByRole('button', { name: 'Reconnect' }))
    expect(container.querySelector('.ac-band')).toBeInTheDocument()
    fireEvent.keyDown(document, { key: 'Escape' })
    expect(container.querySelector('.ac-band')).not.toBeInTheDocument()

    fireEvent.click(screen.getByRole('button', { name: 'Reconnect' }))
    fireEvent.click(
      within(container.querySelector('.ac-band') as HTMLElement).getByRole('button', {
        name: 'Cancel',
      }),
    )
    expect(container.querySelector('.ac-band')).not.toBeInTheDocument()
    expect(onVerify).not.toHaveBeenCalled()
  })
})
