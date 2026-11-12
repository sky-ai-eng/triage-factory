import type { Task } from '../types'
import { apiErrors, apiJSON } from './apiClient'

/** A task as a write names it: its id, and the version the caller's read of it
 *  returned. Every task write sends the version, and the server refuses one
 *  whose task has changed since with 409 VERSION_CONFLICT, so a stale card, a
 *  board that missed a refresh, or an undo toast left open cannot overwrite a
 *  change the caller never saw. */
export type TaskRef = Pick<Task, 'id' | 'version'>

/** The response of POST /api/tasks/{id}/delegate: the status and version the
 *  handoff left, and the conversation the new run is followed through, when
 *  the server could name it. */
export interface DelegateResult {
  status: string
  version: number
  conversation_id?: string
}

const json = { 'Content-Type': 'application/json' }

function taskPath(task: TaskRef, suffix = ''): string {
  return `/api/tasks/${encodeURIComponent(task.id)}${suffix}`
}

/** Closes the task (done, dismissed) or re-asserts the caller's in_progress
 *  stage. Answers the task as the write left it. */
export function patchTaskStatus(
  task: TaskRef,
  status: 'done' | 'dismissed' | 'in_progress',
): Promise<Task> {
  return apiJSON<Task>(taskPath(task), {
    method: 'PATCH',
    headers: json,
    body: JSON.stringify({ status, version: task.version }),
  })
}

/** Parks the task until `until`, an RFC3339 instant (see `snoozeUntilFromPreset`). */
export function snoozeTask(task: TaskRef, until: string): Promise<Task> {
  return apiJSON<Task>(taskPath(task), {
    method: 'PATCH',
    headers: json,
    body: JSON.stringify({ snooze_until: until, version: task.version }),
  })
}

/** Claims the task for the caller. */
export function claimTask(task: TaskRef): Promise<Task> {
  return apiJSON<Task>(taskPath(task, '/claim'), {
    method: 'POST',
    headers: json,
    body: JSON.stringify({ version: task.version }),
  })
}

/** Hands the caller's (or, for a team admin, anyone's) claim to another user. */
export function reassignTask(task: TaskRef, targetUserID: string): Promise<Task> {
  return apiJSON<Task>(taskPath(task, '/claim'), {
    method: 'POST',
    headers: json,
    body: JSON.stringify({ target_user_id: targetUserID, version: task.version }),
  })
}

/** Hands the task to the agent and starts its run. Without a blueprint the
 *  server runs the team's default. */
export function delegateTask(task: TaskRef, blueprintID?: string): Promise<DelegateResult> {
  return apiJSON<DelegateResult>(taskPath(task, '/delegate'), {
    method: 'POST',
    headers: json,
    body: JSON.stringify({
      ...(blueprintID && { blueprint_id: blueprintID }),
      version: task.version,
    }),
  })
}

/** Returns the task to the queue, stopping whatever run was working it. */
export function requeueTask(task: TaskRef): Promise<Task> {
  return apiJSON<Task>(taskPath(task, '/requeue'), {
    method: 'POST',
    headers: json,
    body: JSON.stringify({ version: task.version }),
  })
}

/** Whether a failed task write was refused because the task changed since the
 *  caller read it. The caller's copy is stale: re-read it before offering the
 *  gesture again, or the retry is refused the same way. */
export function isVersionConflict(err: unknown): boolean {
  return apiErrors(err).some((item) => item.reason === 'VERSION_CONFLICT')
}
