package db

import "errors"

// ErrClaimReleased is the fence trip: an executor engagement tried to write
// against a claim that is no longer live, so the write was refused.
//
// The claim is the executor's title to a conversation, and that title is a
// LEASE on database time: it carries an expiry, its holder renews it on a
// timer, and every write the holder makes presents it. Authority ends at the
// expiry whether or not a successor exists — nobody has to arrive for a dead
// engagement's writes to stop being accepted. A holder that cannot renew
// fences its own engagement (kills its cell, writes nothing more) on its own
// monotonic clock, strictly before the lease it can no longer prove lapses,
// so the cell is dead before anyone may take the conversation over.
//
// What that buys is a guarantee per claim rather than per process. A zombie
// still holding a dead claim — a stalled process, a clock that jumped, a
// blocking call that starved a deadline check — writing into a conversation a
// successor now drives is not repairable after the fact: interleaved
// transcript, spend settled onto the wrong engagement, a terminal status
// landing on in-flight work, and two authors in one window that cannot be told
// apart.
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
// described above. Local mode has no second executor, so apart from its own
// lease lapsing the stop verb is the only thing that can trip its fence, and
// the refusal it gets is the ordinary shape of every stop that catches a run
// mid-setup.
var ErrClaimReleased = errors.New("db: claim released — this engagement no longer owns the conversation")
