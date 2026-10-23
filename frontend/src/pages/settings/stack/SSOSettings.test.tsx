import { describe, it, expect, vi, beforeEach } from 'vitest'
import { render, screen, fireEvent, waitFor } from '@testing-library/react'
import { HttpError } from '../../../lib/apiClient'
import type {
  BreakGlassPrincipal,
  SSOConnection,
  SSOConnectionResponse,
  SSODomain,
} from '../../../types'

// Mock the apiClient I/O but keep HttpError + httpErrorMessage real — the
// verify path's 409 handling runs through httpErrorMessage to surface the
// server's friendly message.
const apiMocks = vi.hoisted(() => ({ apiJSON: vi.fn(), apiFetch: vi.fn() }))
vi.mock('../../../lib/apiClient', async (importOriginal) => {
  const actual = await importOriginal<typeof import('../../../lib/apiClient')>()
  return { ...actual, apiJSON: apiMocks.apiJSON, apiFetch: apiMocks.apiFetch }
})

const toasts = vi.hoisted(() => ({
  toast: { success: vi.fn(), error: vi.fn(), info: vi.fn(), warning: vi.fn() },
}))
vi.mock('../../../components/Toast/toastStore', () => toasts)

import SSOSettings from './SSOSettings'

const SP = {
  entity_id: 'https://tf.example.com/auth/v1/sso/saml/metadata',
  acs_url: 'https://tf.example.com/auth/v1/sso/saml/acs',
}

function makeConnection(over: Partial<SSOConnection> = {}): SSOConnection {
  return {
    id: 'conn-1',
    kind: 'saml',
    provider_id: 'prov-uuid-123',
    default_role: 'member',
    enabled: true,
    enforced: false,
    tested: false,
    created_at: '2026-06-01T00:00:00Z',
    updated_at: '2026-06-01T00:00:00Z',
    ...over,
  }
}

function makeDomain(over: Partial<SSODomain> = {}): SSODomain {
  return {
    id: 'dom-1',
    domain: 'example.com',
    txt_host: '_triagefactory-verification.example.com',
    txt_value: 'tok-abc123',
    status: 'pending',
    ...over,
  }
}

// Mutable server state the mock reads/writes so a POST/PATCH/DELETE is reflected
// by the next GET — the same stateful-mock shape OrgPage.test uses.
let connState: SSOConnectionResponse
let domainList: SSODomain[]
let breakGlass: BreakGlassPrincipal[]
// When set, the verify endpoint throws it instead of stamping verified — drives
// the two 409 states (DNS-not-found-yet / already-claimed).
let verifyError: HttpError | null
// Refusals keyed "METHOD path": the call throws the error instead of applying,
// the way the server answers a guard's 409.
let refusals: Record<string, HttpError>

function conflict(message: string): HttpError {
  return new HttpError(409, JSON.stringify({ errors: [{ reason: 'CONFLICT', message }] }))
}

// seedConnected primes the load with an existing connection (+ optional domains)
// so a test starts past the register step. entityID is overridable to exercise
// Sign-on URL derivation (e.g. a sub-pathed deployment public URL).
function seedConnected(
  domains: SSODomain[] = [],
  entityID: string = SP.entity_id,
  conn: Partial<SSOConnection> = {},
) {
  connState = { connection: makeConnection(conn), entity_id: entityID, acs_url: SP.acs_url }
  domainList = domains
}

