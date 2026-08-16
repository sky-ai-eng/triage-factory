import { describe, it, expect, vi, beforeEach } from 'vitest'
import { render, screen, fireEvent, waitFor, within } from '@testing-library/react'
import InviteModal from './InviteModal'
import { HttpError } from '../lib/apiClient'

// useOrgRole drives the role ceiling; default the caller to 'admin'. Individual
// tests re-mock it to flex owner/member.
const roleMock = vi.hoisted(() => ({ role: 'admin' as string | null }))
vi.mock('../hooks/useOrgRole', () => ({
  useOrgRole: () => ({ role: roleMock.role, isAdmin: true, loading: false }),
}))

// The team picker reads useTeams; give it one team so the <select> has a real
// option beyond "No team".
vi.mock('../hooks/useTeams', () => ({
  useTeams: () => ({
    teams: [{ id: 'team-1', name: 'Platform', slug: 'platform', role: 'admin' }],
    lastActingTeamId: '',
    multi: false,
    loading: false,
    loaded: true,
    error: null,
    refresh: vi.fn(),
    createTeam: vi.fn(),
  }),
}))

describe('InviteModal', () => {
  beforeEach(() => {
    roleMock.role = 'admin'
    Object.defineProperty(navigator, 'clipboard', {
      configurable: true,
      value: { writeText: vi.fn().mockResolvedValue(undefined) },
    })
  })

  it('caps the role select to the caller’s role and never offers owner', () => {
    roleMock.role = 'owner'
    render(<InviteModal create={vi.fn()} onClose={vi.fn()} />)

    const select = screen.getByLabelText(/role/i) as HTMLSelectElement
    const options = within(select)
      .getAllByRole('option')
      .map((o) => o.getAttribute('value'))

    // Owner is the founder sentinel — even an owner inviter only sees
    // admin + member.
    expect(options).toEqual(['member', 'admin'])
  })

  it('posts the invite and surfaces the one-time link with a Copy button', async () => {
    const acceptUrl = 'https://tf.example.com/invite/accept?token=abc123'
    const create = vi.fn().mockResolvedValue({
      id: 'inv-1',
      accept_url: acceptUrl,
      expires_at: '2026-07-01T00:00:00Z',
    })
    render(<InviteModal create={create} onClose={vi.fn()} />)

    fireEvent.change(screen.getByLabelText(/email/i), { target: { value: 'new@example.com' } })
    fireEvent.click(screen.getByRole('button', { name: /send invite/i }))

    // Default role is member, default team is "No team" ('').
    await waitFor(() => expect(create).toHaveBeenCalledWith('new@example.com', 'member', ''))

    // The link is revealed in a read-only field; Copy writes it to the clipboard.
    const linkField = (await screen.findByLabelText(/invite link/i)) as HTMLInputElement
    expect(linkField.value).toBe(acceptUrl)
    fireEvent.click(screen.getByRole('button', { name: /copy/i }))
    expect(navigator.clipboard.writeText).toHaveBeenCalledWith(acceptUrl)
  })

  it('passes the chosen role and team through to create', async () => {
    const create = vi.fn().mockResolvedValue({
      id: 'inv-2',
      accept_url: 'https://tf.example.com/invite/accept?token=xyz',
      expires_at: '2026-07-01T00:00:00Z',
    })
    render(<InviteModal create={create} onClose={vi.fn()} />)

    fireEvent.change(screen.getByLabelText(/email/i), { target: { value: 'lead@example.com' } })
    fireEvent.change(screen.getByLabelText(/role/i), { target: { value: 'admin' } })
    fireEvent.change(screen.getByLabelText(/team/i), { target: { value: 'team-1' } })
    fireEvent.click(screen.getByRole('button', { name: /send invite/i }))

    await waitFor(() => expect(create).toHaveBeenCalledWith('lead@example.com', 'admin', 'team-1'))
  })

  it('surfaces the 409 already-pending conflict inline', async () => {
    const create = vi
      .fn()
      .mockRejectedValue(
        new HttpError(
          409,
          JSON.stringify({
            errors: [
              { reason: 'CONFLICT', message: 'an invite for that email is already pending' },
            ],
          }),
        ),
      )
    render(<InviteModal create={create} onClose={vi.fn()} />)

    fireEvent.change(screen.getByLabelText(/email/i), { target: { value: 'dupe@example.com' } })
    fireEvent.click(screen.getByRole('button', { name: /send invite/i }))

    // Surfaced inline AND announced to assistive tech (role="alert").
    const alert = await screen.findByRole('alert')
    expect(alert).toHaveTextContent(/already pending/i)
  })

  it('traps Tab focus within the dialog (WCAG 2.1.2)', () => {
    render(<InviteModal create={vi.fn()} onClose={vi.fn()} />)
    const dialog = screen.getByRole('dialog')

    // Focus lands inside on open (the email field's autoFocus).
    expect(dialog.contains(document.activeElement)).toBe(true)

    const focusables = Array.from(
      dialog.querySelectorAll<HTMLElement>(
        'a[href],button:not([disabled]),input:not([disabled]),select:not([disabled]),textarea:not([disabled]),[tabindex]:not([tabindex="-1"])',
      ),
    )
    const first = focusables[0]
    const last = focusables[focusables.length - 1]
    expect(first).not.toBe(last)

    // Tab off the last focusable wraps to the first…
    last.focus()
    fireEvent.keyDown(dialog, { key: 'Tab' })
    expect(document.activeElement).toBe(first)

    // …and Shift+Tab off the first wraps back to the last.
    first.focus()
    fireEvent.keyDown(dialog, { key: 'Tab', shiftKey: true })
    expect(document.activeElement).toBe(last)
  })
})
