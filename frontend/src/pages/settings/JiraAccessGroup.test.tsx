// The connected group's two inline actions. What's pinned is the env-overlay
// rule (TFAC-677): TRIAGE_FACTORY_JIRA_* wins on every read, so a credential
// typed here would be stored and then ignored — the group states that and
// withholds the rebind, rather than offering a control that reports success and
// changes nothing. Disconnect stays, because it's honest about its own outcome.
import { describe, it, expect, vi } from 'vitest'
import { render, screen } from '@testing-library/react'

import JiraAccessGroup from './JiraAccessGroup'

function renderConnected(over: { envProvided?: boolean } = {}) {
  const onReplace = vi.fn()
  render(
    <JiraAccessGroup
      value={{
        jira_url: 'https://jira.example.com',
        jira_pat: '',
        jira_email: '',
        jira_api_token: '',
      }}
      onChange={() => {}}
      connected
      orgId="org-1"
      deployment="data_center"
      onReplace={onReplace}
      envProvided={over.envProvided ?? false}
      bare
    />,
  )
  return { onReplace }
}

describe('JiraAccessGroup · connected actions', () => {
  it('offers an in-place rebind on a vault-stored credential', () => {
    renderConnected()
    expect(screen.getByRole('button', { name: 'Replace credential' })).toBeInTheDocument()
    expect(screen.getByRole('button', { name: 'Disconnect' })).toBeInTheDocument()
  })

  it('withholds the rebind when the environment supplies the connection', () => {
    renderConnected({ envProvided: true })
    expect(screen.queryByRole('button', { name: 'Replace credential' })).not.toBeInTheDocument()
    expect(screen.getByText(/TRIAGE_FACTORY_JIRA_\*/)).toBeInTheDocument()
    // Still disconnectable — that path reports the overlay caveat itself.
    expect(screen.getByRole('button', { name: 'Disconnect' })).toBeInTheDocument()
  })
})