function installApi() {
  apiMocks.apiJSON.mockImplementation(
    async (path: string, opts?: { method?: string; body?: string }) => {
      const method = opts?.method ?? 'GET'
      if (refusals[`${method} ${path}`]) throw refusals[`${method} ${path}`]

      if (path === '/api/sso/connection') {
        if (method === 'GET') return connState
        if (method === 'POST') {
          // Register → a freshly created connection is enabled, its IdP derived
          // from the metadata URL's host (every test registers an Entra URL).
          connState = { ...connState, connection: makeConnection({ idp: 'entra' }) }
          return connState
        }
        if (method === 'PATCH') {
          // Field-by-field, like the server: whatever the body names is written.
          const patch = JSON.parse(opts!.body!) as Partial<SSOConnection>
          connState = {
            ...connState,
            connection: connState.connection ? { ...connState.connection, ...patch } : null,
          }
          return connState
        }
      }

      if (path === '/api/sso/connection/enforcement' && method === 'PATCH') {
        const { enforced } = JSON.parse(opts!.body!) as { enforced: boolean }
        connState = {
          ...connState,
          connection: connState.connection ? { ...connState.connection, enforced } : null,
        }
        return connState
      }

      // The break-glass section loads on mount whenever a connection exists.
      if (path === '/api/sso/break-glass') {
        if (method === 'GET') return breakGlass
        if (method === 'POST') {
          const { email } = JSON.parse(opts!.body!) as { email: string }
          breakGlass = [
            ...breakGlass,
            { user_id: `u-${breakGlass.length + 1}`, email, created_at: '' },
          ]
          return breakGlass
        }
      }

      if (path === '/api/sso/domains') {
        if (method === 'GET') return domainList
        if (method === 'POST') {
          const { domain } = JSON.parse(opts!.body!) as { domain: string }
          const created = makeDomain({ id: `dom-${domainList.length + 1}`, domain })
          domainList = [...domainList, created]
          return created
        }
      }

      const verifyMatch = path.match(/^\/api\/sso\/domains\/(.+)\/verify$/)
      if (verifyMatch && method === 'POST') {
        if (verifyError) throw verifyError
        const id = verifyMatch[1]
        const verified: SSODomain = {
          ...domainList.find((d) => d.id === id)!,
          status: 'verified',
          verified_at: '2026-06-02T00:00:00Z',
        }
        domainList = domainList.map((d) => (d.id === id ? verified : d))
        return verified
      }

      throw new Error(`unexpected apiJSON ${method} ${path}`)
    },
  )

  apiMocks.apiFetch.mockImplementation(async (path: string, opts?: { method?: string }) => {
    if (refusals[`${opts?.method ?? 'GET'} ${path}`])
      throw refusals[`${opts?.method ?? 'GET'} ${path}`]
    const delMatch = path.match(/^\/api\/sso\/domains\/(.+)$/)
    if (delMatch && opts?.method === 'DELETE') {
      domainList = domainList.filter((d) => d.id !== delMatch[1])
      return undefined as unknown as Response
    }
    const bgDelMatch = path.match(/^\/api\/sso\/break-glass\/(.+)$/)
    if (bgDelMatch && opts?.method === 'DELETE') {
      breakGlass = breakGlass.filter((p) => p.user_id !== bgDelMatch[1])
      return undefined as unknown as Response
    }
    throw new Error(`unexpected apiFetch ${opts?.method} ${path}`)
  })
}

beforeEach(() => {
  apiMocks.apiJSON.mockReset()
  apiMocks.apiFetch.mockReset()
  // Default: no connection yet (the cold-start register flow).
  connState = { connection: null, entity_id: SP.entity_id, acs_url: SP.acs_url }
  domainList = []
  breakGlass = []
  verifyError = null
  refusals = {}
  toasts.toast.error.mockReset()
  toasts.toast.success.mockReset()
  window.confirm = vi.fn(() => true)
  // jsdom's window.open is a noisy no-op; stub it so the Test SSO popup launch is
  // assertable and doesn't log "Not implemented".
  window.open = vi.fn()
  Object.defineProperty(navigator, 'clipboard', {
    configurable: true,
    value: { writeText: vi.fn().mockResolvedValue(undefined) },
  })
  installApi()
})

