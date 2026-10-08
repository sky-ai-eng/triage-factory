// The connected group's two inline actions.
import { describe, it, expect, vi } from 'vitest'
import { render, screen } from '@testing-library/react'

import JiraAccessGroup from './JiraAccessGroup'

function renderConnected() {
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
      bare
    />,
  )
  return { onReplace }
}

describe('JiraAccessGroup · connected actions', () => {
  it('offers an in-place rebind and a disconnect', () => {
    renderConnected()
    expect(screen.getByRole('button', { name: 'Replace credential' })).toBeInTheDocument()
    expect(screen.getByRole('button', { name: 'Disconnect' })).toBeInTheDocument()
  })
})
