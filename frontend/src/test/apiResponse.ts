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
 *  whatever object was handed in. */
export function jsonBody(body: unknown): {
  json: () => Promise<unknown>
  text: () => Promise<string>
} {
  return {
    json: () => Promise.resolve(body),
    text: async () => JSON.stringify(await body),
  }
}