describe('SSOSettings — SP details + connection', () => {
  it('renders the SP Entity ID and ACS URL with copy buttons', async () => {
    render(<SSOSettings />)

    // No connection yet, so no IdP to name: the protocol's own labels.
    const entity = (await screen.findByLabelText('Entity ID')) as HTMLInputElement
    expect(entity.value).toBe(SP.entity_id)
    const acs = screen.getByLabelText('ACS URL') as HTMLInputElement
    expect(acs.value).toBe(SP.acs_url)

    // Copy the Entity ID writes it to the clipboard.
    fireEvent.click(screen.getByRole('button', { name: /copy entity id/i }))
    expect(navigator.clipboard.writeText).toHaveBeenCalledWith(SP.entity_id)

    // The Sign-on URL needs a provider_id, so it's absent until connected.
    expect(screen.queryByLabelText('Sign-on URL')).not.toBeInTheDocument()
  })

  it('derives the Sign-on URL from the Entity ID once connected', async () => {
    seedConnected()
    render(<SSOSettings />)

    const signOn = (await screen.findByLabelText('Sign-on URL')) as HTMLInputElement
    expect(signOn.value).toBe(
      'https://tf.example.com/api/auth/oauth/saml?provider_id=prov-uuid-123&return_to=%2F',
    )
  })

  it('preserves a sub-path in the public URL when deriving the Sign-on URL', async () => {
    // A deployment hosted under a sub-path (TF_PUBLIC_URL = https://example.com/tf)
    // carries it in entity_id; the Sign-on URL must keep it. Deriving from
    // URL().origin would drop /tf and break the My Apps tile — this guards that.
    seedConnected([], 'https://example.com/tf/auth/v1/sso/saml/metadata')
    render(<SSOSettings />)

    const signOn = (await screen.findByLabelText('Sign-on URL')) as HTMLInputElement
    expect(signOn.value).toBe(
      'https://example.com/tf/api/auth/oauth/saml?provider_id=prov-uuid-123&return_to=%2F',
    )
  })

  it('registers a connection and reflects the enabled state', async () => {
    render(<SSOSettings />)

    const input = (await screen.findByLabelText(/saml metadata url/i)) as HTMLInputElement
    fireEvent.change(input, {
      target: { value: 'https://login.microsoftonline.com/x/federationmetadata.xml' },
    })
    fireEvent.click(screen.getByRole('button', { name: /^connect$/i }))

    await waitFor(() =>
      expect(apiMocks.apiJSON).toHaveBeenCalledWith(
        '/api/sso/connection',
        expect.objectContaining({
          method: 'POST',
          body: expect.stringContaining('federationmetadata.xml'),
        }),
      ),
    )

    // The connected view reflects the enabled connection (toggle on).
    expect(await screen.findByText(/connected · enabled/i)).toBeInTheDocument()
    expect(screen.getByRole('switch', { name: /enable sso/i })).toBeChecked()
    // The domains surface opens once a connection exists.
    expect(screen.getByLabelText(/domain to claim/i)).toBeInTheDocument()
    // The derived IdP now names the card and keys the SP details' wording.
    expect(screen.getByRole('combobox', { name: 'Identity provider' })).toHaveValue('entra')
    expect(screen.getByLabelText('Identifier (Entity ID)')).toBeInTheDocument()
    expect(screen.getByLabelText('Reply URL (ACS)')).toBeInTheDocument()
  })

  it('opens the verify-before-enforce test in a popup, even when disabled', async () => {
    // The Test SSO button is offered on a not-yet-enabled connection — testing
    // before enabling is the whole point — and launches the round-trip in a new
    // window (a GET the server 303s through the IdP, untouched session).
    connState = {
      connection: makeConnection({ enabled: false }),
      entity_id: SP.entity_id,
      acs_url: SP.acs_url,
    }
    domainList = []
    render(<SSOSettings />)

    expect(await screen.findByText(/connected · disabled/i)).toBeInTheDocument()
    fireEvent.click(screen.getByRole('button', { name: /test sso/i }))

    // noopener/noreferrer must be present — the popup navigates to the external
    // IdP, so it must not retain a window.opener handle on this tab.
    expect(window.open).toHaveBeenCalledWith(
      '/api/sso/connection/test',
      'tf-sso-test',
      expect.stringMatching(/(?=.*popup)(?=.*noopener)(?=.*noreferrer)/),
    )
  })

  it('toggles the connection disabled via PATCH', async () => {
    seedConnected()
    render(<SSOSettings />)

    const toggle = await screen.findByRole('switch', { name: /enable sso/i })
    expect(toggle).toBeChecked()
    fireEvent.click(toggle)

    await waitFor(() =>
      expect(apiMocks.apiJSON).toHaveBeenCalledWith(
        '/api/sso/connection',
        expect.objectContaining({ method: 'PATCH', body: JSON.stringify({ enabled: false }) }),
      ),
    )
    expect(await screen.findByText(/connected · disabled/i)).toBeInTheDocument()
    expect(screen.getByRole('switch', { name: /enable sso/i })).not.toBeChecked()
  })
})

// The picker lists every provider, Entra included; the wording checks read
// the rest of the section.
const NOT_OPTIONS = { ignore: 'option, script, style' }

