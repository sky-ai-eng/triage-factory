package agentproc

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/sandbox"
)

// stubToolHostBinary points the mount source at a file a non-root test user
// can create, since toolHostMounts refuses to assemble without one.
func stubToolHostBinary(t *testing.T) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "tf-harness-tools")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	prev := hostToolHostBinaryPath
	hostToolHostBinaryPath = path
	t.Cleanup(func() { hostToolHostBinaryPath = prev })
}

func destinations(mounts []sandbox.Mount) map[string]sandbox.Mount {
	byDest := make(map[string]sandbox.Mount, len(mounts))
	for _, m := range mounts {
		byDest[m.Destination] = m
	}
	return byDest
}

// TestToolHostMounts_CarryTheStagedStepSkill pins that a native jail mounts
// this step's SKILL.md.
//
// The two runtimes build their mount sets independently, so a mount added to
// the SDK path does not reach this one — a blueprint step would stage a skill
// the agent then never sees, silently, with the run otherwise healthy. Only a
// test says the two paths must agree.
func TestToolHostMounts_CarryTheStagedStepSkill(t *testing.T) {
	stubToolHostBinary(t)
	staged := t.TempDir()

	mounts, err := toolHostMounts(t.TempDir(), nil, staged, "")
	if err != nil {
		t.Fatal(err)
	}
	got, ok := destinations(mounts)[sandbox.TrustedSkillsDestination]
	if !ok {
		t.Fatalf("no mount at %s; the step staged a skill the agent will never see", sandbox.TrustedSkillsDestination)
	}
	if got.Source != staged {
		t.Errorf("skills mount source = %q, want the staging dir %q", got.Source, staged)
	}
	// Read-only is required, not merely permitted: the mount exists so the
	// jail can READ a skill TF authored, and the broker rejects an rw bind.
	if len(got.Options) != 1 || got.Options[0] != "ro" {
		t.Errorf("skills mount options = %v, want [ro]", got.Options)
	}
}

// TestToolHostMounts_SkillsAreOptional pins the two ways a launch legitimately
// carries no skill: a run that is not a blueprint step, and a step whose
// staging dir was swept before a cold cross-executor resume. Neither may fail
// the launch — the agent continues from its transcript, which is a discovery
// degradation rather than a broken run.
func TestToolHostMounts_SkillsAreOptional(t *testing.T) {
	stubToolHostBinary(t)

	for _, tc := range []struct{ name, source string }{
		{"not a blueprint step", ""},
		{"staging dir swept", filepath.Join(t.TempDir(), "gone")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mounts, err := toolHostMounts(t.TempDir(), nil, tc.source, "")
			if err != nil {
				t.Fatalf("a missing skill must not fail the launch: %v", err)
			}
			if _, ok := destinations(mounts)[sandbox.TrustedSkillsDestination]; ok {
				t.Error("a skills mount was assembled with no staged skill behind it; the broker would reject the launch")
			}
		})
	}
}

// TestToolHostMounts_CarryTheStagedEntityMemory pins the same contract for the
// blueprint handoff. It is the case the skills test above warned about and the
// native path then reproduced: the memory mount reached RunOptions without
// reaching ToolHostOptions, so a native step read none of its predecessors'
// notes while an SDK step of the same blueprint read all of them.
func TestToolHostMounts_CarryTheStagedEntityMemory(t *testing.T) {
	stubToolHostBinary(t)
	staged := t.TempDir()

	mounts, err := toolHostMounts(t.TempDir(), nil, "", staged)
	if err != nil {
		t.Fatal(err)
	}
	got, ok := destinations(mounts)[sandbox.TrustedMemoryDestination]
	if !ok {
		t.Fatalf("no mount at %s; this step reads none of the prior memory it staged", sandbox.TrustedMemoryDestination)
	}
	if got.Source != staged {
		t.Errorf("memory mount source = %q, want the staging dir %q", got.Source, staged)
	}
	// Read-only for the reason the broker enforces it: an rw bind would let one
	// step rewrite the handoff a later step reads as fact.
	if len(got.Options) != 1 || got.Options[0] != "ro" {
		t.Errorf("memory mount options = %v, want [ro]", got.Options)
	}
}

// TestToolHostMounts_EntityMemoryIsOptional mirrors the skills case: a swept
// staging dir costs the step its handoff, which is a degraded start rather than
// a launch the broker must refuse.
func TestToolHostMounts_EntityMemoryIsOptional(t *testing.T) {
	stubToolHostBinary(t)

	for _, tc := range []struct{ name, source string }{
		{"nothing staged", ""},
		{"staging dir swept", filepath.Join(t.TempDir(), "gone")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mounts, err := toolHostMounts(t.TempDir(), nil, "", tc.source)
			if err != nil {
				t.Fatalf("missing prior memory must not fail the launch: %v", err)
			}
			if _, ok := destinations(mounts)[sandbox.TrustedMemoryDestination]; ok {
				t.Error("a memory mount was assembled with nothing staged behind it; the broker would reject the launch")
			}
		})
	}
}
