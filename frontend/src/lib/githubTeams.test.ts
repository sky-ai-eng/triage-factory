import { describe, expect, it } from 'vitest'

import { groupKey, identifyGroups, type GitHubTeamRef } from './githubTeams'

const ref = (id: string, org: string, slug: string, orgId = '9'): GitHubTeamRef => ({
  github_team_id: id,
  github_org_id: id ? orgId : '',
  org_login: org,
  team_slug: slug,
})

describe('groupKey', () => {
  it('keys a team by GitHub id, whatever it is called', () => {
    expect(groupKey(ref('11', 'acme', 'backend'))).toBe(groupKey(ref('11', 'acme-corp', 'server')))
    expect(groupKey(ref('11', 'acme', 'backend'))).not.toBe(groupKey(ref('12', 'acme', 'backend')))
  })

  it('keys a mapping with no id by its case-folded handle', () => {
    expect(groupKey(ref('', 'Acme', 'Backend'))).toBe(groupKey(ref('', 'acme', 'backend')))
    expect(groupKey(ref('', 'acme', 'backend'))).not.toBe(groupKey(ref('11', 'acme', 'backend')))
  })
})

describe('identifyGroups', () => {
  it('gives a mapping with no id the candidate with its handle', () => {
    const candidates = [ref('11', 'acme', 'backend'), ref('12', 'acme', 'frontend')]
    expect(
      identifyGroups([ref('', 'ACME', 'backend'), ref('12', 'acme', 'old-name')], candidates),
    ).toEqual([ref('11', 'acme', 'backend'), ref('12', 'acme', 'old-name')])
  })

  it('leaves a mapping no candidate matches as it is', () => {
    expect(identifyGroups([ref('', 'acme', 'gone')], [ref('11', 'acme', 'backend')])).toEqual([
      ref('', 'acme', 'gone'),
    ])
  })
})