describe('SSOSettings — identity provider', () => {
  it('names the derived IdP on the card with its mark and words the SP details for it', async () => {
    seedConnected([], SP.entity_id, { idp: 'okta' })
    render(<SSOSettings />)

    const picker = await screen.findByRole('combobox', { name: 'Identity provider' })
    expect(picker).toHaveValue('okta')
    expect(document.querySelector('img[src="/idp/okta.svg"]')).not.toBeNull()
    // Okta's console names for the SP values, so a label matches its field.
    expect(screen.getByLabelText('Audience URI (SP Entity ID)')).toHaveValue(SP.entity_id)
    expect(screen.getByLabelText('Single sign-on URL (ACS)')).toHaveValue(SP.acs_url)
    expect(screen.getByText(/paste these into okta/i)).toBeInTheDocument()
    expect(screen.queryByText(/entra/i, NOT_OPTIONS)).not.toBeInTheDocument()
  })

  it('corrects the IdP via PATCH and rewords the section', async () => {
    seedConnected([], SP.entity_id, { idp: 'entra' })
    render(<SSOSettings />)

    const picker = await screen.findByRole('combobox', { name: 'Identity provider' })
    expect(screen.getByText(/in entra, also set the sign-on url/i)).toBeInTheDocument()
    fireEvent.change(picker, { target: { value: 'onelogin' } })

    await waitFor(() =>
      expect(apiMocks.apiJSON).toHaveBeenCalledWith(
        '/api/sso/connection',
        expect.objectContaining({ method: 'PATCH', body: JSON.stringify({ idp: 'onelogin' }) }),
      ),
    )
    expect(await screen.findByLabelText('Audience (EntityID)')).toHaveValue(SP.entity_id)
    expect(screen.getByRole('combobox', { name: 'Identity provider' })).toHaveValue('onelogin')
    expect(toasts.toast.success).toHaveBeenCalledWith('Identity provider set to OneLogin')
    // OneLogin has no vendor mark, and nothing on the section says Entra now.
    expect(document.querySelector('img[src^="/idp/"]')).toBeNull()
    expect(screen.queryByText(/entra/i, NOT_OPTIONS)).not.toBeInTheDocument()
  })

  it.each([
    ['other', 'other' as const, 'Other'],
    ['not known', undefined, 'Not known'],
  ])('keeps the wording provider-neutral for %s', async (_, idp, shown) => {
    seedConnected([], SP.entity_id, { idp })
    render(<SSOSettings />)

    const picker = (await screen.findByRole('combobox', {
      name: 'Identity provider',
    })) as HTMLSelectElement
    expect(picker.selectedOptions[0].textContent).toBe(shown)
    expect(screen.getByLabelText('Entity ID')).toHaveValue(SP.entity_id)
    expect(screen.getByLabelText('ACS URL')).toHaveValue(SP.acs_url)
    expect(screen.getByText(/paste these into your identity provider/i)).toBeInTheDocument()
    expect(screen.getByText(/if your identity provider’s app tile/i)).toBeInTheDocument()
    expect(document.querySelector('img[src^="/idp/"]')).toBeNull()
    expect(screen.queryByText(/entra/i, NOT_OPTIONS)).not.toBeInTheDocument()
  })

  it('leaves the stored IdP showing when the correction is refused', async () => {
    seedConnected([], SP.entity_id, { idp: 'google' })
    refusals['PATCH /api/sso/connection'] = new HttpError(
      422,
      JSON.stringify({
        errors: [{ reason: 'INVALID_FIELD', field: 'idp', message: 'idp must be one of …' }],
      }),
    )
    render(<SSOSettings />)

    const picker = await screen.findByRole('combobox', { name: 'Identity provider' })
    fireEvent.change(picker, { target: { value: 'ping' } })

    await waitFor(() => expect(toasts.toast.error).toHaveBeenCalledWith('idp must be one of …'))
    expect(picker).toHaveValue('google')
    expect(picker).not.toBeDisabled()
  })

  it('holds the enable switch while an IdP correction is in flight', async () => {
    // Both PATCHes answer with the whole connection, so one landing after the
    // other would overwrite its field with a stale value: one gate for both.
    seedConnected([], SP.entity_id, { idp: 'entra' })
    let release!: () => void
    const held = new Promise<void>((r) => (release = r))
    const base = apiMocks.apiJSON.getMockImplementation()!
    apiMocks.apiJSON.mockImplementation(async (path: string, opts?: { method?: string }) => {
      if (opts?.method === 'PATCH') await held
      return base(path, opts)
    })
    render(<SSOSettings />)

    const picker = await screen.findByRole('combobox', { name: 'Identity provider' })
    fireEvent.change(picker, { target: { value: 'okta' } })
    await waitFor(() => expect(screen.getByRole('switch', { name: /enable sso/i })).toBeDisabled())
    expect(picker).toBeDisabled()

    release()
    await waitFor(() =>
      expect(screen.getByRole('switch', { name: /enable sso/i })).not.toBeDisabled(),
    )
    expect(picker).toHaveValue('okta')
  })
})

