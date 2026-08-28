import { useCallback, useEffect, useRef, useSyncExternalStore } from 'react'
import type { WSEvent } from '../types'
import { toastStore } from '../components/Toast/toastStore'

type Handler = (event: WSEvent) => void

// --- Singleton connection manager ---
// Lives outside React's lifecycle so StrictMode double-mounts and page
// navigations don't tear down the socket.

let globalWs: WebSocket | null = null
const listeners = new Set<Handler>()

// --- Connection state ---
// Whether the shared socket is currently up, published to React through
// useWsConnected below. It lives here for the same reason the socket does: the
// connection outlives every component, so its state cannot be a component's.
//
// It starts TRUE — "nothing has gone wrong yet", not "a socket is open". A
// false here is what the shell renders as "Connection lost · retrying" and
// what makes every live readout go inert, and claiming that before the first
// handshake has had a chance to complete would greet every cold load with a
// failure that is not happening. The first onclose is the first honest false.
let wsConnected = true
const connListeners = new Set<() => void>()

function setWsConnected(next: boolean) {
  if (wsConnected === next) return
  wsConnected = next
  for (const fn of connListeners) fn()
}

// The useSyncExternalStore pair. Both are module-level and stable, which is
// what keeps the subscription from being torn down and rebuilt every render.
function subscribeWsConnected(onChange: () => void): () => void {
  connListeners.add(onChange)
  return () => {
    connListeners.delete(onChange)
  }
}

function getWsConnected(): boolean {
  return wsConnected
}

// --- Auth close-code bridge (TFAC-75) ---
// The server actively closes a socket when the session behind it is
// revoked or the user loses membership in the org it's scoped to. Those
// kicks ride application close codes (4000-4999), which the browser
// surfaces verbatim on the CloseEvent so we can distinguish them from a
// network drop. The socket lives outside React, so AuthContext registers
// a handler here and useWebSocket just forwards the code — keeping this
// module auth-agnostic.
export const WS_CLOSE_SESSION_REVOKED = 4001
export const WS_CLOSE_MEMBERSHIP_CHANGED = 4002

type AuthCloseHandler = (code: number) => void
let authCloseHandler: AuthCloseHandler | null = null
export function setWsAuthCloseHandler(fn: AuthCloseHandler | null) {
  authCloseHandler = fn
}

// --- Event-source availability bridge ---
// The `sources_updated` ping is payload-free and org-scoped, and what it
// invalidates is the availability cache in useEventSources. Registered here
// rather than imported, for the reason the auth bridge above is: this module is
// the socket, and importing the cache would close a cycle through
// useApiOrgId → AuthContext → here. The cache registers itself when it loads,
// which is exactly when there is something stale to drop.
let sourcesInvalidator: (() => void) | null = null
export function setWsSourcesInvalidator(fn: (() => void) | null) {
  sourcesInvalidator = fn
}

// --- Presence (TFAC-392) ---
// We report, over the same socket, whether this tab is on an answer-capable
// surface (the board or a run's detail page) AND focused. The backend uses it
// to fast-deny an unattended permission prompt instead of waiting the full
// timeout. The "viewing" surface is set by the page components (Board /
// RunDetail) via setPresenceView; "visible" tracks the Page Visibility + focus
// state. Both live at module scope so they survive navigations and reconnects —
// the current state is (re)sent on every (re)connect so presence is correct
// after a dropped socket.
type PresenceView = 'board' | `run:${string}` | 'other'

const presence: { viewing: PresenceView; visible: boolean } = {
  viewing: 'other',
  visible: computeVisible(),
}

// computeVisible folds the Page Visibility API and window focus into one
// "answer-capable right now" bit: a backgrounded tab (hidden) or a blurred
// window does not count as present. Guarded for non-browser/test environments
// where document may be undefined.
function computeVisible(): boolean {
  if (typeof document === 'undefined') return false
  return document.visibilityState === 'visible' && document.hasFocus()
}

function sendPresence() {
  if (globalWs && globalWs.readyState === WebSocket.OPEN) {
    globalWs.send(
      JSON.stringify({ type: 'presence', viewing: presence.viewing, visible: presence.visible }),
    )
  }
}

/**
 * setPresenceView records which surface this tab is on and pushes the update to
 * the server. Page components call it on mount ('board' / 'run:<id>') and on
 * unmount ('other'). We always re-send (even for an unchanged value) so a focus
 * change that raced a navigation still lands.
 */
export function setPresenceView(viewing: PresenceView) {
  presence.viewing = viewing
  sendPresence()
}

