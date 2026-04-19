import { useEffect, useState } from 'react'
import { toastStore, type ToastItem } from './toastStore'

// useToast subscribes a component to the current toast list. Most callers
// don't need this — firing a toast is a side effect via the imported `toast`
// object from toastStore. This hook is for components that want to render
// current toasts themselves (only ToastProvider does today).
export function useToast(): ToastItem[] {
  const [items, setItems] = useState<ToastItem[]>(toastStore.getState())
  useEffect(() => toastStore.subscribe(setItems), [])
  return items
}

// Re-export the fire helpers so consumers can import from one place.
export { toast } from './toastStore'
export type { ToastLevel, ToastItem } from './toastStore'
