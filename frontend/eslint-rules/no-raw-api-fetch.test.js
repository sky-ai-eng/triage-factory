// @vitest-environment node
//
// Node, not the suite's default jsdom, to match its sibling guard's harness —
// RuleTester has no use for a DOM and the environments differ on globals.
import { RuleTester } from 'eslint'
import tseslint from 'typescript-eslint'
import { describe, it } from 'vitest'
import rule from './no-raw-api-fetch.js'

// RuleTester runs its assertions inline when no global describe/it exists
// (vitest runs with globals off), so each run() call throws inside its own it().
const ruleTester = new RuleTester({
  languageOptions: {
    parser: tseslint.parser,
    ecmaVersion: 2022,
    sourceType: 'module',
  },
})

const rawFetch = (url) => ({ messageId: 'rawFetch', data: { url } })

describe('no-raw-api-fetch', () => {
  it('flags a raw fetch against /api, in every spelling that reached main', () => {
    ruleTester.run('no-raw-api-fetch', rule, {
      valid: [
        // The wrapper itself, in both call shapes.
        "const res = await apiFetch('/api/tasks')",
        'const data = await apiJSON<Team[]>(`/api/orgs/${orgId}/teams`)',
        // A fetch that isn't the backend: no cookie, no 401 funnel, no server
        // `{ error }` convention — none of this rule's business.
        "await fetch('https://api.github.com/user')",
        'await fetch(`${base}/healthz`)',
        "await fetch('/healthz')",
        // A path that merely starts with the same letters. `/api-docs` is a
        // different route on the same origin, and a bare startsWith would
        // claim it.
        "await fetch('/api-docs')",
        "await fetch('/apiary/things')",
        // A computed URL is out of scope by construction: seeing it needs
        // constant propagation, and no call site in this codebase writes one.
        'await fetch(url)',
        'await fetch(base + path)',
        // A method NAMED fetch on something that isn't the global.
        "await client.fetch('/api/tasks')",
        "await this.fetch('/api/tasks')",
      ],
      invalid: [
        {
          code: "const res = await fetch('/api/tasks')",
          errors: [rawFetch('/api/tasks')],
        },
        // The interpolated form — the majority of the sweep. The literal's
        // first quasi is the prefix.
        {
          code: 'const res = await fetch(`/api/orgs/${orgId}/teams`)',
          errors: [rawFetch('/api/orgs/')],
        },
        // With options, which is how every mutation is written.
        {
          code: "await fetch('/api/tasks/1/swipe', { method: 'POST', body })",
          errors: [rawFetch('/api/tasks/1/swipe')],
        },
        // The bare-prefix template form: the first quasi stops at `/api`, which
        // is still one of ours.
        {
          code: 'await fetch(`/api${path}`)',
          errors: [rawFetch('/api')],
        },
        // A query string straight off the mount point.
        {
          code: "await fetch('/api?probe=1')",
          errors: [rawFetch('/api?probe=1')],
        },
        // Not awaited, and used as a promise — the .then() form.
        {
          code: "fetch('/api/config').then((r) => r.json())",
          errors: [rawFetch('/api/config')],
        },
        // window./globalThis.-qualified: the same call, spelled around the lint.
        {
          code: "await window.fetch('/api/me')",
          errors: [rawFetch('/api/me')],
        },
        {
          code: 'await globalThis.fetch(`/api/runs/${id}`)',
          errors: [rawFetch('/api/runs/')],
        },
        // Two in one file both report — the rule has no per-file latch.
        {
          code: "await fetch('/api/a'); await fetch('/api/b')",
          errors: [rawFetch('/api/a'), rawFetch('/api/b')],
        },
      ],
    })
  })

  it('exempts lib/apiClient.ts, where the real fetch has to happen', () => {
    ruleTester.run('no-raw-api-fetch', rule, {
      valid: [
        { code: "await fetch('/api/tasks')", filename: 'src/lib/apiClient.ts' },
        { code: "await fetch('/api/tasks')", filename: '/abs/frontend/src/lib/apiClient.ts' },
      ],
      invalid: [
        // The exemption is that one file, not the directory around it.
        {
          code: "await fetch('/api/tasks')",
          filename: 'src/lib/api.ts',
          errors: [rawFetch('/api/tasks')],
        },
        // Tests are not exempt: a test that hand-rolls a request against /api
        // is asserting on a code path the app no longer has.
        {
          code: "await fetch('/api/tasks')",
          filename: 'src/pages/Repos.test.tsx',
          errors: [rawFetch('/api/tasks')],
        },
      ],
    })
  })
})
