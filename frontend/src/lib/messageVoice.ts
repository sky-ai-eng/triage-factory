import type { Message } from '../types'

// messageVoice — who a transcript row speaks for.
//
// A role=user row is not proof a person typed anything. The backend writes
// several of them on the agent's behalf: a stop note recording why an
// engagement ended (a spend-cap park, a provider error that could not be
// retried, a user pressing stop), a notice that the executor changed, the
// loop's own wrap-up ask, the machine-composed row that replaces a compacted
// span. The subtype is what tells them apart, and it is the same closed set
// the engine's own turn-budget derivation keys on — see agentloop.IsHumanInput:
// a blank subtype, "text" (its legacy spelling), and the mid-work steer stamp
// are a person speaking; every other subtype on a user row is the system.
//
// Rendering the system half as operator speech is not a cosmetic slip. It
// tells the reader they said something they never said, and it puts words in
// their mouth at exactly the moments the machine is explaining itself.

const HUMAN_SUBTYPES = new Set(['', 'text', 'injection:steer'])

/** True when a role=user row records something the system did, not something a person said. */
export function isSystemNotice(msg: Message): boolean {
  return msg.role === 'user' && !HUMAN_SUBTYPES.has(msg.subtype ?? '')
}
