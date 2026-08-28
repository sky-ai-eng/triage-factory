import { describe, it, expect, vi, beforeEach } from 'vitest'
import { renderHook, act } from '@testing-library/react'
import { useWebSocket, useWsConnected } from './useWebSocket'

// The connection signal the shell renders as `offline`. The socket is a module
// singleton, so the test drives a fake WebSocket class rather than the hook:
// what is under test is that a real open/close reaches React at all.

class FakeSocket {
  static last: FakeSocket | null = null
  onopen: (() => void) | null = null
  onclose: ((e: { code: number }) => void) | null = null
  onmessage: ((e: { data: string }) => void) | null = null
  readyState = 0
  constructor() {
    FakeSocket.last = this
  }
  send() {}
  close() {}
}

beforeEach(() => {
  FakeSocket.last = null
  vi.stubGlobal('WebSocket', FakeSocket as unknown as typeof WebSocket)
})

/** Mounts a subscriber alongside the signal — the signal reports on the socket
 *  the app's listeners keep alive and never opens one itself. */
function mountRail() {
  return renderHook(() => {
    useWebSocket(() => {})
    return useWsConnected()
  })
}

describe('useWsConnected', () => {
  it('claims nothing is wrong before the first handshake resolves', () => {
    const { result } = mountRail()
    // Not "a socket is open" — "nothing has gone wrong yet". Greeting a cold
    // load with "Connection lost" would announce a failure that is not
    // happening, and inert every readout for the length of one handshake.
    expect(result.current).toBe(true)
  })

  it('goes offline when the socket drops and comes back when it reopens', () => {
    const { result } = mountRail()
    const socket = FakeSocket.last!

    act(() => {
      socket.onclose?.({ code: 1006 })
    })
    expect(result.current).toBe(false)

    // Recovery is an open, whichever socket delivers it — the reconnect
    // loop's next one in production, this one here.
    act(() => {
      socket.onopen?.()
    })
    expect(result.current).toBe(true)
  })
})
