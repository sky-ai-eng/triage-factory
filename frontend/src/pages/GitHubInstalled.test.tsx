// The recovery page for an installation of the deployment GitHub App that no
// workspace has bound. What's pinned here is the ticket's contract: the page
// describes an ordinary outcome (never a failure), offers the ORDINARY Connect
// ceremony and nothing else — no adopt path, no list to pick from — and renders
// nothing about the installation that sent the visitor here, because on a
// shared App that is somebody else's GitHub account.
import { describe, it, expect, vi, beforeEach } from 'vitest'
import { render, screen, fireEvent, waitFor } from '@testing-library/react'
import { MemoryRouter } from 'react-router'
import GitHubInstalled from './GitHubInstalled'
import type { AuthOrg } from '../types'

const authState = vi.hoisted(() => ({
  orgs: [] as { id: string; name: string; role: string }[],
  activeOrgId: null as string | null,
}))
vi.mock('../contexts/AuthContext', () => ({
  useAuth: () => ({ status: 'authed', orgs: authState.orgs as AuthOrg[] }),
}))
vi.mock('../contexts/OrgContext', () => ({
  useActiveOrgId: () => authState.activeOrgId,
}))

const connect = vi.hoisted(() => ({
  startManagedGitHubConnectAccount: vi.fn(),
}))
vi.mock('../lib/githubApp', async (importOriginal) => {
  const actual = await importOriginal<typeof import('../lib/githubApp')>()
  return {
    ...actual,
    startManagedGitHubConnectAccount: connect.startManagedGitHubConnectAccount,
  }
})

function renderAt(search = '') {
  return render(
    <MemoryRouter initialEntries={[`/github/installed${search}`]}>
      <GitHubInstalled />
    </MemoryRouter>,
  )
}

// The words the copy is forbidden to use for this state: it is the expected
// outcome of a supported install path, and the page must not imply otherwise.
const FAULT_WORDS = /\b(error|fail|failed|failure|wrong|problem)\b/i

beforeEach(() => {
  connect.startManagedGitHubConnectAccount.mockReset()
  connect.startManagedGitHubConnectAccount.mockResolvedValue(undefined)
  authState.orgs = [{ id: 'org-1', name: 'Acme', role: 'admin' }]
  authState.activeOrgId = 'org-1'
})

describe('GitHubInstalled', () => {
  it('describes the installed-but-unbound state without calling it a failure', () => {
    renderAt()
    expect(screen.getByRole('heading', { name: 'GitHub App installed' })).toBeInTheDocument()
    expect(screen.getByText(/isn.t connected to a workspace yet/i)).toBeInTheDocument()
    expect(document.body.textContent).not.toMatch(FAULT_WORDS)
  })

  it('connects the named account, and lists nothing to pick from', async () => {
    renderAt()
    // One button and no list: the connect form. Nothing enumerates
    // installations, and nothing needs to know whether the account has
    // the App — the ceremony works that out.
    expect(screen.getAllByRole('button')).toHaveLength(1)
    expect(screen.queryByRole('list')).not.toBeInTheDocument()
    expect(screen.queryByRole('combobox')).not.toBeInTheDocument()

    fireEvent.click(screen.getByRole('button', { name: 'Connect GitHub to Acme' }))
    fireEvent.change(screen.getByRole('textbox'), { target: { value: ' acme-corp ' } })
    fireEvent.click(screen.getByRole('button', { name: 'Connect' }))
    await waitFor(() => {
      expect(connect.startManagedGitHubConnectAccount).toHaveBeenCalledWith('org-1', 'acme-corp')
    })
  })

  it('surfaces a refused start inline and stays put', async () => {
    connect.startManagedGitHubConnectAccount.mockRejectedValue(
      new Error('This workspace already has a GitHub personal access token.'),
    )
    renderAt()
    fireEvent.click(screen.getByRole('button', { name: 'Connect GitHub to Acme' }))
    fireEvent.change(screen.getByRole('textbox'), { target: { value: 'acme-corp' } })
    fireEvent.click(screen.getByRole('button', { name: 'Connect' }))
    expect(
      await screen.findByText('This workspace already has a GitHub personal access token.'),
    ).toBeInTheDocument()
    expect(screen.getByRole('textbox')).toBeInTheDocument()
  })

  it('links back to the workspace settings surface that carries the same button', () => {
    renderAt()
    expect(screen.getByRole('link', { name: /workspace settings/i })).toHaveAttribute(
      'href',
      '/orgs/org-1/settings#github-app',
    )
  })

  // The callback forwards nothing about the installation — not its id, not the
  // account it targets — and the page has no read that could fetch either. So
  // whatever GitHub account installed the App, it is named nowhere here.
  it('renders no installation id and no account login', () => {
    renderAt('?installation_id=4242&account=someone-elses-org')
    const text = document.body.textContent ?? ''
    expect(text).not.toContain('4242')
    expect(text).not.toContain('someone-elses-org')
  })

  it('tells a non-admin to find an admin instead of offering a button that would 403', () => {
    authState.orgs = [{ id: 'org-1', name: 'Acme', role: 'member' }]
    renderAt()
    expect(screen.queryByRole('button')).not.toBeInTheDocument()
    expect(screen.getByText(/takes a workspace admin/i)).toBeInTheDocument()
    expect(document.body.textContent).not.toMatch(FAULT_WORDS)
  })

  it('lets a member of several workspaces choose which one to connect from', () => {
    authState.orgs = [
      { id: 'org-1', name: 'Acme', role: 'admin' },
      { id: 'org-2', name: 'Globex', role: 'admin' },
    ]
    renderAt()
    expect(screen.getByRole('button', { name: 'Connect GitHub to Acme' })).toBeInTheDocument()
    fireEvent.change(screen.getByRole('combobox'), { target: { value: 'org-2' } })
    fireEvent.click(screen.getByRole('button', { name: 'Connect GitHub to Globex' }))
    fireEvent.change(screen.getByRole('textbox'), { target: { value: 'globex' } })
    fireEvent.click(screen.getByRole('button', { name: 'Connect' }))
    expect(connect.startManagedGitHubConnectAccount).toHaveBeenCalledWith('org-2', 'globex')
  })

  // GitHub parked the install with an owner: nothing exists to connect yet,
  // so the page says "requested" and does not offer a Connect that would find
  // nothing.
  it('renders the requested outcome with no Connect button', () => {
    renderAt('?outcome=requested')
    expect(screen.getByRole('heading', { name: 'Install request sent' })).toBeInTheDocument()
    expect(screen.getByText(/once they approve it/i)).toBeInTheDocument()
    expect(screen.queryByRole('button')).not.toBeInTheDocument()
    expect(screen.getByRole('link', { name: /workspace settings/i })).toBeInTheDocument()
    expect(document.body.textContent).not.toMatch(FAULT_WORDS)
  })
})
