// The consent layer over the two paid model probes.
//
// NO PROBE EVER RUNS WITHOUT ONE OF EXACTLY TWO EXPLICIT CONSENTS: the save
// gate, when somebody saves a selection nothing has established yet, and the
// eager sweep offered once, right after a provider credential is connected.
// Both are dialogs a person presses through, and both state what the request
// costs in the terms of the transport that will make it. Nothing here fires on
// a timer, a mount, or a read.
//
// It is an in-module store rather than a hook — the same shape as toastStore,
// and for the same reason: the callers are a settings section's Save, a wizard
// step's persist and a credential bind's aftermath, and only one of the three
// is a component that could hold state. Each awaits `gateModelSave` /
// `offerModelSweep` and gets back a plain boolean; the single mounted host
// renders whatever is pending.

import { toast } from '../components/Toast/toastStore'
import { modelCatalogRows, refreshModelCatalog } from '../hooks/useModelCatalog'
import { httpErrorMessage } from './apiClient'
import {
  availabilityHelp,
  needsTestBeforeSave,
  sweepModelTests,
  testModel,
  type ModelCatalogEntry,
} from './models'

/** A save waiting on one model's verdict. */
export interface ModelSaveGateRequest {
  kind: 'save'
  orgId: string
  model: ModelCatalogEntry
}

/** The eager pass over one provider's untested models. */
export interface ModelSweepRequest {
  kind: 'sweep'
  orgId: string
  provider: string
  providerLabel: string
  /** The models this sweep will spend on — the provider's rows that are not
   *  already verified. The count is the caller's, read off the same catalog the
   *  sweep walks, so the dialog can say how many requests it is about to make
   *  without a second endpoint answering the same question. */
  candidates: ModelCatalogEntry[]
}

export type ModelGateRequest = ModelSaveGateRequest | ModelSweepRequest

type Pending = ModelGateRequest & { settle: (proceeded: boolean) => void }

let pending: Pending | null = null
const listeners = new Set<() => void>()

function emit() {
  for (const l of listeners) l()
}

export const modelGate = {
  /** current is what the host should be rendering, or null. */
  current: (): ModelGateRequest | null => pending,
  subscribe(listener: () => void): () => void {
    listeners.add(listener)
    return () => {
      listeners.delete(listener)
    }
  },
  /** settle closes whatever is open and hands the awaiting caller its answer.
   *  The host calls it; nothing else should. */
  settle(proceeded: boolean) {
    const open = pending
    pending = null
    emit()
    open?.settle(proceeded)
  },
}

function open(req: ModelGateRequest): Promise<boolean> {
  if (pending) {
    // One dialog at a time. A second request while one is open is a double
    // press or two sections saving at once; refusing it leaves the open dialog
    // — and the decision the person is already making — untouched.
    return Promise.resolve(false)
  }
  return new Promise<boolean>((resolve) => {
    pending = { ...req, settle: resolve }
    emit()
  })
}

/**
 * gateModelSave decides whether a save carrying this model may proceed, asking
 * the person first when the answer costs money to learn.
 *
 * Four answers, and only one of them is a dialog:
 *   - no availability field → PROCEED. Nothing is stored about this row and no
 *     probe can be, so there is no gate to engage.
 *   - verified → PROCEED. A single green is permanent; re-testing would spend
 *     to re-learn it.
 *   - red / unconfigured → REFUSE, naming the fix. The picker does not offer
 *     these, so reaching here is a stale catalog rather than a choice — the
 *     save would be refused by the server anyway, and this says why.
 *   - unverified → ask, test, and proceed only on green.
 */
export function gateModelSave(orgId: string, model: ModelCatalogEntry): Promise<boolean> {
  if (!needsTestBeforeSave(model)) {
    if (model.availability === 'red' || model.availability === 'unconfigured') {
      toast.error(`${model.display_name}: ${availabilityHelp(model)}`)
      return Promise.resolve(false)
    }
    return Promise.resolve(true)
  }
  return open({ kind: 'save', orgId, model })
}

/**
 * offerModelSweep offers the eager pass after a provider credential is
 * connected, and resolves once the person has answered either way.
 *
 * DECLINING IS ALLOWED and costs nothing later: the rows stay untested and the
 * save gate catches each one individually the first time somebody picks it. An
 * offer with no way out would be a probe that runs itself.
 */
export function offerModelSweep(
  orgId: string,
  provider: string,
  providerLabel: string,
  candidates: ModelCatalogEntry[],
): Promise<boolean> {
  if (candidates.length === 0) return Promise.resolve(false)
  return open({ kind: 'sweep', orgId, provider, providerLabel, candidates })
}

