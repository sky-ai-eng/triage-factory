// no-raw-api-fetch — fail the lint when application code calls the backend with
// a bare `fetch()` instead of going through lib/apiClient.
//
// Why a lint rule and not a convention: the convention already existed and lost
// 146 call sites to 50. A raw fetch is the path of least resistance — it needs
// no import — and every one of them silently opts out of three things the
// wrapper owns:
//
//   1. The 401 funnel. `unauthHandler` is invoked inside apiFetch, and
//      AuthContext registers it at mount so an expired session re-authenticates.
//      A raw fetch never reaches it: the page renders a failed read and sits
//      there until the user reloads.
//   2. The non-JSON guard. apiJSON re-casts a 2xx-with-HTML body as an
//      HttpError, because the Vite dev server answers an unproxied /api path
//      with 200 + index.html. A bare res.json() throws a native SyntaxError
//      whose parser message reaches the UI.
//   3. One error-string convention. Everything that throws an HttpError lands
//      on httpErrorMessage; a hand-rolled `!res.ok` arm invents its own.
//
// SCOPE — the rule flags every `fetch()` it cannot PROVE points somewhere other
// than the backend. That direction is the whole design, and it is the correction
// of an earlier version that only flagged a first argument literally beginning
// `/api`:
//
//     const url = `/api/settings/team/${id}/github-groups`
//     await fetch(url)                       // invisible to a literal check
//     await fetch(teamPath(teamId))          // likewise
//     await fetch(`${base}/${promptId}`)     // likewise
//
// Ten call sites across four files were written that way, and a guard that
// reads only the inline form is a guard that rewards hoisting the URL into a
// variable. Since the rule cannot follow a value to its definition without
// constant-propagation or type information, an opaque argument is reported
// rather than waved through: "I can't see where this points" is the finding.
//
// What it does NOT flag is anything a literal proves is elsewhere — a
// scheme-qualified or protocol-relative URL (a third party, a blob, a data
// URI), or a root-relative path that isn't the backend mount. Those have no
// cookie, no 401 funnel and no server `{ error }` convention to share, so they
// are none of this rule's business.
//
// A genuinely-external fetch built from a variable is the one shape this costs:
// it needs an inline eslint-disable naming the destination. That is the right
// price — it makes "this bypasses the API client, deliberately" a thing someone
// wrote down, and no such call exists in this codebase today.
//
// lib/apiClient.ts is the one file-level exemption, since the wrapper is where
// the real fetch has to happen. Tests are not exempt: a test that stubs `fetch`
// stubs the global (not a call), and one that hand-rolls a request against /api
// is asserting on a code path the app no longer has.

const EXEMPT_FILE = 'lib/apiClient.ts'

// The backend is mounted at /api, so a path is one of ours when it is exactly
// `/api` (the template-literal form, `fetch(`/api${path}`)`, whose first quasi
// stops there) or continues with a separator. Matching a bare `startsWith`
// would also claim `/api-docs`, a different route on the same origin.
const API_PATH = /^\/api($|[/?#])/

// A URL that names its own origin — `https:`, `blob:`, `data:`, or the
// protocol-relative `//host/…`. Nothing same-origin can hide behind one.
const ABSOLUTE_URL = /^([a-z][a-z0-9+.-]*:|\/\/)/i

/** The string an argument node contributes as the start of the URL, or null if
 *  the node is not a literal path. A template literal's first quasi is the
 *  prefix — everything before the first `${}` — which is where `/api` lives in
 *  every interpolated path. */
function urlPrefix(node) {
  if (!node) return null
  if (node.type === 'Literal' && typeof node.value === 'string') return node.value
  if (node.type === 'TemplateLiteral') {
    const first = node.quasis[0]
    return first ? first.value.cooked : null
  }
  return null
}

/** Whether a literal prefix proves the request goes somewhere other than this
 *  app's `/api`. Only a proof counts — everything else is reported. */
function isProvablyElsewhere(prefix) {
  if (ABSOLUTE_URL.test(prefix)) return true
  // A root-relative path pins the whole path, so one that isn't /api can't
  // become /api through interpolation.
  return prefix.startsWith('/') && !API_PATH.test(prefix)
}

function isFetchCallee(callee) {
  // `fetch(…)` and `window.fetch(…)` / `globalThis.fetch(…)` — the same call
  // spelled three ways.
  if (callee.type === 'Identifier') return callee.name === 'fetch'
  if (callee.type === 'MemberExpression' && !callee.computed) {
    return (
      callee.property.type === 'Identifier' &&
      callee.property.name === 'fetch' &&
      callee.object.type === 'Identifier' &&
      (callee.object.name === 'window' || callee.object.name === 'globalThis')
    )
  }
  return false
}

const GUIDANCE =
  'A raw fetch bypasses the 401 re-auth funnel, the non-JSON body guard, and the httpErrorMessage convention. Use apiJSON<T>(path) for a JSON body, apiFetch(path) for a 204 / header / non-JSON body, and the `allow` option for a status the call deliberately tolerates.'

/** @type {import('eslint').Rule.RuleModule} */
export default {
  meta: {
    type: 'problem',
    docs: {
      description: 'disallow calling the backend with a raw fetch instead of lib/apiClient',
    },
    schema: [],
    messages: {
      rawFetch: `Call the API through apiFetch/apiJSON from lib/apiClient, not a raw fetch('{{url}}…'). ${GUIDANCE}`,
      opaqueFetch: `This fetch's URL isn't a literal, so the lint can't tell whether it points at /api — and hoisting the path into a variable is exactly how ten call sites escaped an earlier version of this rule. Route it through apiFetch/apiJSON from lib/apiClient. ${GUIDANCE} If it genuinely targets another origin, disable this rule on the line and say where it points.`,
    },
  },

  create(context) {
    // Normalized so the check is the same on Windows path separators.
    const filename = context.filename.replaceAll('\\', '/')
    if (filename.endsWith(EXEMPT_FILE)) return {}

    return {
      CallExpression(node) {
        if (!isFetchCallee(node.callee)) return
        const target = node.arguments[0]
        const prefix = urlPrefix(target)
        // An EMPTY prefix is as opaque as no literal at all: a template that
        // opens on its interpolation (`` `${base}/${promptId}` ``) has an empty
        // first quasi, and that is the exact shape PromptDrawer and teamConfig
        // used to slip past the literal-only version of this rule. It proves
        // nothing, so it can neither exempt the call nor be quoted back at the
        // author as the URL.
        const visible = prefix !== null && prefix !== ''
        if (visible && isProvablyElsewhere(prefix)) return
        context.report({
          node: target ?? node,
          messageId: visible ? 'rawFetch' : 'opaqueFetch',
          data: { url: prefix ?? '' },
        })
      },
    }
  },
}
