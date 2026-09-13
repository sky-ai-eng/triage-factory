import { Dialog } from '../../ui/dialog/Dialog'

// RequeueConfirm gates returning a task to the queue while a run is in
// flight, and it is the only confirmation the gesture has: with nothing
// running the move is reversible by re-claiming, so the drop fires without a
// word. The panel arrives already built because the question is routine, and
// the verb is a press rather than a hold — a hold is what this system spends
// on what cannot be taken back, and a requeued task can simply be delegated
// again. The destructive keyboard rules still apply (focus lands on Cancel,
// Enter does not confirm), the same as every other destructive dialog here.
export default function RequeueConfirm({
  open,
  onConfirm,
  onCancel,
}: {
  open: boolean
  onConfirm: () => void
  onCancel: () => void
}) {
  return (
    <Dialog
      open={open}
      kind="destructive"
      build="none"
      title="Return this run to the queue?"
      body="This stops the run. Its work stays with the task."
      confirmLabel="Return to queue"
      cancelLabel="Cancel"
      onConfirm={onConfirm}
      onCancel={onCancel}
    />
  )
}
