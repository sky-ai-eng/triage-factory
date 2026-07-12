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
