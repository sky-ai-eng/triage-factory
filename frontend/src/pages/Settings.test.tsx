import { describe, it, expect, vi } from 'vitest'
import { render, screen } from '@testing-library/react'
import Settings from './Settings'

// One route, two pages, by mode. What is worth pinning is the seam: local mode
// keeps the Org / Team / User stack it always had, and multi mode is the
// personal settings page — with nothing of the other mode's leaking through.

const authMock = vi.hoisted(() => ({ local: true }))
vi.mock('../contexts/AuthContext', () => ({
  useOptionalAuth: () => (authMock.local ? null : ({} as unknown)),
}))
vi.mock('../contexts/OrgContext', () => ({ useActiveOrgId: () => 'o1' }))

// The two pages are theirs to test; here they are stand-ins.
vi.mock('./usersettings/UserSettingsPage', () => ({
  default: () => <div data-testid="multi-page">the personal settings page</div>,
}))
vi.mock('./settings/stack/OrgSettings', () => ({ default: () => <div>org stack</div> }))
vi.mock('./settings/stack/TeamSettings', () => ({ default: () => <div>team stack</div> }))
vi.mock('./settings/stack/UserSettings', () => ({ default: () => <div>user stack</div> }))
vi.mock('./setup/glass', () => ({ GlassBackdrop: () => null }))

describe('Settings', () => {
  it('keeps the Org / Team / User stack in local mode, with no token surface', () => {
    authMock.local = true
    render(<Settings />)
    expect(screen.getByText('Organization')).toBeInTheDocument()
    expect(screen.getByText('Team')).toBeInTheDocument()
    expect(screen.getByText('user stack')).toBeInTheDocument()
    expect(screen.queryByTestId('multi-page')).not.toBeInTheDocument()
  })

  it('is the personal settings page in multi mode', () => {
    authMock.local = false
    render(<Settings />)
    expect(screen.getByTestId('multi-page')).toBeInTheDocument()
    expect(screen.queryByText('org stack')).not.toBeInTheDocument()
  })
})
