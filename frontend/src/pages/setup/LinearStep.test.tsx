import { describe, it, expect, vi } from 'vitest'
import { render, screen, waitFor } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { LinearAccessStep } from './LinearStep'
import { initialWizardState } from './steps'
import { LOCAL_DEFAULT_ORG_ID } from '../../lib/githubApp'
import { jsonBody } from '../../test/apiResponse'

describe('LinearAccessStep — disconnecting', () => {
  // The unbind clears the workspace columns on the settings row, which moves
  // its version. Holding the old one would make the next org step's save
  // conflict with the wizard's own disconnect.
  it('holds the wizard and carries the settings version the unbind produced', async () => {
    const orgPath = `/api/orgs/${LOCAL_DEFAULT_ORG_ID}`
    vi.stubGlobal(
      'fetch',
      vi.fn((input: unknown) => {
        const path = String(input)
        if (path === `${orgPath}/linear/access/credential`) {
          return Promise.resolve({ ok: true, ...jsonBody({ status: 'disconnected' }) })
        }
        if (path === `${orgPath}/settings`) {
          return Promise.resolve({ ok: true, ...jsonBody({ version: 8 }) })
        }
        return Promise.resolve({ ok: false, status: 404, ...jsonBody({}) })
      }),
    )
    const base = initialWizardState()
    const patch = vi.fn()
    const hold = vi.fn((work: () => Promise<void>) => work())
    render(
      <LinearAccessStep
        state={{
          ...base,
          linearConnected: true,
          linearBoundAs: 'Ada',
          linearWorkspaceUrlKey: 'acme',
          org: { ...base.org, version: 7 },
        }}
        patch={patch}
        orgId={LOCAL_DEFAULT_ORG_ID}
        teamId="default"
        isLocal
        advance={() => {}}
        hold={hold}
      />,
    )

    await userEvent.click(screen.getByRole('button', { name: 'Disconnect' }))

    // The whole disconnect runs under the wizard's busy guard, so Continue
    // cannot skip the bind on a connection this click is removing.
    expect(hold).toHaveBeenCalledTimes(1)
    await waitFor(() =>
      expect(patch).toHaveBeenLastCalledWith({
        org: expect.objectContaining({ version: 8, linear_api_key: '' }),
      }),
    )
    expect(patch).toHaveBeenCalledWith(expect.objectContaining({ linearConnected: false }))
  })
})
