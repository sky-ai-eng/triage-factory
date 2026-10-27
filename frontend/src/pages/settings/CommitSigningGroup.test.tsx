// The Commit signing section in each of its states, and the setup flow driven
// against a fake of the signer routes.
import { beforeEach, describe, expect, it, vi } from 'vitest'
import { render, screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import CommitSigningGroup from './CommitSigningGroup'
import type { CommitIdentity, CommitSigner } from '../../lib/commitSigner'
import { jsonBody } from '../../test/apiResponse'

const ORG = 'org-1'
const GH = `/api/orgs/${ORG}/github`
const KEY = 'ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIcurrent'
const NEW_KEY = 'ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAInew'

const hoursAgo = (h: number) => new Date(Date.now() - h * 3600_000).toISOString()

function signer(over: Partial<CommitSigner> = {}): CommitSigner {
  return {
    host: 'https://github.com',
    github_user_id: '583231',
    login: 'octocat',
    name: 'The Octocat',
    email: null,
    commit_name: 'The Octocat',
    commit_email: '583231+octocat@users.noreply.github.com',
    public_key: KEY,
    pending_public_key: null,
    key_title: 'Triage Factory commit signing',
    status: 'pending',
    status_reason: null,
    status_since: hoursAgo(3),
    checked_at: null,
    ...over,
  }
}

const botIdentity: CommitIdentity = {
  identity: {
    name: 'acme[bot]',
    email: '7+acme[bot]@users.noreply.github.com',
    github_user_id: '7',
    login: null,
  },
  signing: 'none',
  unsigned_reason: null,
  git_version: null,
}

const signerIdentity = (signing: CommitIdentity['signing']): CommitIdentity => ({
  identity: {
    name: 'The Octocat',
    email: '583231+octocat@users.noreply.github.com',
    github_user_id: '583231',
    login: 'octocat',
  },
  signing,
  unsigned_reason: null,
  git_version: null,
})

type Reply = { status: number; body?: unknown }
type Handler = (body: unknown) => Reply

// fakeApi answers the signer routes from a mutable record of the org's state,
// with per-route overrides for refusals. Every call is kept for assertions.
function fakeApi(start: { signer: CommitSigner | null; identity: CommitIdentity }) {
  const state = { ...start }
  const overrides: Record<string, Handler> = {}
  const calls: Array<{ method: string; path: string; body: unknown }> = []
  const routes: Record<string, Handler> = {
    'GET /commit-signer': () => (state.signer ? { status: 200, body: state.signer } : notFound()),
    'GET /commit-identity': () => ({ status: 200, body: state.identity }),
    'POST /commit-signer': (body) => {
      const { login, email } = body as { login: string; email?: string }
      state.signer = signer({
        login,
        email: email ?? null,
        ...(email ? { commit_email: email } : {}),
      })
      state.identity = signerIdentity('none')
      return { status: 201, body: state.signer }
    },
    'POST /commit-signer/verify': () => {
      const s = state.signer!
      state.signer = {
        ...s,
        public_key: s.pending_public_key ?? s.public_key,
        pending_public_key: null,
        status: 'active',
        status_reason: null,
        status_since: new Date().toISOString(),
      }
      state.identity = signerIdentity('org')
      return { status: 200, body: state.signer }
    },
    'POST /commit-signer/rotate': () => {
      state.signer = { ...state.signer!, pending_public_key: NEW_KEY }
      return { status: 200, body: state.signer }
    },
    'PATCH /commit-signer': (body) => {
      const { email } = body as { email: string | null }
      const s = state.signer!
      state.signer = {
        ...s,
        email,
        commit_email: email ?? `${s.github_user_id}+${s.login}@users.noreply.github.com`,
      }
      return { status: 200, body: state.signer }
    },
    'DELETE /commit-signer': () => {
      state.signer = null
      state.identity = botIdentity
      return { status: 204 }
    },
  }
  const fetchMock = vi.fn(async (input: unknown, init?: RequestInit) => {
    const method = init?.method ?? 'GET'
    const path = String(input)
    const body = typeof init?.body === 'string' ? JSON.parse(init.body) : undefined
    calls.push({ method, path, body })
    const key = `${method} ${path.replace(GH, '')}`
    const handler = overrides[key] ?? routes[key]
    const reply = handler ? handler(body) : notFound()
    return { ok: reply.status < 300, status: reply.status, ...jsonBody(reply.body) }
  })
  vi.stubGlobal('fetch', fetchMock)
  return {
    state,
    calls,
    refuse(
      key: string,
      status: number,
      ...errors: Array<{ reason: string; message: string; field?: string }>
    ) {
      overrides[key] = () => ({ status, body: { errors } })
    },
    lift(key: string) {
      delete overrides[key]
    },
    writes: () => calls.filter((c) => c.method !== 'GET'),
  }
}

function notFound(): Reply {
  return {
    status: 404,
    body: { errors: [{ reason: 'NOT_FOUND', message: 'commit signer not found' }] },
  }
}

function renderGroup(opts: { githubUrl?: string; canManage?: boolean } = {}) {
  render(
    <CommitSigningGroup
      orgId={ORG}
      githubUrl={opts.githubUrl ?? 'https://github.com'}
      canManage={opts.canManage ?? true}
    />,
  )
}

beforeEach(() => {
  Object.defineProperty(navigator, 'clipboard', {
    configurable: true,
    value: { writeText: vi.fn().mockResolvedValue(undefined) },
  })
})

describe('CommitSigningGroup · no signer', () => {
  it('names who commits today and offers setup', async () => {
    fakeApi({ signer: null, identity: botIdentity })
    renderGroup()

    expect(await screen.findByText(/Commits are authored by/)).toHaveTextContent(
      'Commits are authored by acme[bot] and aren’t signed.',
    )
    expect(screen.getByRole('button', { name: 'Set up signing' })).toBeInTheDocument()
    expect(screen.getByRole('link', { name: /How to create the signer account/ })).toHaveAttribute(
      'href',
      expect.stringContaining('docs/users/workspace/commit-signing.md'),
    )
  })

  it('asks for the GitHub URL before setup when the org has none', async () => {
    fakeApi({ signer: null, identity: { ...botIdentity, identity: null } })
    renderGroup({ githubUrl: '' })

    expect(await screen.findByText('Commits aren’t signed.')).toBeInTheDocument()
    expect(screen.queryByRole('button', { name: 'Set up signing' })).toBeNull()
    expect(screen.getByText(/connect GitHub above before setting up signing/)).toBeInTheDocument()
  })

  it('takes a login on github.com, then shows the key, the steps and Verify', async () => {
    const api = fakeApi({ signer: null, identity: botIdentity })
    renderGroup()

    await userEvent.click(await screen.findByRole('button', { name: 'Set up signing' }))
    // github.com derives the noreply address, so there is no email to type.
    expect(screen.queryByLabelText('Commit email')).toBeNull()
    await userEvent.type(screen.getByLabelText('GitHub login'), 'octocat')
    await userEvent.click(screen.getByRole('button', { name: 'Continue' }))

    expect(await screen.findByLabelText('Public key')).toHaveValue(KEY)
    expect(api.writes()).toEqual([
      { method: 'POST', path: `${GH}/commit-signer`, body: { login: 'octocat' } },
    ])
    expect(screen.getByRole('link', { name: /SSH and GPG keys → New SSH key/ })).toHaveAttribute(
      'href',
      'https://github.com/settings/ssh/new',
    )
    expect(screen.getByText('Triage Factory commit signing')).toBeInTheDocument()
    expect(screen.getByText('Signing key')).toBeInTheDocument()
    expect(screen.getByRole('button', { name: 'Verify' })).toBeInTheDocument()
    // The identity was re-read after the write: a pending signer authors,
    // unsigned.
    expect(screen.getByText(/aren’t signed until Verify finds the key/)).toBeInTheDocument()

    await userEvent.click(screen.getByRole('button', { name: 'Copy Public key' }))
    expect(navigator.clipboard.writeText).toHaveBeenCalledWith(KEY)
  })

  it('asks for an email off github.com and sends it', async () => {
    const api = fakeApi({ signer: null, identity: botIdentity })
    renderGroup({ githubUrl: 'https://ghes.example.com' })

    await userEvent.click(await screen.findByRole('button', { name: 'Set up signing' }))
    expect(screen.getByText(/ghes\.example\.com has no noreply address/)).toBeInTheDocument()
    await userEvent.type(screen.getByLabelText('GitHub login'), 'agent')
    // The login alone does not complete the form on this host.
    expect(screen.getByRole('button', { name: 'Continue' })).toBeDisabled()
    await userEvent.type(screen.getByLabelText('Commit email'), 'agent@example.com')
    await userEvent.click(screen.getByRole('button', { name: 'Continue' }))

    await screen.findByLabelText('Public key')
    expect(api.writes()[0].body).toEqual({ login: 'agent', email: 'agent@example.com' })
  })

  it('puts a refusal on the field it names, and anything else under the form', async () => {
    const api = fakeApi({ signer: null, identity: botIdentity })
    api.refuse('POST /commit-signer', 422, {
      reason: 'INVALID_FIELD',
      field: 'login',
      message: 'acme is a GitHub organization account; only a user account can hold a signing key',
    })
    renderGroup()

    await userEvent.click(await screen.findByRole('button', { name: 'Set up signing' }))
    await userEvent.type(screen.getByLabelText('GitHub login'), 'acme')
    await userEvent.click(screen.getByRole('button', { name: 'Continue' }))

    expect(
      await screen.findByText(/only a user account can hold a signing key/),
    ).toBeInTheDocument()
    expect(screen.getByLabelText('GitHub login')).toHaveAttribute('aria-invalid', 'true')
    // The message is the field's description, so a screen reader reads it
    // with the field.
    expect(screen.getByLabelText('GitHub login')).toHaveAccessibleDescription(
      /only a user account can hold a signing key/,
    )

    api.refuse('POST /commit-signer', 422, {
      reason: 'SSH_SIGNING_UNSUPPORTED',
      message: 'ghes.example.com does not serve SSH signing keys',
    })
    await userEvent.click(screen.getByRole('button', { name: 'Continue' }))
    expect(await screen.findByText(/does not serve SSH signing keys/)).toBeInTheDocument()
    expect(screen.getByLabelText('GitHub login')).toHaveAttribute('aria-invalid', 'false')
    expect(screen.getByLabelText('GitHub login')).toHaveValue('acme')
  })

  it('lets setup be cancelled', async () => {
    fakeApi({ signer: null, identity: botIdentity })
    renderGroup()

    await userEvent.click(await screen.findByRole('button', { name: 'Set up signing' }))
    await userEvent.click(screen.getByRole('button', { name: 'Cancel' }))
    expect(screen.getByRole('button', { name: 'Set up signing' })).toBeInTheDocument()
  })
})

describe('CommitSigningGroup · deployment signer', () => {
  const deployment: CommitIdentity = {
    identity: {
      name: 'Triage Factory',
      email: '9+TriageFactory@users.noreply.github.com',
      github_user_id: '9',
      login: 'TriageFactory',
    },
    signing: 'deployment',
    unsigned_reason: null,
    git_version: null,
  }

  it('says the deployment signs, and offers the org its own account', async () => {
    fakeApi({ signer: null, identity: deployment })
    renderGroup()

    expect(await screen.findByText(/Commits are signed as/)).toHaveTextContent(
      'Commits are signed as @TriageFactory, this deployment’s signing account.',
    )
    await userEvent.click(screen.getByRole('button', { name: 'Use your own account' }))
    expect(screen.getByLabelText('GitHub login')).toBeInTheDocument()
  })
})

describe('CommitSigningGroup · signer states', () => {
  it('shows a pending signer with how long it has been pending', async () => {
    fakeApi({ signer: signer({ status_since: hoursAgo(3) }), identity: signerIdentity('none') })
    renderGroup()

    expect(await screen.findByText('Pending')).toBeInTheDocument()
    expect(screen.getByText(/for 3h/)).toBeInTheDocument()
    expect(screen.getByLabelText('Public key')).toHaveValue(KEY)
  })

  it('shows an active signer, its key and its controls', async () => {
    fakeApi({
      signer: signer({ status: 'active', status_since: hoursAgo(72) }),
      identity: signerIdentity('org'),
    })
    renderGroup()

    expect(await screen.findByText('Active')).toBeInTheDocument()
    expect(screen.getByText(/for 3d/)).toBeInTheDocument()
    expect(screen.getByText('Agents’ commits are signed as @octocat.')).toBeInTheDocument()
    expect(screen.getByLabelText('Public key')).toHaveValue(KEY)
    // An active signer needs no setup steps.
    expect(screen.queryByRole('link', { name: /New SSH key/ })).toBeNull()
    for (const name of ['Verify', 'Rotate key', 'Remove', 'Change']) {
      expect(screen.getByRole('button', { name })).toBeInTheDocument()
    }
  })

  it('says why an active signer still commits unsigned on this machine', async () => {
    fakeApi({
      signer: signer({ status: 'active' }),
      identity: {
        ...signerIdentity('none'),
        unsigned_reason: 'git_too_old',
        git_version: '2.33.0',
      },
    })
    renderGroup()

    expect(await screen.findByText(/older than 2\.34/)).toHaveTextContent(
      'This machine’s git, 2.33.0, is older than 2.34',
    )
  })

  it.each([
    ['unknown_key', /the key a pushed commit was signed with is not on @octocat/],
    [
      'no_user',
      /583231\+octocat@users\.noreply\.github\.com, the address commits carry, belongs to no account/,
    ],
    ['unverified_email', /is not verified on @octocat\. Verify the address on the account/],
    ['key_missing', /The signing key is no longer on @octocat/],
    ['bad_email', /GitHub reported a pushed commit as not verified: bad_email\./],
  ])('puts broken reason %s in words', async (reason, words) => {
    fakeApi({
      signer: signer({ status: 'broken', status_reason: reason, status_since: hoursAgo(2) }),
      identity: signerIdentity('none'),
    })
    renderGroup()

    const alert = await screen.findByRole('alert')
    expect(alert).toHaveTextContent(words)
    expect(alert).toHaveTextContent('Commits are still authored as The Octocat')
    expect(screen.getByText('Broken')).toBeInTheDocument()
    expect(screen.getByText(/for 2h/)).toBeInTheDocument()
  })

  it('shows a member the state without a single control', async () => {
    fakeApi({ signer: signer({ status: 'pending' }), identity: signerIdentity('none') })
    renderGroup({ canManage: false })

    expect(await screen.findByText('Pending')).toBeInTheDocument()
    expect(screen.getByLabelText('Public key')).toHaveValue(KEY)
    for (const name of ['Verify', 'Rotate key', 'Remove', 'Change']) {
      expect(screen.queryByRole('button', { name })).toBeNull()
    }
  })

  it('shows a member no setup button either', async () => {
    fakeApi({ signer: null, identity: botIdentity })
    renderGroup({ canManage: false })

    await screen.findByText(/Commits are authored by/)
    expect(screen.queryByRole('button', { name: 'Set up signing' })).toBeNull()
  })

  it('offers a retry when the section cannot load', async () => {
    const api = fakeApi({ signer: null, identity: botIdentity })
    api.refuse('GET /commit-identity', 500, {
      reason: 'INTERNAL',
      message: 'Could not read the identity.',
    })
    renderGroup()

    expect(await screen.findByText('Could not read the identity.')).toBeInTheDocument()
    api.lift('GET /commit-identity')
    await userEvent.click(screen.getByRole('button', { name: 'Try again' }))
    expect(await screen.findByText(/Commits are authored by/)).toBeInTheDocument()
  })
})

describe('CommitSigningGroup · managed users', () => {
  it.each([
    ['a GHE.com account', 'https://acme.ghe.com', 'agent', true],
    ['an Enterprise Managed User on github.com', 'https://github.com', 'agent_acme', true],
    ['a github.com account', 'https://github.com', 'agent', false],
    ['a GHES account', 'https://ghes.example.com', 'agent_x', false],
  ])('%s: the steps say to verify the email first: %s', async (_name, host, login, managed) => {
    fakeApi({ signer: signer({ host, login }), identity: signerIdentity('none') })
    renderGroup({ githubUrl: host })

    await screen.findByText('Pending')
    const step = screen.queryByText(/Verify the account’s email on GitHub first/)
    if (managed) expect(step).toBeInTheDocument()
    else expect(step).toBeNull()
  })
})

describe('CommitSigningGroup · Verify', () => {
  it('says the listing can lag when the key is not found, and stays pending', async () => {
    const api = fakeApi({ signer: signer(), identity: signerIdentity('none') })
    api.refuse('POST /commit-signer/verify', 409, {
      reason: 'KEY_NOT_FOUND',
      message:
        "the signer's key is not among octocat's signing keys on GitHub; add it in Settings → SSH and GPG keys → New SSH key with key type Signing key, then verify again. GitHub's listing can take a minute to show a key just added",
    })
    renderGroup()

    await userEvent.click(await screen.findByRole('button', { name: 'Verify' }))
    expect(
      await screen.findByText(/can take a minute to show a key just added/),
    ).toBeInTheDocument()
    expect(screen.getByText('Pending')).toBeInTheDocument()
    expect(screen.getByRole('button', { name: 'Verify' })).toBeEnabled()
  })

  it('makes the signer active once the key is found', async () => {
    fakeApi({ signer: signer(), identity: signerIdentity('none') })
    renderGroup()

    await userEvent.click(await screen.findByRole('button', { name: 'Verify' }))
    expect(await screen.findByText('Active')).toBeInTheDocument()
    expect(screen.getByText(/as of just now/)).toBeInTheDocument()
    expect(screen.getByText('Agents’ commits are signed as @octocat.')).toBeInTheDocument()
  })

  it("shows GitHub's answer when the failing commit still fails", async () => {
    const api = fakeApi({
      signer: signer({ status: 'broken', status_reason: 'unverified_email' }),
      identity: signerIdentity('none'),
    })
    api.refuse('POST /commit-signer/verify', 409, {
      reason: 'COMMIT_UNVERIFIED',
      message: 'GitHub still reports commit abc123 in acme/api as not verified (unverified_email)',
    })
    renderGroup()

    await userEvent.click(await screen.findByRole('button', { name: 'Verify' }))
    expect(await screen.findByText(/still reports commit abc123/)).toBeInTheDocument()
    expect(screen.getByText('Broken')).toBeInTheDocument()
  })
})

describe('CommitSigningGroup · rotation', () => {
  it('shows the new key until Verify promotes it, then says to leave the old one', async () => {
    const api = fakeApi({ signer: signer({ status: 'active' }), identity: signerIdentity('org') })
    renderGroup()

    await userEvent.click(await screen.findByRole('button', { name: 'Rotate key' }))
    expect(await screen.findByLabelText('New public key')).toHaveValue(NEW_KEY)
    expect(
      screen.getByText(/The current key keeps signing until Verify finds the new key/),
    ).toBeInTheDocument()
    expect(screen.queryByText(/Leave the old public key/)).toBeNull()

    await userEvent.click(screen.getByRole('button', { name: 'Verify' }))
    expect(
      await screen.findByText(/Leave the old public key on @octocat unless it may have leaked/),
    ).toHaveTextContent(/deleting it turns the commits it signed Unverified/)
    expect(screen.getByLabelText('Public key')).toHaveValue(NEW_KEY)
    expect(api.writes().map((w) => `${w.method} ${w.path.replace(GH, '')}`)).toEqual([
      'POST /commit-signer/rotate',
      'POST /commit-signer/verify',
    ])
  })
})

describe('CommitSigningGroup · commit email', () => {
  it('says to verify the address on the account first, and sends it', async () => {
    const api = fakeApi({ signer: signer({ status: 'active' }), identity: signerIdentity('org') })
    renderGroup()

    expect(await screen.findByText(/Commit email:/)).toHaveTextContent(
      '583231+octocat@users.noreply.github.com (GitHub’s noreply address)',
    )
    await userEvent.click(screen.getByRole('button', { name: 'Change' }))
    expect(
      screen.getByText(/Add the address to @octocat on GitHub and verify it there first/),
    ).toBeInTheDocument()
    // The derived address is already in use, so there is nothing to restore.
    expect(screen.queryByRole('button', { name: 'Use the noreply address' })).toBeNull()

    await userEvent.type(screen.getByLabelText('Commit email'), 'agent@acme.com')
    await userEvent.click(screen.getByRole('button', { name: 'Save' }))

    expect(await screen.findByText(/Commit email:/)).toHaveTextContent('agent@acme.com')
    expect(api.writes()).toEqual([
      { method: 'PATCH', path: `${GH}/commit-signer`, body: { email: 'agent@acme.com' } },
    ])
  })

  it('restores the noreply address on github.com', async () => {
    const api = fakeApi({
      signer: signer({ status: 'active', email: 'agent@acme.com', commit_email: 'agent@acme.com' }),
      identity: signerIdentity('org'),
    })
    renderGroup()

    await userEvent.click(await screen.findByRole('button', { name: 'Change' }))
    await userEvent.click(screen.getByRole('button', { name: 'Use the noreply address' }))

    expect(await screen.findByText(/Commit email:/)).toHaveTextContent('noreply')
    expect(api.writes()[0].body).toEqual({ email: null })
  })

  it('offers no noreply address off github.com', async () => {
    fakeApi({
      signer: signer({
        host: 'https://ghes.example.com',
        status: 'active',
        email: 'agent@acme.com',
        commit_email: 'agent@acme.com',
      }),
      identity: signerIdentity('org'),
    })
    renderGroup({ githubUrl: 'https://ghes.example.com' })

    await userEvent.click(await screen.findByRole('button', { name: 'Change' }))
    expect(screen.getByLabelText('Commit email')).toHaveValue('agent@acme.com')
    expect(screen.queryByRole('button', { name: 'Use the noreply address' })).toBeNull()
  })

  it('shows a refused address and keeps the form open', async () => {
    const api = fakeApi({ signer: signer({ status: 'active' }), identity: signerIdentity('org') })
    api.refuse('PATCH /commit-signer', 400, {
      reason: 'INVALID_FIELD',
      field: 'email',
      message: 'email must be a bare email address, such as agent@example.com',
    })
    renderGroup()

    await userEvent.click(await screen.findByRole('button', { name: 'Change' }))
    await userEvent.type(screen.getByLabelText('Commit email'), 'Agent <agent@acme.com>')
    await userEvent.click(screen.getByRole('button', { name: 'Save' }))

    expect(await screen.findByText(/must be a bare email address/)).toBeInTheDocument()
    expect(screen.getByLabelText('Commit email')).toHaveAccessibleDescription(
      /must be a bare email address/,
    )
  })
})

describe('CommitSigningGroup · Remove', () => {
  it('confirms, deletes, and shows who commits without the signer', async () => {
    const api = fakeApi({ signer: signer({ status: 'active' }), identity: signerIdentity('org') })
    renderGroup()

    await userEvent.click(await screen.findByRole('button', { name: 'Remove' }))
    const confirm = screen.getByText(/Remove @octocat as the signer\?/).parentElement!
    expect(confirm).toHaveTextContent(
      'The public key stays on the account until you delete it there.',
    )
    await userEvent.click(within(confirm).getByRole('button', { name: 'Remove signer' }))

    expect(await screen.findByText(/Commits are authored by/)).toHaveTextContent('acme[bot]')
    expect(api.writes()).toEqual([
      { method: 'DELETE', path: `${GH}/commit-signer`, body: undefined },
    ])
  })

  it('keeps the signer when the removal is called off', async () => {
    const api = fakeApi({ signer: signer({ status: 'active' }), identity: signerIdentity('org') })
    renderGroup()

    await userEvent.click(await screen.findByRole('button', { name: 'Remove' }))
    await userEvent.click(screen.getByRole('button', { name: 'Keep it' }))
    expect(screen.getByRole('button', { name: 'Remove' })).toBeInTheDocument()
    await waitFor(() => expect(api.writes()).toEqual([]))
  })
})
