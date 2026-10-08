import { describe, it, expect, vi } from 'vitest'
import { render, screen, waitFor } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { JiraAccessStep } from './JiraStep'
import { initialWizardState } from './steps'
import { LOCAL_DEFAULT_ORG_ID } from '../../lib/githubApp'
import { jsonBody } from '../../test/apiResponse'

describe('JiraAccessStep — disconnecting', () => {
  it('holds the wizard while it disconnects, and carries the version the unbind left', async () => {
    const orgPath = `/api/orgs/${LOCAL_DEFAULT_ORG_ID}`
    vi.stubGlobal(
      'fetch',
      vi.fn((input: unknown) => {
        const path = String(input)
        if (path === `${orgPath}/jira/access/credential`) {
          return Promise.resolve({ ok: true, ...jsonBody({ status: 'disconnected' }) })
        }
        if (path === `${orgPath}/settings`) {
          // The unbind cleared the Jira URL on the settings row, moving its
          // version from the 7 the wizard holds.
          return Promise.resolve({ ok: true, ...jsonBody({ version: 8 }) })
        }
        return Promise.resolve({ ok: false, status: 404, ...jsonBody({}) })
      }),
    )
    const base = initialWizardState()
    const patch = vi.fn()
    let versionPatchedInsideHold = false
    const hold = vi.fn(async (work: () => Promise<void>) => {
      await work()
      versionPatchedInsideHold = patch.mock.calls.some(([p]) => p.org?.version === 8)
    })
    render(
      <JiraAccessStep
        state={{
          ...base,
          jiraConnected: true,
          jiraUrlConfirmed: true,
          jiraDeployment: 'data_center',
          org: { ...base.org, jira_url: 'https://jira.example.com', version: 7 },
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

    // The whole disconnect, the version re-read included, runs under the
    // wizard's busy guard, so Continue cannot skip the bind on a connection
    // this click is removing, nor save at the version the unbind replaced.
    expect(hold).toHaveBeenCalledTimes(1)
    await waitFor(() =>
      expect(patch).toHaveBeenLastCalledWith({
        jiraConnected: false,
        jiraUrlConfirmed: false,
        jiraDeployment: null,
        org: expect.objectContaining({
          jira_url: 'https://jira.example.com',
          jira_pat: '',
          version: 8,
        }),
      }),
    )
    expect(versionPatchedInsideHold).toBe(true)
  })
})
