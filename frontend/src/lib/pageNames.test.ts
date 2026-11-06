import { describe, expect, it } from 'vitest'
import { pageNameFor, rememberPageName } from './pageNames'

describe('pageNames', () => {
  it('names a page by its path, whatever the query', () => {
    rememberPageName('/orgs/o1/knowledge', 'Acme knowledge')
    expect(pageNameFor('/orgs/o1/knowledge?tab=shared')).toBe('Acme knowledge')
  })

  // The shell's first render after a navigation can record the page it left
  // under the new path; a page that names itself nothing must not keep that.
  it('forgets a name when the page names itself nothing', () => {
    rememberPageName('/orgs/o1/unnamed', 'Acme knowledge')
    rememberPageName('/orgs/o1/unnamed', '')
    expect(pageNameFor('/orgs/o1/unnamed')).toBeUndefined()
  })
})
