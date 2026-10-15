//go:build linux

package capbroker

import (
	"strings"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/sandbox"
)

// TestValidateOrchestratorUIDFlag_RejectsSidecarBand pins the broker-side
// half of the sidecar uid-band collision guard: an --orchestrator-uid
// inside the reserved sidecar range must be refused, with a clear error
// naming the collision. This is the earliest point the operator-supplied
// value is known — the orchestrator itself repeats the equivalent check
// against its own post-drop uid as the authoritative backstop
// (internal/app/privsep.go), since this flag and that drop are configured
// on two different command lines and could drift.
func TestValidateOrchestratorUIDFlag_RejectsSidecarBand(t *testing.T) {
	for _, uid := range []int{sandbox.SidecarUIDBase, sandbox.SidecarUIDBase + 5, sandbox.SidecarUIDBase + sandbox.MaxSandboxes - 1} {
		err := validateOrchestratorUIDFlag(uid)
		if err == nil {
			t.Errorf("uid %d: expected rejection, got nil", uid)
			continue
		}
		if !strings.Contains(err.Error(), "sidecar uid band") {
			t.Errorf("uid %d: err = %v, want it to name the sidecar uid band collision", uid, err)
		}
	}
}

// TestValidateOrchestratorUIDFlag_RejectsJailRange pins the same guard for
// the jail identities: an orchestrator at a jail uid would own the run trees
// handed to that index, and one at the parked owner would own every parked
// tree.
func TestValidateOrchestratorUIDFlag_RejectsJailRange(t *testing.T) {
	for _, uid := range []int{sandbox.ParkedRunTreeUID, sandbox.JailUIDBase, sandbox.JailUID(255)} {
		err := validateOrchestratorUIDFlag(uid)
		if err == nil {
			t.Errorf("uid %d: expected rejection, got nil", uid)
			continue
		}
		if !strings.Contains(err.Error(), "jail uid range") {
			t.Errorf("uid %d: err = %v, want it to name the jail uid range", uid, err)
		}
	}
}

// TestValidateOrchestratorUIDFlag_AllowsOutsideBand pins that the guard
// doesn't false-positive on the documented default (10001), the flag's
// "unset" sentinel (-1), or a value just past the band's top edge.
func TestValidateOrchestratorUIDFlag_AllowsOutsideBand(t *testing.T) {
	for _, uid := range []int{-1, 0, 10001, sandbox.SidecarUIDBase + sandbox.MaxSandboxes} {
		if err := validateOrchestratorUIDFlag(uid); err != nil {
			t.Errorf("uid %d: unexpected rejection: %v", uid, err)
		}
	}
}

// TestValidateOrchestratorGIDFlag_RejectsLeakyGroups pins the boot-time refusal
// of a run-tree group that would defeat its own purpose. The group is chosen so
// that no sandboxed principal carries it; the two ways to break that — a jail
// gid (a run's jail and its sidecar carry it) or a value in the sidecar band —
// must be rejected with an error that names the hazard, so a misconfigured
// TF_ORCHESTRATOR_GID fails closed at boot instead of silently opening every run
// tree to a run's sidecar.
func TestValidateOrchestratorGIDFlag_RejectsLeakyGroups(t *testing.T) {
	for _, gid := range []int{sandbox.JailGID(0), sandbox.JailGID(255)} {
		if err := validateOrchestratorGIDFlag(gid); err == nil {
			t.Errorf("gid %d: expected jail-band rejection, got nil", gid)
		} else if !strings.Contains(err.Error(), "jail gid band") {
			t.Errorf("gid %d: err = %v, want it to name the jail gid band", gid, err)
		}
	}
	for _, gid := range []int{sandbox.SidecarUIDBase, sandbox.SidecarUIDBase + sandbox.MaxSandboxes - 1} {
		if err := validateOrchestratorGIDFlag(gid); err == nil {
			t.Errorf("gid %d: expected sidecar-band rejection, got nil", gid)
		} else if !strings.Contains(err.Error(), "sidecar band") {
			t.Errorf("gid %d: err = %v, want it to name the sidecar band", gid, err)
		}
	}
}

// TestValidateOrchestratorGIDFlag_AllowsExclusiveGroups pins that the guard
// admits the documented default (10001, the orchestrator's own gid, carried by
// nothing sandboxed), the "unset" sentinel (-1), and a value just past the
// sidecar band's top edge.
func TestValidateOrchestratorGIDFlag_AllowsExclusiveGroups(t *testing.T) {
	for _, gid := range []int{-1, 10000, 10001, sandbox.SidecarUIDBase + sandbox.MaxSandboxes, sandbox.JailUIDBase + sandbox.MaxSandboxes} {
		if err := validateOrchestratorGIDFlag(gid); err != nil {
			t.Errorf("gid %d: unexpected rejection: %v", gid, err)
		}
	}
}
