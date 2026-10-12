import { describe, it, expect } from 'vitest'
import { safeReturnTo } from './returnTo'

describe('safeReturnTo', () => {
  it('keeps a path on this origin', () => {
    expect(safeReturnTo('/orgs/o1/board', '/home')).toBe('/orgs/o1/board')
    expect(safeReturnTo('/orgs/o1/board?tab=2#top', '/home')).toBe('/orgs/o1/board?tab=2#top')
  })

  it('falls back with nothing to keep', () => {
    expect(safeReturnTo(null, '/home')).toBe('/home')
    expect(safeReturnTo('', '/home')).toBe('/home')
    expect(safeReturnTo('orgs/o1', '/home')).toBe('/home')
  })

  it.each([
    ['//evil.example'],
    ['https://evil.example/'],
    ['javascript:alert(1)'],
    ['/\\evil.example'],
    ['/%5Cevil.example'],
    ['/%5cevil.example'],
    ['/\t/evil.example'],
    ['/\n/evil.example'],
    ['/\r/evil.example'],
    ['/%E0'],
  ])('refuses %j', (raw) => {
    expect(safeReturnTo(raw, '/home')).toBe('/home')
  })

  it('refuses what a query string decodes to', () => {
    // URLSearchParams decodes once, so this is the value a page reads.
    const read = new URLSearchParams('return_to=/%5Cevil.example').get('return_to')
    expect(read).toBe('/\\evil.example')
    expect(safeReturnTo(read, '/home')).toBe('/home')
  })
})
