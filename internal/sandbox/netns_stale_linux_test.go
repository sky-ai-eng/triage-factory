//go:build linux

package sandbox

import (
	"context"
	"os"
	"os/exec"
	"testing"
)

// staleNetnsSubnetIdx is the index these tests build their cell on. Any uint8
// is a valid index (the allocator's range is the full [0, MaxSandboxes)), so
// the top of the range is picked for the same reason a test picks a high port:
// the allocator hands out the lowest free index, so a real run on the same box
// will not be holding this one. The netns and veth names carry a hash of the
// run id besides, so only the /24 is shared at all.
const staleNetnsSubnetIdx uint8 = 255

// requireNetnsCapableHost skips unless this process can actually create network
// namespaces — root, iproute2 present, and a kernel/container that permits the
// namespace — and leaves a namespace called name behind for the caller.
// Skipping (rather than failing) matches the rest of this package's privileged
// tests: the same suite has to run on a stripped-down dev box.
//
// The delete-if-exists is the rule this whole file exists to pin, applied to
// the test's own setup. Without it, a namespace of this name left behind by an
// interrupted earlier run makes `ip netns add` fail with "File exists" — which
// is indistinguishable from "this host cannot make namespaces" once it reaches
// the skip below, so a perfectly capable box would skip these tests silently
// and forever, keeping the stale namespace that caused it. A test whose
// precondition cannot tolerate a stale predecessor of the same name has no
// business asserting that the production path does.
//
// Root first, because it is what makes the pre-delete meaningful: removing a
// namespace needs the same privilege creating one does, so an unprivileged
// process would fail the delete, fail the add with "File exists", and land back
// in the same ambiguity this ordering exists to remove.
func requireNetnsCapableHost(t *testing.T, name string) {
	t.Helper()
	if os.Geteuid() != 0 {
		t.Skip("needs root: creating a network namespace requires CAP_SYS_ADMIN")
	}
	if _, err := exec.LookPath("ip"); err != nil {
		t.Skip("iproute2 not installed; skipping netns test")
	}
	ctx := context.Background()
	_ = runIPNoErr(ctx, "netns", "delete", name)
	if err := runIP(ctx, "netns", "add", name); err != nil {
		t.Skipf("this environment does not permit netns creation (root, but no CAP_SYS_ADMIN — a container without it, say): %v", err)
	}
	t.Cleanup(func() { _ = runIPNoErr(context.Background(), "netns", "delete", name) })
}

// TestSetupNetwork_OverStaleNetnsOfTheSameName pins the delete-if-exists that
// makes a per-run netns tolerate a stale predecessor of its own name.
//
// The name is deterministic in (run id, subnet index), so a namespace leaked by
// a failed teardown collides with whichever later claim of the same run draws
// the same index — and `ip netns add` fails outright on an existing name, which
// fails the whole cell bring-up. The precondition here IS the bug: the test
// creates the namespace first, exactly as a failed teardown would have left it.
func TestSetupNetwork_OverStaleNetnsOfTheSameName(t *testing.T) {
	const runID = "stale-netns-run"
	name := NetnsNameForRun(runID, staleNetnsSubnetIdx)
	// Creating the namespace up front is both the precondition and the
	// capability probe — the cleanup covers the case where setupNetwork below
	// never gets far enough to own it.
	requireNetnsCapableHost(t, name)

	ctx := context.Background()
	state, err := setupNetwork(ctx, runID, staleNetnsSubnetIdx)
	t.Cleanup(func() { _ = teardownNetwork(context.Background(), state) })
	if err != nil {
		t.Fatalf("setupNetwork over a stale netns of the same name: %v", err)
	}
	if state.netnsName != name {
		t.Errorf("netns name = %q, want the deterministic %q", state.netnsName, name)
	}
	// The namespace the run got must be the freshly created one, carrying the
	// veth setupNetwork put in it — not the empty husk the predecessor left.
	if err := runIP(ctx, "-n", name, "link", "show", state.vethSandbox); err != nil {
		t.Errorf("sandbox veth %s is not in netns %s: %v", state.vethSandbox, name, err)
	}
}

// TestSetupNetwork_CleanHost is the ordinary path, kept beside the case above
// so the delete-if-exists cannot have made bring-up depend on there being
// something to delete.
func TestSetupNetwork_CleanHost(t *testing.T) {
	const runID = "clean-netns-run"
	requireNetnsCapableHost(t, "tf-probe-clean-netns")

	ctx := context.Background()
	state, err := setupNetwork(ctx, runID, staleNetnsSubnetIdx)
	t.Cleanup(func() { _ = teardownNetwork(context.Background(), state) })
	if err != nil {
		t.Fatalf("setupNetwork on a clean host: %v", err)
	}
	if got := NetnsNameForRun(runID, staleNetnsSubnetIdx); state.netnsName != got {
		t.Errorf("netns name = %q, want %q", state.netnsName, got)
	}
}
