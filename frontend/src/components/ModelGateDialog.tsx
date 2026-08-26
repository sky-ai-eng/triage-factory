// The one dialog behind every paid model probe: the save gate and the eager
// post-connect sweep. Mounted once, beside the toaster, because its callers are
// a settings section's Save, a wizard step's persist and a credential bind's
// aftermath — and only one of those is a component that could host a modal.
//
// It never opens itself. Something awaits gateModelSave / offerModelSweep, this
// renders what that put on the store, and pressing through is what spends the
// request.

import { useEffect, useRef, useState } from 'react'
import { AnimatePresence, motion } from 'motion/react'
import { useFocusTrap } from '../hooks/useFocusTrap'
import {
  modelGate,
  runSaveTest,
  runSweep,
  sweepCostSentence,
  type GateAttempt,
  type ModelGateRequest,
} from '../lib/modelGate'
import { probeCostSentence } from '../lib/models'

export default function ModelGateDialog() {
  const [request, setRequest] = useState<ModelGateRequest | null>(modelGate.current())
  useEffect(() => modelGate.subscribe(() => setRequest(modelGate.current())), [])

  return (
    <AnimatePresence>
      {request && (
        // Keyed on the request so a second gate — a different model, or the
        // sweep after a rebind — opens with its own phase rather than inheriting
        // the last one's outcome.
        <GateBody key={gateKey(request)} request={request} />
      )}
    </AnimatePresence>
  )
}

function gateKey(request: ModelGateRequest): string {
  return request.kind === 'save' ? `save:${request.model.key}` : `sweep:${request.provider}`
}

// Where the dialog is in the one flow it runs: ask → (spending) → done, or the
// two ways it stops. `blocked` is a settled no — the save may not proceed and
// no repeat of this press would change that. `transient` established nothing
// and is worth pressing again.
type Phase = 'ask' | 'spending' | 'blocked' | 'transient'

function GateBody({ request }: { request: ModelGateRequest }) {
  const [phase, setPhase] = useState<Phase>('ask')
  const [detail, setDetail] = useState('')
  const dialogRef = useRef<HTMLDivElement>(null)
  const cancelRef = useRef<HTMLButtonElement>(null)
  const busy = phase === 'spending'

  // Focus starts on the way out (WCAG 2.1.2 for the trap; the non-spending
  // default for what to press first), because the other button spends money.
  useFocusTrap(dialogRef, { initialFocus: cancelRef })

  const close = (proceeded: boolean) => modelGate.settle(proceeded)

  useEffect(() => {
    const onKey = (e: KeyboardEvent) => {
      if (e.key === 'Escape' && !busy) close(false)
    }
    window.addEventListener('keydown', onKey)
    return () => window.removeEventListener('keydown', onKey)
  }, [busy])

  const run = async () => {
    setPhase('spending')
    setDetail('')
    const attempt: GateAttempt =
      request.kind === 'save' ? await runSaveTest(request) : await runSweep(request)
    if (attempt.status === 'green') {
      close(true)
      return
    }
    setDetail(attempt.detail)
    setPhase(attempt.status)
  }

  const copy = request.kind === 'save' ? saveCopy(request) : sweepCopy(request)

  return (
    <>
      <motion.div
        className="fixed inset-0 z-[60] bg-scrim backdrop-blur-sm"
        initial={{ opacity: 0 }}
        animate={{ opacity: 1 }}
        exit={{ opacity: 0 }}
        onClick={() => !busy && close(false)}
      />
      <motion.div
        ref={dialogRef}
        tabIndex={-1}
        role="alertdialog"
        aria-modal="true"
        aria-labelledby="model-gate-title"
        aria-describedby="model-gate-body"
        className="fixed left-1/2 top-1/2 z-[60] w-[min(460px,calc(100vw-2rem))] -translate-x-1/2 -translate-y-1/2 rounded-2xl border border-line-1 bg-ground/95 p-5 shadow-float shadow-black/[0.12] backdrop-blur-2xl focus:outline-none"
        initial={{ opacity: 0, scale: 0.96, y: 8 }}
        animate={{ opacity: 1, scale: 1, y: 0 }}
        exit={{ opacity: 0, scale: 0.96, y: 8 }}
        transition={{ type: 'spring', damping: 28, stiffness: 360 }}
        onClick={(e) => e.stopPropagation()}
      >
        <h2 id="model-gate-title" className="text-body font-semibold tracking-tight text-ink-1">
          {phase === 'blocked' ? copy.blockedTitle : copy.title}
        </h2>
        <p id="model-gate-body" className="mt-2 text-card-title leading-relaxed text-ink-2">
          {phase === 'ask' || phase === 'spending' ? copy.body : detail}
        </p>
        {phase === 'transient' && (
          <p className="mt-2 text-reported text-ink-3">
            Nothing was recorded, so nothing changed. Try again when you&rsquo;re ready.
          </p>
        )}
        <div className="mt-5 flex items-center justify-end gap-2">
          <button
            ref={cancelRef}
            type="button"
            onClick={() => close(false)}
            disabled={busy}
            className="rounded-[5px] px-3 py-1.5 text-ui font-medium text-ink-3 transition-colors hover:bg-tint-3 hover:text-ink-2 disabled:opacity-50"
          >
            {phase === 'blocked' ? 'Close' : copy.cancelLabel}
          </button>
          {phase !== 'blocked' && (
            <button
              type="button"
              onClick={() => void run()}
              disabled={busy}
              className="rounded-[5px] bg-warm px-3 py-1.5 text-ui font-semibold text-ground transition-opacity hover:opacity-90 disabled:cursor-wait disabled:opacity-60"
            >
              {busy ? 'Testing…' : phase === 'transient' ? 'Try again' : copy.confirmLabel}
            </button>
          )}
        </div>
      </motion.div>
    </>
  )
}

interface GateCopy {
  title: string
  blockedTitle: string
  body: string
  cancelLabel: string
  confirmLabel: string
}

function saveCopy(request: Extract<ModelGateRequest, { kind: 'save' }>): GateCopy {
  const name = request.model.display_name
  return {
    title: `Test ${name} before saving?`,
    blockedTitle: `${name} can’t be used`,
    body: `Nothing has established that your credentials can run ${name}. ${probeCostSentence(
      request.model,
    )} The setting is saved only if it answers.`,
    cancelLabel: 'Cancel',
    confirmLabel: 'Test and save',
  }
}

function sweepCopy(request: Extract<ModelGateRequest, { kind: 'sweep' }>): GateCopy {
  const n = request.candidates.length
  return {
    title: `Test ${n} ${n === 1 ? 'model' : 'models'} against ${request.providerLabel}?`,
    blockedTitle: 'The tests could not be run',
    // Declining has to read as a real option, because it is one: the save gate
    // catches each untested model the first time somebody picks it.
    body: `${sweepCostSentence(
      request.candidates,
    )} You can skip this — anything left untested is tested the first time somebody saves it.`,
    cancelLabel: 'Not now',
    confirmLabel: `Test ${n} ${n === 1 ? 'model' : 'models'}`,
  }
}
