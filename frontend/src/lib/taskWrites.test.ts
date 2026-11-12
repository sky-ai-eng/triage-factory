import { describe, it, expect, vi } from 'vitest'
import { jsonBody } from '../test/apiResponse'
import { HttpError } from './apiClient'
import {
  claimTask,
  delegateTask,
  isVersionConflict,
  patchTaskStatus,
  reassignTask,
  requeueTask,
  snoozeTask,
} from './taskWrites'

// Every task write sends the version its caller read, beside the fields the
// route takes, and nothing else: the routes decode strictly.

const TASK = { id: 'task-1', version: 7 }

function stubFetch() {
  const fetchMock = vi.fn().mockResolvedValue({ ok: true, status: 200, ...jsonBody({ ...TASK }) })
  vi.stubGlobal('fetch', fetchMock)
  return fetchMock
}

function sent(fetchMock: ReturnType<typeof stubFetch>) {
  const [path, init] = fetchMock.mock.calls[0] as [string, RequestInit]
  return { path, method: init.method, body: JSON.parse(String(init.body)) as unknown }
}

describe('task writes', () => {
  it.each([
    [
      'status',
      () => patchTaskStatus(TASK, 'done'),
      '/api/tasks/task-1',
      'PATCH',
      { status: 'done', version: 7 },
    ],
    [
      'snooze',
      () => snoozeTask(TASK, '2026-11-13T09:00:00.000Z'),
      '/api/tasks/task-1',
      'PATCH',
      { snooze_until: '2026-11-13T09:00:00.000Z', version: 7 },
    ],
    ['claim', () => claimTask(TASK), '/api/tasks/task-1/claim', 'POST', { version: 7 }],
    [
      'reassign',
      () => reassignTask(TASK, 'u2'),
      '/api/tasks/task-1/claim',
      'POST',
      { target_user_id: 'u2', version: 7 },
    ],
    [
      'delegate',
      () => delegateTask(TASK, 'bp-1'),
      '/api/tasks/task-1/delegate',
      'POST',
      { blueprint_id: 'bp-1', version: 7 },
    ],
    [
      'delegate to the default blueprint',
      () => delegateTask(TASK),
      '/api/tasks/task-1/delegate',
      'POST',
      { version: 7 },
    ],
    ['requeue', () => requeueTask(TASK), '/api/tasks/task-1/requeue', 'POST', { version: 7 }],
  ])('%s names the version it read', async (_name, write, path, method, body) => {
    const fetchMock = stubFetch()
    await write()
    expect(sent(fetchMock)).toEqual({ path, method, body })
  })
})

describe('isVersionConflict', () => {
  it("is a 409 whose reason says the task moved on, and nothing else's", () => {
    const refusal = (reason: string) =>
      new HttpError(409, JSON.stringify({ errors: [{ reason, message: 'refused' }] }))
    expect(isVersionConflict(refusal('VERSION_CONFLICT'))).toBe(true)
    expect(isVersionConflict(refusal('ALREADY_TERMINAL'))).toBe(false)
    expect(isVersionConflict(new Error('network'))).toBe(false)
  })
})
