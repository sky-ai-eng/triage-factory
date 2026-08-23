import * as Toast from '@radix-ui/react-toast'
import { X } from 'lucide-react'
import { useNavigate } from 'react-router'
import { toastStore, type ToastLevel } from './toastStore'
import { useToast } from './useToast'
import { useWebSocket } from '../../hooks/useWebSocket'

// Per-level auto-dismiss duration, in milliseconds. Errors stay until the
// user explicitly dismisses — they usually indicate something actionable
// (reconfigure auth, check logs, retry) and shouldn't vanish on their own.
// Warnings linger longer than successes so the user has time to read them
// without feeling flashed.
const LEVEL_DURATION: Record<ToastLevel, number> = {
  info: 3000,
  success: 3000,
  warning: 6000,
  error: 1000 * 60 * 60 * 24, // "effectively sticky" — Radix requires a number
}

// Level-tinted left edge + icon color, descending the severity ladder the
// token set expresses: alarm for failure, warm for caution, ink for the calm
// levels — success one step firmer than info.
const LEVEL_STYLE: Record<ToastLevel, { border: string; label: string }> = {
  info: { border: 'border-l-[var(--color-ink-2)]', label: 'text-[var(--color-ink-2)]' },
  success: { border: 'border-l-[var(--color-ink-1)]', label: 'text-[var(--color-ink-1)]' },
  warning: { border: 'border-l-[var(--color-warm)]', label: 'text-[var(--color-warm)]' },
  error: { border: 'border-l-[var(--color-alarm)]', label: 'text-[var(--color-alarm)]' },
}

export default function ToastProvider() {
  const items = useToast()
  const navigate = useNavigate()
  // Keep the WS singleton connected whenever the provider is mounted, even
  // on pages (like Setup) whose components don't otherwise subscribe. The
  // handler itself is a no-op — toast events are intercepted in the WS
  // layer before listeners fire. This just pins the connection open.
  useWebSocket(() => {})

  return (
    <Toast.Provider swipeDirection="right">
      {items.map((item) => {
        const style = LEVEL_STYLE[item.level]
        return (
          <Toast.Root
            key={item.id}
            duration={LEVEL_DURATION[item.level]}
            onOpenChange={(open) => {
              if (!open) toastStore.dismiss(item.id)
            }}
            className={`
              group relative flex items-start gap-3 pl-4 pr-3 py-3 w-[340px]
              bg-raised/95 backdrop-blur-xl
              border border-line-1 border-l-4 ${style.border}
              rounded-xl shadow-float shadow-black/[0.08]
              data-[state=open]:animate-in data-[state=open]:slide-in-from-right-full
              data-[state=closed]:animate-out data-[state=closed]:fade-out data-[state=closed]:slide-out-to-right-full
              data-[swipe=move]:translate-x-[var(--radix-toast-swipe-move-x)]
              data-[swipe=cancel]:translate-x-0 data-[swipe=cancel]:transition-transform
              data-[swipe=end]:animate-out data-[swipe=end]:slide-out-to-right-full
            `}
          >
            <div className="flex-1 min-w-0">
              {item.title && (
                <Toast.Title
                  className={`text-reported font-semibold uppercase tracking-wide mb-1 ${style.label}`}
                >
                  {item.title}
                </Toast.Title>
              )}
              <Toast.Description className="text-body text-ink-1 leading-snug whitespace-pre-line">
                {item.body}
              </Toast.Description>
              {item.action && (
                <Toast.Action altText={item.action.label} asChild className="mt-2 inline-flex">
                  <button
                    type="button"
                    onClick={() => {
                      navigate(item.action!.to)
                      toastStore.dismiss(item.id)
                    }}
                    className={`text-ui font-semibold underline-offset-2 hover:underline ${style.label}`}
                  >
                    {item.action.label}
                  </button>
                </Toast.Action>
              )}
            </div>
            <Toast.Close
              aria-label="Dismiss"
              className="shrink-0 text-ink-3 hover:text-ink-2 transition-colors p-0.5 -mr-0.5"
            >
              <X size={14} />
            </Toast.Close>
          </Toast.Root>
        )
      })}

      <Toast.Viewport
        className="
          fixed bottom-4 right-4 z-[100]
          flex flex-col gap-2
          w-[340px] max-w-[calc(100vw-2rem)]
          outline-none
        "
      />
    </Toast.Provider>
  )
}
