// readError extracts a usable error message from a non-ok fetch response.
// Tries the `error` field of a JSON body first (the server's convention
// in writeJSON responses), then falls back to the status text + code so
// toasts always show *something* meaningful instead of "[object Object]".
export async function readError(res: Response, fallback: string): Promise<string> {
  try {
    const body = await res.clone().json()
    if (body && typeof body.error === 'string' && body.error.length > 0) {
      return `${fallback}: ${body.error}`
    }
  } catch {
    // Body wasn't JSON — fall through to the text path.
  }
  try {
    const text = await res.text()
    if (text) return `${fallback}: ${text}`
  } catch {
    // ignore
  }
  return `${fallback} (${res.status})`
}