// Re-evaluate visibility on tab background/foreground and window focus/blur,
// re-sending the current surface so the server's view stays live. Registered
// once at module load (browser only).
if (typeof window !== 'undefined' && typeof document !== 'undefined') {
  const onVisibilityChange = () => {
    presence.visible = computeVisible()
    sendPresence()
  }
  document.addEventListener('visibilitychange', onVisibilityChange)
  window.addEventListener('focus', onVisibilityChange)
  window.addEventListener('blur', onVisibilityChange)
}

// Track per-repo clone_status across WS events so we only fire the
// "clone failed" toast on the *transition* into 'failed', not on every
// repository_updated event carrying the same failed status. Module-level
// (not React state) so the dedupe survives page navigations and the
// short-lived useWebSocket subscriptions on individual pages.
const cloneStatusByRepo = new Map<string, 'ok' | 'failed' | 'pending'>()

function ensureConnected() {
  if (
    globalWs &&
    (globalWs.readyState === WebSocket.OPEN || globalWs.readyState === WebSocket.CONNECTING)
  ) {
    return
  }

  const proto = window.location.protocol === 'https:' ? 'wss:' : 'ws:'
  const ws = new WebSocket(`${proto}//${window.location.host}/api/ws`)

  // Re-send presence on every (re)connect so the server's view of this tab's
  // surface + focus survives a dropped socket. visible is recomputed here in
  // case focus changed while disconnected.
  ws.onopen = () => {
    setWsConnected(true)
    presence.visible = computeVisible()
    sendPresence()
  }

  ws.onmessage = (e) => {
    try {
      const event = JSON.parse(e.data) as WSEvent
      // Global handler: any toast event goes straight into the store, no
      // per-page listener required. Keeps consumers ignorant of WS plumbing.
      if (event.type === 'toast') {
        toastStore.push({
          id: event.data.id,
          level: event.data.level,
          title: event.data.title,
          body: event.data.body,
        })
        return
      }
      // Cross-page clone failure surfacing: when a repo's clone_status
      // transitions to 'failed' on the backend (bootstrap, lazy clone,
      // or import path), fire a sticky error toast with a CTA to the
      // Repos page. Doing it here (rather than in Repos.tsx) means the
      // user sees it even when they're on Board / Settings / Tasks.
      if (event.type === 'repository_updated' && event.data && typeof event.data === 'object') {
        const data = event.data as {
          id?: string
          slug?: string
          clone_status?: 'ok' | 'failed' | 'pending'
          clone_error_kind?: 'ssh' | 'other'
        }
        // Keyed on the row id, worded with the slug — the two jobs the
        // event's two identity fields exist to keep apart. Keying on the
        // name would restart the dedupe on a rename and re-fire a toast
        // for a failure the user already saw.
        if (data.id && data.clone_status) {
          const prev = cloneStatusByRepo.get(data.id)
          cloneStatusByRepo.set(data.id, data.clone_status)
          if (data.clone_status === 'failed' && prev !== 'failed') {
            const kind = data.clone_error_kind === 'ssh' ? ' (SSH)' : ''
            toastStore.push({
              level: 'error',
              title: 'Clone failed',
              body: `Could not clone ${data.slug ?? 'a repository'}${kind}. Open the Repos page for details.`,
              action: { label: 'Go to Repos', to: '/repos' },
            })
          }
        }
      }
      // Event-source availability changed for the org this socket is scoped
      // to — an admin paused or resumed a source, or a credential moved. The
      // ping is payload-free by design (the cheap default): the handler drops
      // the cached answer and whoever is rendering refetches through the REST
      // read, which is what carries the scoping.
      if (event.type === 'sources_updated') {
        sourcesInvalidator?.()
        return
      }
      for (const fn of listeners) {
        fn(event)
      }
    } catch {
      // ignore non-JSON messages (pings, etc.)
    }
  }

  ws.onclose = (e) => {
    globalWs = null
    // A close that reaches here is a disconnect a reader should see: the one
    // deliberate close the app makes — the org switch — detaches this handler
    // first, so it never announces itself. The reconnect below flips the
    // signal back on the next open.
    setWsConnected(false)
    // Session revoked (logout elsewhere, admin kill): hand off to the
    // auth layer to clear state and route to /login, and do NOT
    // reconnect — the cookie is dead, so a retry would 401-loop.
    if (e.code === WS_CLOSE_SESSION_REVOKED) {
      authCloseHandler?.(e.code)
      return
    }
    // Removed from the org this socket was scoped to: refresh the session
    // view (active org may be gone) so the app re-routes, then fall
    // through to the normal reconnect so a still-valid session re-scopes
    // to a remaining org (or the handshake 409s until one is picked).
    if (e.code === WS_CLOSE_MEMBERSHIP_CHANGED) {
      authCloseHandler?.(e.code)
    }
    // Only reconnect if there are still listeners
    if (listeners.size > 0) {
      setTimeout(ensureConnected, 2000)
    }
  }

  globalWs = ws
}

