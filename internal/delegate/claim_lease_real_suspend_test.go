package delegate

import (
	"context"
	"fmt"
	"os"
	"runtime"
	"testing"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/suspendclock"
)

// Bounds for the real-suspend test. realSuspendMinSleep is comfortably longer
// than the lease, so the sleep is certain to lapse it; realSuspendWait is how
// long the test waits, awake, for the sleep to start.
const (
	realSuspendMinSleep = 2 * time.Minute
	realSuspendWait     = 20 * time.Minute
	// realSuspendSettle is how long the suspend clock must hold still before
	// the machine counts as awake. A laptop can wake briefly in the dark and
	// sleep again; the test measures the whole sleep, not its first part.
	realSuspendSettle = 10 * time.Second
	// realSuspendJitter is how far two readings of an idle suspend clock can
	// differ: its two kernel clocks are read in two calls, and a preemption
	// between them shows up as up to hundreds of microseconds. A change under
	// this is the same reading.
	realSuspendJitter = 100 * time.Millisecond
	// realSuspendClockSlack is how far the suspend clock may disagree with the
	// wall clock's jump across the sleep. The wall clock can be corrected by
	// NTP on wake by the drift the hardware clock accumulated while asleep,
	// which is well under this.
	realSuspendClockSlack = 5 * time.Second
)

