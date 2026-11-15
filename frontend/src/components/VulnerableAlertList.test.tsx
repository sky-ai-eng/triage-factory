import { describe, it, expect } from 'vitest'
import { render, screen } from '@testing-library/react'
import VulnerableAlertList from './VulnerableAlertList'
import type { DependabotAlert } from '../types'

const alerts: DependabotAlert[] = [
  {
    number: 11,
    url: 'https://github.com/octo/api/security/dependabot/11',
    ghsa_id: 'GHSA-high-0000-0000',
    cve_id: 'CVE-2026-7',
    summary: '<img src=x onerror=alert(1)> Prototype pollution, see https://evil.example',
    severity: 'high',
    manifest_path: 'package-lock.json',
    scope: 'runtime',
    vulnerable_version_range: '< 4.17.21',
    first_patched_version: '4.17.21',
  },
  {
    number: 4,
    url: 'https://github.com/octo/api/security/dependabot/4',
    ghsa_id: 'GHSA-low0-0000-0000',
    severity: 'low',
    manifest_path: 'web/package.json',
    scope: 'development',
  },
]

describe('VulnerableAlertList', () => {
  it('lists each alert with its advisory, severity, manifest, scope and fix, linked to GitHub', () => {
    render(<VulnerableAlertList alerts={alerts} />)
    const items = screen.getAllByRole('listitem')
    expect(items).toHaveLength(2)

    expect(items[0]).toHaveTextContent('High')
    expect(items[0]).toHaveTextContent('CVE-2026-7')
    expect(items[0]).toHaveTextContent('package-lock.json')
    expect(items[0]).toHaveTextContent('Runtime')
    expect(items[0]).toHaveTextContent('Fixed in 4.17.21')
    expect(screen.getByRole('link', { name: /GHSA-high-0000-0000/ })).toHaveAttribute(
      'href',
      'https://github.com/octo/api/security/dependabot/11',
    )

    expect(items[1]).toHaveTextContent('Low')
    expect(items[1]).toHaveTextContent('Development')
    expect(items[1]).toHaveTextContent('No patched version')
  })

  it('renders the advisory summary as plain text: no markup and no links', () => {
    const { container } = render(<VulnerableAlertList alerts={alerts} />)
    expect(container.querySelector('img')).toBeNull()
    expect(
      screen.getByText(
        '<img src=x onerror=alert(1)> Prototype pollution, see https://evil.example',
      ),
    ).toBeInTheDocument()
    // The only links are the two alerts' own.
    expect(screen.getAllByRole('link').map((a) => a.getAttribute('href'))).toEqual([
      'https://github.com/octo/api/security/dependabot/11',
      'https://github.com/octo/api/security/dependabot/4',
    ])
  })

  it('says so when no alert is open', () => {
    render(<VulnerableAlertList alerts={[]} />)
    expect(screen.getByText('No open alerts.')).toBeInTheDocument()
  })
})
