package db

import "errors"

// ErrClaimReleased is the fence trip: an executor engagement tried to write
// against a claim that is no longer live, so the write was refused.
//
// The claim is the executor's title to a conversation. Ownership is meant to
// end cooperatively — an executor that loses contact with the database
// self-fences (stops its own sandboxes) strictly before the fleet reaper is
// allowed to release its claims and requeue the work, so a successor never
// starts while the predecessor is still running. That guarantee is
// process-level and clock-based, and the failure modes it does not cover (a
// stalled process, a clock that jumps, a blocking call that starves the
// deadline check) all end the same way: a zombie still holding a dead claim,
// writing into a conversation a successor now drives. Interleaved transcript,
// spend settled onto the wrong engagement, a terminal status landing on
// in-flight work — none of it is repairable after the fact, because two
// authors in one window cannot be told apart.
//
// So the claim-fenced writes refuse instead. Each validates the named claim
// with a locking read in its own transaction, which conflicts with the
// release, so a concurrent release either waits for the write to commit or
// the write sees the release and returns this error. The two outcomes are
// exactly the two honest answers; there is no window between them.
//
// A caller that gets this back has one correct reaction: stop. It is not the
// owner, so it must not write a terminal status, record a result, or release
// anything — whoever released the claim owns the conversation's disposition
// now. Killing its own sandbox and returning silently is the whole
// obligation. A refusal with no deliberate stop behind it is worth logging at
// error level wherever it surfaces: in production it means the cooperative
// fence failed, which is an incident, not noise.
//
// Both dialects fence. The successor executor is the multi-mode rival, but
// there is a second one every mode has: the stop verb. A person stopping a
// conversation parks the row and releases its claim without consulting the
// engagement — deliberately, since the whole point of a stop is to override
// whichever executor holds the run — and an engagement still bringing its
// runtime up when that lands reaches its next write as exactly the zombie
// described above. Local mode has no reaper and no second executor, so the
// stop verb is the only thing that can trip its fence, and the refusal it
// gets is the ordinary shape of every stop that catches a run mid-setup.
var ErrClaimReleased = errors.New("db: claim released — this engagement no longer owns the conversation")
