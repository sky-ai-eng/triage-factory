import { apiFetch } from './apiClient'

// dismissDeliverable abandons one deliverable that waits on a person. The two
// kinds take different routes because they are different acts: closing a
// draft pull request is a GitHub write (the /dismiss verb, branch kept), while
// abandoning a staged review never leaves this process (a state write on the
// review sub-resource).
export async function dismissDeliverable(id: string, kind: string): Promise<void> {
  const path = `/api/deliverables/${encodeURIComponent(id)}`
  if (kind === 'review') {
    await apiFetch(`${path}/review`, {
      method: 'PATCH',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ state: 'dismissed' }),
    })
    return
  }
  await apiFetch(`${path}/dismiss`, { method: 'POST' })
}