describe('SSOSettings — domains', () => {
  it('claims a domain and shows the DNS TXT record with copy buttons', async () => {
    seedConnected()
    render(<SSOSettings />)

    const input = (await screen.findByLabelText(/domain to claim/i)) as HTMLInputElement
    fireEvent.change(input, { target: { value: 'example.com' } })
    fireEvent.click(screen.getByRole('button', { name: /add domain/i }))

    // The exact DNS record renders: host + TXT type + value.
    const host = (await screen.findByLabelText('Host / name')) as HTMLInputElement
    expect(host.value).toBe('_triagefactory-verification.example.com')
    expect(screen.getByText('TXT')).toBeInTheDocument()
    const value = screen.getByLabelText('Value') as HTMLInputElement
    expect(value.value).toBe('tok-abc123')

    // Copy the host writes the record name to the clipboard.
    fireEvent.click(screen.getByRole('button', { name: /copy host \/ name/i }))
    expect(navigator.clipboard.writeText).toHaveBeenCalledWith(
      '_triagefactory-verification.example.com',
    )
  })

  it('verifies a pending domain and flips the badge to Verified', async () => {
    seedConnected([makeDomain()])
    render(<SSOSettings />)

    expect(await screen.findByText('Pending')).toBeInTheDocument()
    fireEvent.click(screen.getByRole('button', { name: /^verify$/i }))

    expect(await screen.findByText('Verified')).toBeInTheDocument()
    expect(screen.queryByText('Pending')).not.toBeInTheDocument()
    // The TXT record collapses once verified (nothing left to publish).
    expect(screen.queryByLabelText('Host / name')).not.toBeInTheDocument()
  })

  it('surfaces the "DNS not found yet" 409 as a friendly inline message', async () => {
    seedConnected([makeDomain()])
    verifyError = new HttpError(
      409,
      JSON.stringify({
        errors: [
          {
            reason: 'CONFLICT',
            message: 'DNS record not found yet — propagation can take time, retry.',
          },
        ],
      }),
    )
    render(<SSOSettings />)

    fireEvent.click(await screen.findByRole('button', { name: /^verify$/i }))

    expect(await screen.findByText(/not found yet/i)).toBeInTheDocument()
    // Still pending — no false flip to Verified.
    expect(screen.getByText('Pending')).toBeInTheDocument()
  })

  it('surfaces the "already claimed by another organization" 409 as a friendly message', async () => {
    seedConnected([makeDomain()])
    verifyError = new HttpError(
      409,
      JSON.stringify({
        errors: [
          {
            reason: 'CONFLICT',
            message: 'this domain is already verified by another organization',
          },
        ],
      }),
    )
    render(<SSOSettings />)

    fireEvent.click(await screen.findByRole('button', { name: /^verify$/i }))

    expect(await screen.findByText(/already verified by another organization/i)).toBeInTheDocument()
  })

  it('removes a domain via DELETE', async () => {
    seedConnected([makeDomain()])
    render(<SSOSettings />)

    await screen.findByText('example.com')
    fireEvent.click(screen.getByRole('button', { name: /remove example\.com/i }))

    await waitFor(() =>
      expect(apiMocks.apiFetch).toHaveBeenCalledWith('/api/sso/domains/dom-1', {
        method: 'DELETE',
      }),
    )
    await waitFor(() => expect(screen.queryByText('example.com')).not.toBeInTheDocument())
  })
})