/**
 * offerSweepAfterConnect is the aftermath of binding a provider credential: the
 * one moment an eager pass over that provider's models is offered.
 *
 * The catalog is re-read first, because the bind is what every availability on
 * it derives from — before it those rows were "not connected", and a sweep
 * counted off the stale list would offer to test nothing.
 *
 * It resolves whether or not the person accepts, and a decline is a real
 * answer: the rows stay untested and each is gated the first time somebody
 * saves it. A deployment with nothing to test — no TF-owned credential for a
 * verdict to be about — offers nothing at all, which needs no mode branch
 * because such rows carry no availability field to be a candidate on.
 */
export async function offerSweepAfterConnect(
  orgId: string,
  provider: string,
  providerLabel: string,
): Promise<void> {
  await refreshModelCatalog(orgId)
  await offerModelSweep(
    orgId,
    provider,
    providerLabel,
    sweepCandidates(modelCatalogRows(orgId), provider),
  )
}

/** sweepCandidates is the set a sweep of `provider` would spend on, read off the
 *  catalog: that provider's rows, minus the ones already verified. It is the
 *  same rule the endpoint applies, which is why the dialog can state the count
 *  without asking the server to count for it.
 *
 *  A row that names no provider belongs to whichever credential the org bound,
 *  so it is a candidate for the provider being connected — the harness resolves
 *  the path, and the sweep records what THAT credential can invoke. */
export function sweepCandidates(
  models: ModelCatalogEntry[],
  provider: string,
): ModelCatalogEntry[] {
  return models.filter(
    (m) =>
      (m.provider ?? provider) === provider &&
      m.availability !== undefined &&
      m.availability !== 'verified',
  )
}

/** The outcome of one attempt inside the dialog. `blocked` carries the reason
 *  the save may not proceed; `transient` carries what went wrong and invites a
 *  retry, having stored nothing. */
export type GateAttempt =
  | { status: 'green' }
  | { status: 'blocked'; detail: string }
  | { status: 'transient'; detail: string }

/** runSaveTest spends the probe behind the save gate. Exported for the host,
 *  which owns the dialog's phases but not what a verdict means. */
export async function runSaveTest(req: ModelSaveGateRequest): Promise<GateAttempt> {
  try {
    const result = await testModel(req.orgId, req.model.key)
    await refreshModelCatalog(req.orgId)
    switch (result.outcome) {
      case 'verified':
        return { status: 'green' }
      case 'red':
        return {
          status: 'blocked',
          detail:
            result.detail ||
            'Your provider refused this model. Fix it with your provider, then test again.',
        }
      default:
        // Inconclusive, and anything a later build might add: nothing was
        // stored, so the state is exactly what it was and pressing again is the
        // whole remedy.
        return {
          status: 'transient',
          detail: result.detail || 'Nobody answered. Nothing was recorded — try again.',
        }
    }
  } catch (e) {
    return { status: 'transient', detail: httpErrorMessage(e, 'The test could not be run.') }
  }
}

/** runSweep spends the eager pass and reports what it learned, as the sentence
 *  the aftermath toast carries. Every count is a filter over the items the
 *  endpoint returned — it publishes no summary of its own, and a second
 *  representation of the same numbers is one more thing that can disagree. */
export async function runSweep(req: ModelSweepRequest): Promise<GateAttempt> {
  try {
    const sweep = await sweepModelTests(req.orgId, req.provider)
    await refreshModelCatalog(req.orgId)
    const count = (outcome: string) => sweep.items.filter((i) => i.outcome === outcome).length
    const verified = count('verified')
    const red = count('red')
    const inconclusive = count('inconclusive')
    const parts = [`${verified} verified`]
    if (red > 0) parts.push(`${red} unavailable`)
    if (inconclusive > 0) parts.push(`${inconclusive} unanswered`)
    toast.success(`${req.providerLabel}: ${parts.join(', ')}.`)
    return { status: 'green' }
  } catch (e) {
    return { status: 'transient', detail: httpErrorMessage(e, 'The tests could not be run.') }
  }
}

/** sweepCostSentence states what the whole pass spends. It reads the candidates
 *  rather than the provider: a row carrying prices is probed over the direct
 *  API, and one whose cost the harness settles is probed through the harness. */
export function sweepCostSentence(candidates: ModelCatalogEntry[]): string {
  const perProbe = candidates.some((m) => !m.prices_per_mtok)
    ? 'Each one is a real run of the agent harness against your credentials.'
    : 'Each one is a real request against your credentials — about two tokens.'
  return perProbe
}

/** resetModelGateForTest drops anything pending. Test-only. */
export function resetModelGateForTest(): void {
  pending = null
  listeners.clear()
}
