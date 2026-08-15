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
const opaqueFetch = { messageId: 'opaqueFetch' }

describe('no-raw-api-fetch', () => {
  it('flags a raw fetch against /api, in every spelling that reached main', () => {
    ruleTester.run('no-raw-api-fetch', rule, {
      valid: [
        // The wrapper itself, in both call shapes.
        "const res = await apiFetch('/api/tasks')",
        'const data = await apiJSON<Team[]>(`/api/orgs/${orgId}/teams`)',
        // A literal that PROVES another origin: a scheme, or protocol-relative.
        // No cookie, no 401 funnel, no server `{ error }` convention — none of
        // this rule's business.
        "await fetch('https://api.github.com/user')",
        "await fetch('//cdn.example.com/logo.png')",
        "await fetch('blob:http://localhost/abc')",
        "await fetch('data:text/plain,hi')",
        // A root-relative path pins the whole path, so one that isn't /api
        // can't become /api through interpolation.
        "await fetch('/healthz')",
        'await fetch(`/healthz?since=${t}`)',
        // A path that merely starts with the same letters. `/api-docs` is a
        // different route on the same origin, and a bare startsWith would
        // claim it.
        "await fetch('/api-docs')",
        "await fetch('/apiary/things')",
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
        // ── The shapes that escaped the literal-only version of this rule.
        // Every one of these is a real call site from the sweep's follow-up:
        // hoisting the path into a variable or a helper is how ten /api calls
        // stayed raw through a "migrate every raw fetch" PR.
        {
          // GitHubTeamSelector: a local const holding an /api template.
          code: 'const url = `/api/settings/team/${id}/groups`; await fetch(url)',
          errors: [opaqueFetch],
        },
        {
          // teamConfig: the path comes from a helper.
          code: 'await fetch(teamPath(teamId))',
          errors: [opaqueFetch],
        },
        {
          // teamConfig again: helper call inside the template, so the first
          // quasi is empty and proves nothing.
          code: 'await fetch(`${teamPath(teamId)}/repos`)',
          errors: [opaqueFetch],
        },
        {
          // PromptDrawer / TaskRuleEditor: a `base` prop from lib/scope.
          code: 'await fetch(`${base}/${promptId}`)',
          errors: [opaqueFetch],
        },
        {
          code: "await fetch(base, { method: 'POST', body })",
          errors: [opaqueFetch],
        },
        // Concatenation and a bare identifier are the same story.
        {
          code: 'await fetch(base + path)',
          errors: [opaqueFetch],
        },
        {
          code: 'await fetch(url)',
          errors: [opaqueFetch],
        },
        // A call with no argument at all still can't be proven safe.
        {
          code: 'await fetch()',
          errors: [opaqueFetch],
        },
        // An empty first quasi proves nothing, so it can't exempt the call —
        // and the empty string must not be quoted back as though it were the
        // URL, which is why this reports opaque rather than raw.
        {
          code: "await fetch('')",
          errors: [opaqueFetch],
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