describe('SSOSettings — require SSO (enforcement)', () => {
  // Helper: connection that satisfies every gate (enabled + tested) plus a
  // verified domain, so the enforce toggle is available.
  function seedEnforceReady() {
    connState = {
      connection: makeConnection({ enabled: true, tested: true }),
      entity_id: SP.entity_id,
      acs_url: SP.acs_url,
    }
    domainList = [makeDomain({ status: 'verified', verified_at: '2026-06-02T00:00:00Z' })]
  }

  it('disables the toggle with an explanation until enabled + tested + a verified domain', async () => {
    // Enabled but not tested, no verified domain → toggle disabled, reason shown.
    connState = {
      connection: makeConnection({ enabled: true, tested: false }),
      entity_id: SP.entity_id,
      acs_url: SP.acs_url,
    }
    domainList = [makeDomain()] // pending only
    render(<SSOSettings />)

    const toggle = await screen.findByRole('switch', { name: /require sso/i })
    expect(toggle).toBeDisabled()
    expect(screen.getByText(/run a successful test first/i)).toBeInTheDocument()
  })

  it('enables and PATCHes enforcement once the gates are met', async () => {
    seedEnforceReady()
    render(<SSOSettings />)

    const toggle = await screen.findByRole('switch', { name: /require sso/i })
    expect(toggle).not.toBeDisabled()
    fireEvent.click(toggle)

    await waitFor(() =>
      expect(apiMocks.apiJSON).toHaveBeenCalledWith(
        '/api/sso/connection/enforcement',
        expect.objectContaining({ method: 'PATCH', body: JSON.stringify({ enforced: true }) }),
      ),
    )
    expect(await screen.findByText(/sso required/i)).toBeInTheDocument()
  })

  it('says what requiring SSO does, in the hint and in the confirm', async () => {
    const effect =
      'Everyone must sign in through your identity provider to use this organization, except break-glass accounts. People signed in any other way lose access on their next request.'
    seedEnforceReady()
    render(<SSOSettings />)

    expect(await screen.findByText(effect)).toBeInTheDocument()
    expect(screen.queryByText(/off-domain users/i)).not.toBeInTheDocument()

    fireEvent.click(screen.getByRole('switch', { name: /require sso/i }))
    await waitFor(() => expect(window.confirm).toHaveBeenCalledWith(effect))
  })

  it('shows the refusal when the admin would lock themselves out', async () => {
    seedEnforceReady()
    const message =
      'Sign in through single sign-on, or be designated break-glass, before requiring it.'
    refusals['PATCH /api/sso/connection/enforcement'] = conflict(message)
    render(<SSOSettings />)

    fireEvent.click(await screen.findByRole('switch', { name: /require sso/i }))

    await waitFor(() => expect(toasts.toast.error).toHaveBeenCalledWith(message))
    expect(screen.getByRole('switch', { name: /require sso/i })).not.toBeChecked()
  })

  it('shows the refusal to disable the connection while SSO is required', async () => {
    seedEnforceReady()
    connState = {
      ...connState,
      connection: makeConnection({ enabled: true, tested: true, enforced: true }),
    }
    refusals['PATCH /api/sso/connection'] = conflict('Turn off Require SSO first.')
    render(<SSOSettings />)

    fireEvent.click(await screen.findByRole('switch', { name: /enable sso/i }))

    await waitFor(() =>
      expect(toasts.toast.error).toHaveBeenCalledWith('Turn off Require SSO first.'),
    )
    expect(screen.getByRole('switch', { name: /enable sso/i })).toBeChecked()
  })

  it('shows the refusal to remove the last verified domain while SSO is required', async () => {
    seedEnforceReady()
    refusals['DELETE /api/sso/domains/dom-1'] = conflict('Turn off Require SSO first.')
    render(<SSOSettings />)

    await screen.findByText('example.com')
    fireEvent.click(screen.getByRole('button', { name: /remove example\.com/i }))

    await waitFor(() =>
      expect(toasts.toast.error).toHaveBeenCalledWith('Turn off Require SSO first.'),
    )
    expect(screen.getByText('example.com')).toBeInTheDocument()
  })

  it('adds a break-glass principal by email', async () => {
    seedEnforceReady()
    render(<SSOSettings />)

    const input = (await screen.findByLabelText(/break-glass principal email/i)) as HTMLInputElement
    fireEvent.change(input, { target: { value: 'recovery@corp.com' } })
    fireEvent.click(screen.getByRole('button', { name: /^add$/i }))

    await waitFor(() =>
      expect(apiMocks.apiJSON).toHaveBeenCalledWith(
        '/api/sso/break-glass',
        expect.objectContaining({
          method: 'POST',
          body: expect.stringContaining('recovery@corp.com'),
        }),
      ),
    )
    expect(await screen.findByText('recovery@corp.com')).toBeInTheDocument()
  })
})