function subscribe(handler: Handler) {
  listeners.add(handler)
  ensureConnected()

  return () => {
    listeners.delete(handler)
    // Don't close — other pages may still need the connection.
    // The socket will naturally stop reconnecting when listeners hits 0.
  }
}

/**
 * Force the singleton websocket to close and re-open so the next
 * handshake captures fresh server-side identity (cookies, session,
 * active_org_id). Called by the org-switcher after `POST
 * /api/me/active-org` lands a 200, so the hub re-registers the
 * connection with the new active org and the per-(user, org) Broadcast
 * filter routes the user to events from the just-selected tenant
 * rather than the previous one.
 *
 * Implementation choices:
 *   - We close() the existing socket explicitly. The onclose handler
 *     already retries via setTimeout when listeners.size > 0; we
 *     short-circuit the wait by calling ensureConnected() right after
 *     so the new socket comes up immediately rather than after the
 *     2s reconnect delay that's tuned for unexpected disconnects.
 *   - cloneStatusByRepo is intentionally NOT cleared: the per-repo
 *     state is keyed by `owner/repo` which is invariant across orgs
 *     in our deployment shape, and re-firing the "clone failed" toast
 *     on a switched org would be noisy.
 *   - No retry/error handling here: a fail-to-reconnect surfaces via
 *     the existing reconnect-on-close loop, same as any other dropped
 *     connection.
 *
 * Multi-tab note: each tab
 * holds its own globalWs reference, so reconnect only affects the
 * tab that initiated the switch. Other tabs continue to receive
 * events for their previously-selected org until the user navigates
 * or they pick up the new active-org from the session on the next
 * /api/me read.
 */
export function reconnectWebSocket() {
  const existing = globalWs
  globalWs = null
  if (existing && existing.readyState !== WebSocket.CLOSED) {
    // Detach the close handler BEFORE close(). The old socket's
    // onclose runs later (after the TCP close completes) and would
    // otherwise null out globalWs — but by then globalWs points at
    // the NEW socket from ensureConnected() below, dropping its
    // reference and triggering a second reconnect that opens a
    // duplicate /api/ws connection. The 'org switch' close is
    // intentional, so we don't need the auto-reconnect path that
    // onclose triggers anyway.
    existing.onclose = null
    // Use a normal close (1000) so the server doesn't log this as an
    // abnormal disconnect. The new socket spins up below regardless
    // of close-time.
    try {
      existing.close(1000, 'org switch')
    } catch {
      // close() can throw if the socket is in CONNECTING — harmless,
      // ensureConnected below will pick up the slack.
    }
  }
  if (listeners.size > 0) {
    ensureConnected()
  }
}

// --- React hook ---

export function useWebSocket(handler: Handler) {
  // Latest-ref pattern: keep a mutable reference to the freshest handler
  // closure so the stable wrapper below always dispatches to it, without
  // having to re-subscribe on every render. The assignment lives in an
  // effect (not inline during render) per react-hooks/refs.
  const handlerRef = useRef(handler)
  useEffect(() => {
    handlerRef.current = handler
  }, [handler])

  // Stable wrapper so the subscription identity doesn't change on re-renders
  const stableHandler = useCallback((event: WSEvent) => {
    handlerRef.current(event)
  }, [])

  useEffect(() => {
    return subscribe(stableHandler)
  }, [stableHandler])
}

/**
 * useWsConnected reports whether the shared websocket is up.
 *
 * The shell renders `!connected` as offline: live readouts go inert rather
 * than holding their last value, and the condition is stated once at the foot.
 * That is the honest reading for a UI whose freshness is push-driven — with no
 * stream there is no next hint, so a count on screen is a number nobody is
 * maintaining.
 *
 * It does NOT open a socket. It reports on the one the app's event listeners
 * keep alive, so a page with no useWebSocket subscriber reads the last known
 * state rather than a connection it caused.
 */
export function useWsConnected(): boolean {
  // useSyncExternalStore rather than an effect + setState: the connection IS an
  // external store, and this is the API that reads one without a render pass in
  // which the component holds a value the store has already moved past.
  return useSyncExternalStore(subscribeWsConnected, getWsConnected, getWsConnected)
}
