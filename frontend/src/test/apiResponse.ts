// Response stubs for tests that mock the global fetch.
//
// Everything reaching the backend goes through lib/apiClient, and apiJSON reads
// the body with `text()` and parses it itself — that is its non-JSON guard (a
// 2xx carrying the dev server's index.html must surface as an HttpError, not a
// raw SyntaxError). apiFetch reads `text()` too, for the error body an
// HttpError carries. So a stub that implements only `json()` answers a call the
// app no longer makes, and the component under test fails on
// "resp.text is not a function" rather than on anything it does.
//
// `json` is kept alongside `text` because a few call sites still read the
// Response directly — a DELETE whose body is optional (orgCredentials'
// credentialRequest, PromptDrawer's prompt delete) parses best-effort with
// `res.json().catch(...)`.

/** jsonBody spreads into a stub Response, filling both body readers from one
 *  value: `{ ok: true, ...jsonBody(rows) }`.
 *
 *  The body may be a promise — a test that parks a read open passes the
 *  deferred directly — so both readers await it rather than serializing
 *  whatever object was handed in.
 *
 *  An ABSENT body is an empty one, not the string "undefined". `JSON.stringify`
 *  returns the *value* `undefined` (not a string) for `undefined` and anything
 *  else it can't serialize, so a stub built from a body-less response — a
 *  failure case written `{ ok: false }` with no `body` — would otherwise hand
 *  apiClient an `undefined` where `Response.text(): Promise<string>` promises a
 *  string, and HttpError would carry `HTTP 500: undefined` into the assertion.
 *  A real bodyless Response answers `''` from text() and rejects from json();
 *  both readers agree on that here, so a test can't get one reader's story and
 *  the other's. */
export function jsonBody(body: unknown): {
  json: () => Promise<unknown>
  text: () => Promise<string>
} {
  // Serialized once per read rather than at construction, because `body` may be
  // a promise a test resolves later.
  const serialize = async () => JSON.stringify(await body)
  return {
    json: async () => {
      const resolved = await body
      if (JSON.stringify(resolved) === undefined) {
        // What a real Response does on an empty body, and what apiJSON's
        // non-JSON guard is written against.
        throw new SyntaxError('Unexpected end of JSON input')
      }
      return resolved
    },
    text: async () => (await serialize()) ?? '',
  }
}
