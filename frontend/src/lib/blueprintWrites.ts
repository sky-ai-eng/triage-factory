import type { BlueprintStep } from '../types'
import { apiErrors, apiFetch } from './apiClient'
import { blueprintsBase } from './scope'

/** A blueprint as a split or a reconnect names it: its id, and the
 *  blueprint_version its steps were read at. The step index a cut sends is a
 *  position in those steps, so the server refuses a cut whose blueprint has
 *  changed since with 409 VERSION_CONFLICT rather than cut next to a step the
 *  caller never chose. */
export interface BlueprintStepsRef {
  id: string
  version: number
}

const json = { 'Content-Type': 'application/json' }

/** The version a cut on one blueprint names, from that blueprint's steps as
 *  read: the lowest any of them carries. The canvas reads every step a page
 *  at a time, so one blueprint's steps can come from two reads with a write
 *  between them, and only the older version is refused if that happened. */
export function stepsVersion(steps: BlueprintStep[]): number {
  return Math.min(...steps.map((s) => s.blueprint_version ?? 0))
}

/** Splits the blueprint before step atStepIndex: the steps before it stay,
 *  and the rest move to a new blueprint no event handler fires. */
export async function splitBlueprint(bp: BlueprintStepsRef, atStepIndex: number): Promise<void> {
  await apiFetch(`${blueprintsBase()}/${encodeURIComponent(bp.id)}/split`, {
    method: 'POST',
    headers: json,
    body: JSON.stringify({ at_step_index: atStepIndex, version: bp.version }),
  })
}

/** Re-points the boundary before step atStepIndex onto the entry of
 *  targetBlueprintId: the steps before it continue into the target's steps,
 *  and the rest move to a new blueprint no event handler fires. The target is
 *  absorbed whole, and named by id alone. */
export async function reconnectBlueprint(
  bp: BlueprintStepsRef,
  atStepIndex: number,
  targetBlueprintId: string,
): Promise<void> {
  await apiFetch(`${blueprintsBase()}/${encodeURIComponent(bp.id)}/reconnect`, {
    method: 'POST',
    headers: json,
    body: JSON.stringify({
      at_step_index: atStepIndex,
      target_blueprint_id: targetBlueprintId,
      version: bp.version,
    }),
  })
}

/** Whether a failed split or reconnect was refused because the blueprint's
 *  steps changed since the caller read them. Its index named a position in
 *  steps the blueprint no longer has: read them again before offering the
 *  gesture, or the retry is refused the same way. */
export function isStaleSteps(err: unknown): boolean {
  return apiErrors(err).some((item) => item.reason === 'VERSION_CONFLICT')
}
