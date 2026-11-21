import { apiFetch } from './apiClient'

// dismissDeliverable abandons one deliverable that waits on a person. One
// route serves both kinds: a draft pull request is closed on GitHub (branch
// kept), and a staged review is dismissed without leaving this process.
export async function dismissDeliverable(id: string): Promise<void> {
  await apiFetch(`/api/deliverables/${encodeURIComponent(id)}/dismiss`, { method: 'POST' })
}