// TestSuspendRecovery_RealSuspend runs the suspend recovery against a real
// system sleep: the platform's suspend clock, Go's monotonic clock stopping,
// and SQLite's wall clock jumping, none of them faked, at the product's lease
// timings. Skipped by default; opt in with
//
//	TF_TEST_REAL_SUSPEND=1 go test ./internal/delegate -run TestSuspendRecovery_RealSuspend -v -timeout 40m
//
// and, once it logs READY, put the machine to sleep for at least
// realSuspendMinSleep and wake it. It needs no credentials and no network.
//
// Beyond the recovery itself it checks the suspend clock against the wall
// clock's jump across the sleep, which is the evidence that the clock pair the
// platform's suspendclock file reads really measures time asleep.
func TestSuspendRecovery_RealSuspend(t *testing.T) {
	if os.Getenv("TF_TEST_REAL_SUSPEND") != "1" {
		t.Skip("set TF_TEST_REAL_SUSPEND=1 and suspend the machine when the test logs READY")
	}
	base, ok := suspendclock.Suspended()
	if !ok {
		t.Fatalf("suspendclock reports nothing on %s/%s, so there is no recovery to verify here", runtime.GOOS, runtime.GOARCH)
	}

	f := newClaimLeaseFixture(t)
	// The claim as the dispatcher mints it: the product's lease, from now.
	if _, err := f.database.Exec(
		`UPDATE claims SET lease_expires_at = strftime('%Y-%m-%d %H:%M:%f','now',?) WHERE id = ?`,
		fmt.Sprintf("%+.3f seconds", DefaultClaimLease.Seconds()), f.conv.ClaimID,
	); err != nil {
		t.Fatalf("stamp the product lease: %v", err)
	}
	f.runLoop(t, time.Now())
	waitUntil(t, 5*time.Second, "the loop to register its lease", func() bool {
		return f.s.claimLeaseFor(f.conv.ClaimID) != nil
	})

	monoStart := time.Now()
	wallStart := time.Now().Round(0)
	t.Logf("READY: suspend the machine now for at least %s and then wake it (lease %s, renewal every %s); waiting up to %s awake",
		realSuspendMinSleep, DefaultClaimLease, DefaultClaimRenewInterval, realSuspendWait)

	// Wait for the clock to move, then for it to hold still: the whole sleep,
	// including any brief dark wake inside it.
	deadline := time.Now().Add(realSuspendWait)
	var slept time.Duration
	var stillSince time.Time
	for {
		now, _ := suspendclock.Suspended()
		if (now - base - slept).Abs() > realSuspendJitter {
			slept, stillSince = now-base, time.Now()
		} else if slept >= suspendThreshold && time.Since(stillSince) >= realSuspendSettle {
			break
		} else if slept < suspendThreshold && time.Now().After(deadline) {
			t.Fatalf("no suspend observed within %s awake. If the machine did sleep, the suspend clock on %s is not measuring it.",
				realSuspendWait, runtime.GOOS)
		}
		time.Sleep(200 * time.Millisecond)
	}
	woke := stillSince
	wallJump := time.Now().Round(0).Sub(wallStart) - time.Since(monoStart)
	t.Logf("woke: suspend clock says %s asleep, wall clock jumped %s", slept.Round(time.Millisecond), wallJump.Round(time.Millisecond))

	if diff := (wallJump - slept).Abs(); diff > realSuspendClockSlack {
		t.Errorf("the suspend clock (%s) and the wall clock's jump (%s) disagree by %s, over %s: the clock pair suspendclock reads on %s is not measuring time asleep",
			slept, wallJump, diff, realSuspendClockSlack, runtime.GOOS)
	}
	if slept < realSuspendMinSleep {
		t.Fatalf("slept %s, under %s: the lease may not have lapsed, so there may be nothing to recover. Sleep longer and rerun.",
			slept.Round(time.Second), realSuspendMinSleep)
	}

	waitUntil(t, 15*time.Second, "the re-acquire after wake", func() bool {
		return !f.queue.firstReacquire().IsZero()
	})
	// From the last movement of the suspend clock this test saw, so a dark
	// wake inside the sleep can make it negative: the claim was taken back
	// then, and held across the rest of the sleep.
	reacquiredAfter := f.queue.firstReacquire().Sub(woke)

	if err := context.Cause(f.claimCtx); err != nil {
		t.Errorf("the engagement was fenced (%v); a run that slept with its claim unreleased carries on", err)
	}
	if n := f.recoveries(suspendRecoveryReacquired); n < 1 {
		t.Errorf("claims.suspend_recoveries{outcome=reacquired} = %d, want at least 1", n)
	}
	for _, outcome := range []string{suspendRecoveryTaken, suspendRecoveryFailed} {
		if n := f.recoveries(outcome); n != 0 {
			t.Errorf("claims.suspend_recoveries{outcome=%s} = %d, want 0", outcome, n)
		}
	}

	var ids []string
	rows, err := f.database.Query(`SELECT id FROM claims WHERE conversation_id = ?`, f.conv.ID)
	if err != nil {
		t.Fatalf("read the claims: %v", err)
	}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			t.Fatalf("scan a claim: %v", err)
		}
		ids = append(ids, id)
	}
	_ = rows.Close()
	if len(ids) != 1 || ids[0] != f.conv.ClaimID {
		t.Errorf("claims on the conversation = %v, want only the original %s", ids, f.conv.ClaimID)
	}
	if !f.leaseLive(t) {
		t.Error("the claim's lease is not live after the wake")
	}
	var expiry string
	if err := f.database.QueryRow(`SELECT lease_expires_at FROM claims WHERE id = ?`, f.conv.ClaimID).Scan(&expiry); err != nil {
		t.Fatalf("read the lease: %v", err)
	}

	// The engagement can still write.
	if _, err := f.s.conversations.InsertMessageForClaimSystem(context.Background(), f.conv.OrgID, f.conv.ClaimID,
		&domain.Message{ConversationID: f.conv.ID, Role: "assistant", Content: "written after the wake"}); err != nil {
		t.Errorf("fenced write after the wake: %v", err)
	}

	t.Logf("RESULT: platform=%s/%s slept=%s wall_jump=%s reacquired_after_wake=%s claim=%s lease_expires_at=%s (UTC)",
		runtime.GOOS, runtime.GOARCH, slept.Round(time.Millisecond), wallJump.Round(time.Millisecond),
		reacquiredAfter.Round(time.Millisecond), f.conv.ClaimID, expiry)
}
