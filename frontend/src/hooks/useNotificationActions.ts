import { useSyncExternalStore } from 'react'
import { HttpError, httpErrorMessage } from '../lib/apiClient'
import {
  runNotificationAction,
  type NotificationAction,
  type NotificationItem,
} from '../lib/notifications'
import { toast } from '../components/Toast/toastStore'

// The invoke actions this viewer has started, wherever they started them. The
// strip and the notifications page run invokes through this one store, so a
// row's action is out at most once however many places offer it, every place
// that offers it shows it running, and a confirmation is asked in one dialog:
// the shell's. A module store rather than a context, like toastStore: the
// shell and the page it frames hold it alike without either providing it.

/** What the caller learns when the call settles: the row as the viewer now
 *  sees it, `null` once it has left what they may read, `undefined` when the
 *  call failed. The outcome itself is told to the viewer here. */
export type InvokeSettled = (notification: NotificationItem | null | undefined) => void

/** An invoke waiting on the viewer's confirmation, in the org it was asked in. */
export interface PendingInvoke {
  org: string
  item: NotificationItem
  action: NotificationAction
  then?: InvokeSettled
}

export interface NotificationActionsState {
  /** Notification id → the id of its invoke that is out. */
  running: ReadonlyMap<string, string>
  pending: PendingInvoke | null
}

let state: NotificationActionsState = { running: new Map(), pending: null }
const listeners = new Set<() => void>()

function set(next: Partial<NotificationActionsState>) {
  state = { ...state, ...next }
  for (const l of listeners) l()
}

function subscribe(listener: () => void): () => void {
  listeners.add(listener)
  return () => {
    listeners.delete(listener)
  }
}

/** Run an invoke, after the viewer confirms it when it asks for that. A row
 *  whose action is already out takes no second one. */
export function requestInvoke(
  org: string,
  item: NotificationItem,
  action: NotificationAction,
  then?: InvokeSettled,
): void {
  if (state.running.has(item.id)) return
  if (action.confirm) set({ pending: { org, item, action, then } })
  else void run(item, action, then)
}

export function confirmInvoke(): void {
  const p = state.pending
  if (!p) return
  set({ pending: null })
  void run(p.item, p.action, p.then)
}

export function cancelInvoke(): void {
  if (state.pending) set({ pending: null })
}

async function run(item: NotificationItem, action: NotificationAction, then?: InvokeSettled) {
  if (state.running.has(item.id)) return
  set({ running: new Map(state.running).set(item.id, action.id) })
  let settled: NotificationItem | null | undefined
  try {
    const { notification, outcome } = await runNotificationAction(item.id, action.id)
    settled = notification
    // A row still open after its action is a condition that still holds.
    if (!notification) toast.info(outcome)
    else if (notification.resolved_at) toast.success(outcome)
    else toast.warning(outcome)
  } catch (e) {
    const message = httpErrorMessage(e, 'Could not run that action.')
    // 409 is a condition that resolved before the action reached it: news,
    // not a failure.
    if (e instanceof HttpError && e.status === 409) toast.info(message)
    else toast.error(message)
  } finally {
    const next = new Map(state.running)
    next.delete(item.id)
    set({ running: next })
  }
  then?.(settled)
}

/** The row's invoke that is out, if one is. While it is, every invoke the row
 *  offers shows its running label and takes no press. */
export function runningAction(
  item: NotificationItem,
  running: ReadonlyMap<string, string>,
): NotificationAction | undefined {
  const id = running.get(item.id)
  return id === undefined ? undefined : item.actions.find((a) => a.id === id)
}

/** What a running invoke reads as in its own place. */
export function runningLabel(action: NotificationAction): string {
  return action.running_label || action.label
}

export function useNotificationActions(): NotificationActionsState {
  return useSyncExternalStore(subscribe, () => state)
}
