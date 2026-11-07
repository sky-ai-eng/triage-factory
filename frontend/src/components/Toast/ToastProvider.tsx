import { useMemo } from 'react'
import { useNavigate } from 'react-router'
import { ToastLog } from '../../ui/toastlog/ToastLog'
import { toastStore } from './toastStore'
import { useToast } from './useToast'
import { useWebSocket } from '../../hooks/useWebSocket'

// Lifetimes, pausing, eviction and motion all live in ToastLog — see
// src/ui/toastlog/ToastLog.md. The store removes a row only at onDone, once
// its exit has finished.
export default function ToastProvider() {
  const items = useToast()
  const navigate = useNavigate()
  // Keep the WS singleton connected whenever the provider is mounted, even
  // on pages (like Setup) whose components don't otherwise subscribe. The
  // handler itself is a no-op — toast events are intercepted in the WS
  // layer before listeners fire. This just pins the connection open.
  useWebSocket(() => {})

  // The store sits outside React, so an action pushed with only an in-app
  // href is given the router's navigation here; without it a click would
  // reload the app.
  const routed = useMemo(
    () =>
      items.map((t) => {
        const a = t.action
        if (!a || a.onSelect || !a.href.startsWith('/')) return t
        return { ...t, action: { ...a, onSelect: () => navigate(a.href) } }
      }),
    [items, navigate],
  )

  return <ToastLog items={routed} onDone={toastStore.dismiss} />
}
