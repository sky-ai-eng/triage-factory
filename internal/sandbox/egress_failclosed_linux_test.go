//go:build linux

package sandbox

import (
	"context"
	"testing"
)

// TestApplyEgressPolicy_FailsClosed pins the linchpin of Layer 1: the
// per-run egress allowlist is fatal-on-failure, never best-effort.
// Several helpers in iptables_linux.go (teardownIptables, the reapers)
// deliberately swallow errors, but applyEgressPolicy must NOT — a run
// whose allowlist failed to apply would proceed with OPEN egress (able
// to reach a sibling run's credential proxy and the open internet),
// which is exactly the cross-tenant / exfil hole the allowlist closes.
//
// The test drives applyEgressPolicy at a guaranteed-nonexistent netns so
// its first (in-netns) command fails immediately. The point isn't WHY it
// fails — missing netns, missing iptables, or insufficient privilege all
// reach the same branch — but THAT the function surfaces the failure as
// a non-nil error rather than returning nil and letting the caller wire
// up a sandbox with no egress filter. wrap() turns that error into a
// fatal "sandbox: egress policy" failure (sandbox_linux.go), so an
// allowlist that can't be installed aborts the run.
//
// Layer-1 (in-netns) runs before any host-side rule is added, so a
// nonexistent netns fails before applyEgressPolicy mutates host state —
// no rule is leaked even when iptables is present and privileged.
func TestApplyEgressPolicy_FailsClosed(t *testing.T) {
	ctx := context.Background()
	const (
		bogusNetns   = "tf-egress-failclosed-does-not-exist"
		bogusVeth    = "vh-failclosed-nope"
		bogusGateway = "10.42.250.1"
	)

	rules, err := applyEgressPolicy(ctx, bogusNetns, bogusVeth, bogusGateway)
	if err == nil {
		t.Fatal("applyEgressPolicy returned nil error for a nonexistent netns; " +
			"the egress allowlist must fail closed (return an error so wrap aborts the run), not silently no-op")
	}
	if rules != nil {
		t.Errorf("applyEgressPolicy returned %d rule(s) alongside its error; a failed apply must return no rules so teardown has nothing stale to chase", len(rules))
	}
}
