import type { Mock } from 'vitest'
import { httpErrorMessage } from '../lib/apiClient'

/** reportFailedThroughError makes a toast double's `failed` report through its
 *  `error`, with the message the real `toast.failed` shows, so a test asserts
 *  the words a person reads whichever of the two the code calls. Call it once,
 *  at module scope, after the double is defined. */
export function reportFailedThroughError(t: { error: Mock; failed: Mock }) {
  t.failed.mockImplementation((err: unknown, fallback: string, title?: string) =>
    title === undefined
      ? t.error(httpErrorMessage(err, fallback))
      : t.error(httpErrorMessage(err, fallback), title),
  )
}
