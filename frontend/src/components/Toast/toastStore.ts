// A tiny in-module store for toast notifications — deliberately NOT a React
// hook, so non-React callers (like the websocket dispatch path) can fire a
// toast without hook ceremony. Components subscribe via useToast() when they
// need live state; most callers just do `toast.error("...")` and move on.

import type { ToastLevel, ToastLogAction, ToastLogItem } from '../../ui/toastlog/ToastLog'

export type { ToastLevel }

// The store holds exactly what ToastLog draws, so the two cannot drift.
export type ToastAction = ToastLogAction
export type ToastItem = ToastLogItem

type Listener = (items: ToastItem[]) => void

const MAX_TOASTS = 100

let state: ToastItem[] = []
const listeners = new Set<Listener>()

function emit() {
  for (const l of listeners) l(state)
}

function nextID(): string {
  // crypto.randomUUID is stable across our target browsers; the fallback
  // keeps storybook/jsdom environments without crypto happy.
  if (typeof crypto !== 'undefined' && crypto.randomUUID) {
    return crypto.randomUUID()
  }
  return `toast-${Date.now()}-${Math.random().toString(36).slice(2, 10)}`
}

// `at` is stamped here when the caller has none, which is every caller today:
// the websocket payload carries no time, and the row draws it as HH:MM.
function push(item: Omit<ToastItem, 'id' | 'at'> & { id?: string; at?: number }): string {
  const id = item.id ?? nextID()
  // Skip-on-duplicate-ID guards against rendering the same payload twice
  // (e.g. a future WS replay-on-reconnect emitting an already-shown toast).
  // It is NOT a mechanism to collapse recurring conditions — callers don't
  // currently pass stable ids, so each Fire gets a fresh UUID and every
  // call creates a new toast. If you want "one toast per auth-expired
  // poll cycle", rate-limit at the emit site instead.
  if (state.some((t) => t.id === id)) return id
  const nextState = [...state, { ...item, id, at: item.at ?? Date.now() }]
  state = nextState.length > MAX_TOASTS ? nextState.slice(-MAX_TOASTS) : nextState
  emit()
  return id
}

function dismiss(id: string) {
  state = state.filter((t) => t.id !== id)
  emit()
}

function subscribe(listener: Listener): () => void {
  listeners.add(listener)
  listener(state)
  return () => {
    listeners.delete(listener)
  }
}

export const toastStore = {
  push,
  dismiss,
  subscribe,
  getState: () => state,
}

type Opts = { action?: ToastAction }

// Convenience API — the 99% surface. Body first because most toasts are
// body-only; the optional title is second, and the rare navigation action
// rides in the third.
export const toast = {
  info: (body: string, title?: string, opts?: Opts) =>
    push({ level: 'info', body, title, ...opts }),
  success: (body: string, title?: string, opts?: Opts) =>
    push({ level: 'success', body, title, ...opts }),
  warning: (body: string, title?: string, opts?: Opts) =>
    push({ level: 'warning', body, title, ...opts }),
  error: (body: string, title?: string, opts?: Opts) =>
    push({ level: 'error', body, title, ...opts }),
  dismiss,
  push,
}
