import { ToastLog } from '../../ui/toastlog/ToastLog'
import { toastStore } from './toastStore'
import { useToast } from './useToast'
import { useWebSocket } from '../../hooks/useWebSocket'

// Lifetimes, pausing, eviction and motion all live in ToastLog — see
// src/ui/toastlog/ToastLog.md. The store removes a row only at onDone, once
// its exit has finished.
export default function ToastProvider() {
  const items = useToast()
  // Keep the WS singleton connected whenever the provider is mounted, even
  // on pages (like Setup) whose components don't otherwise subscribe. The
  // handler itself is a no-op — toast events are intercepted in the WS
  // layer before listeners fire. This just pins the connection open.
  useWebSocket(() => {})

  return <ToastLog items={items} onDone={toastStore.dismiss} />
}
