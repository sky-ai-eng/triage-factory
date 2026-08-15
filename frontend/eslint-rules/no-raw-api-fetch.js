// no-raw-api-fetch — fail the lint when application code calls the backend
// with a bare `fetch('/api/…')` instead of going through lib/apiClient.
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
// SCOPE — a `fetch()` whose first argument is a string or template literal
// beginning `/api`. That is deliberately syntactic and deliberately narrow:
//
//   - A computed URL (`fetch(url)`, `fetch(base + path)`) is invisible here.
//     Seeing it would need type or constant-propagation information, and the
//     shape does not occur in this codebase — every call site writes its path
//     inline, which is exactly why a literal check catches them all.
//   - A non-/api fetch is none of this rule's business: `fetch` against a
//     third-party URL or a blob has no cookie, no 401 funnel, and no server
//     `{ error }` convention to share.
//
// A template literal counts when its first quasi starts `/api` — that is the
// interpolated-path form (`/api/orgs/${orgId}/teams`), which is the majority.
//
// lib/apiClient.ts is the one exemption, since the wrapper is where the real
// fetch has to happen. Tests are not exempt: a test that stubs `fetch` stubs
// the global, and one that hand-rolls a request against /api is asserting on a
// code path the app no longer has.

const EXEMPT_FILE = 'lib/apiClient.ts'

// The backend is mounted at /api, so a path is one of ours when it is exactly
// `/api` (the template-literal form, `fetch(`/api${path}`)`, whose first quasi
// stops there) or continues with a separator. Matching a bare `startsWith`
// would also claim `/api-docs`, a different route on the same origin.
const API_PATH = /^\/api($|[/?#])/

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

/** @type {import('eslint').Rule.RuleModule} */
export default {
  meta: {
    type: 'problem',
    docs: {
      description: 'disallow calling /api with a raw fetch instead of lib/apiClient',
    },
    schema: [],
    messages: {
      rawFetch:
        "Call the API through apiFetch/apiJSON from lib/apiClient, not a raw fetch('{{url}}…'). A raw fetch bypasses the 401 re-auth funnel, the non-JSON body guard, and the httpErrorMessage convention. Use apiJSON<T>(path) for a JSON body, apiFetch(path) for a 204, and the `allow` option for a status the call deliberately tolerates.",
    },
  },

  create(context) {
    // Normalized so the check is the same on Windows path separators.
    const filename = context.filename.replaceAll('\\', '/')
    if (filename.endsWith(EXEMPT_FILE)) return {}

    return {
      CallExpression(node) {
        if (!isFetchCallee(node.callee)) return
        const prefix = urlPrefix(node.arguments[0])
        if (prefix === null || !API_PATH.test(prefix)) return
        context.report({
          node: node.arguments[0],
          messageId: 'rawFetch',
          data: { url: prefix },
        })
      },
    }
  },
}
