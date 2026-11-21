import { describe, it, expect, vi, beforeEach } from 'vitest'
import { captureJiraIdentityApiToken, captureJiraIdentityPat } from './jiraIdentity'
import { jsonBody } from '../test/apiResponse'

// Each deployment's credential goes to its own route, which refuses the
// other's body, so a swapped route fails every bind of that deployment.

const fetchMock = vi.fn()

beforeEach(() => {
  fetchMock.mockReset()
  fetchMock.mockResolvedValue({
    ok: true,
    status: 200,
    ...jsonBody({ account: 'Ada', host: 'https://jira.example.com' }),
  })
  vi.stubGlobal('fetch', fetchMock)
})

describe('Jira identity capture', () => {
  it('posts a personal access token to the pat route', async () => {
    expect(await captureJiraIdentityPat('org 1', 'tok')).toEqual({
      account: 'Ada',
      host: 'https://jira.example.com',
    })
    expect(fetchMock).toHaveBeenCalledWith(
      '/api/orgs/org%201/jira/identity/pat',
      expect.objectContaining({ method: 'POST', body: JSON.stringify({ pat: 'tok' }) }),
    )
  })

  it('posts an email and API token to the api-token route', async () => {
    await captureJiraIdentityApiToken('org 1', 'ada@example.com', 'cloud-tok')
    expect(fetchMock).toHaveBeenCalledWith(
      '/api/orgs/org%201/jira/identity/api-token',
      expect.objectContaining({
        method: 'POST',
        body: JSON.stringify({ email: 'ada@example.com', token: 'cloud-tok' }),
      }),
    )
  })
})
