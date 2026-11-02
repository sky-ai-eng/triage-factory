// The token field's "classic PAT" link points at the org's own GitHub.
import { describe, it, expect } from 'vitest'
import { render, screen } from '@testing-library/react'

import GitHubAccessGroup from './GitHubAccessGroup'

function renderWithURL(githubURL: string) {
  render(
    <GitHubAccessGroup
      value={{ github_url: githubURL, github_pat: '', github_clone_protocol: 'https' }}
      onChange={() => {}}
      hasToken={false}
      isLocal
      orgId={null}
      showAppPanel={false}
      showBaseUrl={false}
      showCloneProtocol={false}
      bare
    />,
  )
}

const query = '/settings/tokens/new?scopes=repo,read:org,user:email&description=Triage+Factory'

describe('GitHubAccessGroup · classic PAT link', () => {
  it('links to github.com for a github.com org', () => {
    renderWithURL('https://github.com')
    expect(screen.getByRole('link', { name: 'classic PAT' })).toHaveAttribute(
      'href',
      'https://github.com' + query,
    )
  })

  it('links to the Enterprise host the org is on', () => {
    renderWithURL('https://ghe.example.com/')
    expect(screen.getByRole('link', { name: 'classic PAT' })).toHaveAttribute(
      'href',
      'https://ghe.example.com' + query,
    )
  })

  it('renders no link when the base URL is unusable', () => {
    renderWithURL('')
    expect(screen.queryByRole('link', { name: 'classic PAT' })).not.toBeInTheDocument()
    expect(screen.getByText(/classic PAT/)).toBeInTheDocument()
  })
})
