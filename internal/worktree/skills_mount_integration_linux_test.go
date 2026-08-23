//go:build linux

package worktree

import (
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/sandbox"
	"github.com/sky-ai-eng/triage-factory/internal/skills"
)

// Live two-step-blueprint mount integration test: the exact shape that failed
// deterministically in multi mode before step skills left the workspace.
//
// It reproduces the step boundary end to end on a real gVisor launch: build the
// run tree as the orchestrator (planting the jail's `.claude/skills` symlink),
// hand the tree to the per-run sandbox uid the way step 0's launch does, then run
// TWO launches against that same now-agent-owned tree — staging each step's skill
// only in the orchestrator's own dir, writing nothing into the tree at either
// boundary. Step 0's jail must see exactly step 0's SKILL.md through
// ~/.claude/skills, and step 1's must see exactly step 1's.
//
// Before the fix, the second boundary's in-worktree write died with `mkdir …
// /.claude/skills/chain-step-1-…: permission denied` and terminated the blueprint;
// the assertion that step 1 reads its own skill through the mount, from a tree
// TF never touched again, is that failure's inverse.
//
// The launch here goes through sandbox.Wrap — the same spec construction, mounts,
// and runsc exec production uses — but not the cap-broker RPC, so it exercises the
// mount's runtime behavior; ValidateLaunchParams's own gate on this destination is
// covered by the unit tests in internal/sandbox.
//
// Heavily gated and SKIP-by-default (the gh-injector integration test's pattern).
// All must hold:
//
//	TF_TEST_SKILLS_MOUNT_LIVE=1  opt in
//	runsc on PATH                gVisor runtime
//	euid 0                       the sandbox needs CAP_NET_ADMIN / CAP_SYS_ADMIN
//
// Run with, e.g.:
//
//	sudo TF_TEST_SKILLS_MOUNT_LIVE=1 go test ./internal/worktree -run TestIntegration_StepSkillsMount -v
func TestIntegration_StepSkillsMount_TwoStepBoundary(t *testing.T) {
	if os.Getenv("TF_TEST_SKILLS_MOUNT_LIVE") != "1" {
		t.Skip("set TF_TEST_SKILLS_MOUNT_LIVE=1 to run the live step-skills mount integration test")
	}
	if _, err := exec.LookPath("runsc"); err != nil {
		t.Skip("runsc not installed; skipping step-skills mount integration test")
	}
	if os.Geteuid() != 0 {
		t.Skip("gVisor sandbox needs root (CAP_NET_ADMIN / CAP_SYS_ADMIN); skipping")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	// The shared worktree a blueprint's steps run in, built once (step 0's setup)
	// and warm-reused by every later step.
	const treeRootKey = "itest-skills-tree"
	wtDir, _ := sandboxingWorktree(t, treeRootKey)

	// Step 0's launch hands the whole tree to the sandbox uid. From here on the
	// orchestrator is a genuinely unprivileged, capability-less user with respect
	// to this tree — the state that made the old in-worktree materialize fail.
	if err := sandbox.ChownRunTree(ctx, wtDir, ""); err != nil {
		t.Fatalf("ChownRunTree (step 0 launch): %v", err)
	}

	net, err := sandbox.SetupRunNetwork(ctx, treeRootKey)
	if err != nil {
		t.Fatalf("SetupRunNetwork: %v", err)
	}
	t.Cleanup(func() { _ = net.Close() })

	// Each step is its own conversation, so each stages into its own dir; that is
	// what makes "step N sees only its own skill" structural rather than a wipe.
	step := func(t *testing.T, stepIdx int, promptName, body string) string {
		t.Helper()
		conversationID := "itest-skills-step-" + promptName
		slug := skills.SlugForBlueprintStep(stepIdx, promptName)
		staging := sandbox.TrustedSkillsSourcePath(conversationID)
		prompt := &domain.Prompt{Name: promptName, Body: body, Source: "user"}
		if err := skills.StageStepSkill(staging, slug, prompt, "brief for "+promptName); err != nil {
			t.Fatalf("StageStepSkill (step %d): %v", stepIdx, err)
		}
		t.Cleanup(func() { _ = skills.RemoveStagedSkills(staging) })
		// The staging dir is the orchestrator's own — never chowned, so it stays
		// unreadable-as-a-write-target from inside and needs no ownership dance.
		if err := os.Chmod(staging, 0o755); err != nil {
			t.Fatalf("chmod staging: %v", err)
		}

		lr, _, werr := sandbox.Wrap(ctx, sandbox.Config{
			ConversationID: treeRootKey,
			Worktree:       wtDir,
			Argv: []string{"/bin/sh", "-c",
				`echo "--- listing"; ls -1 "$HOME/.claude/skills"; echo "--- content"; cat "$HOME/.claude/skills/` + slug + `/SKILL.md"`},
			Env: []string{"PATH=/usr/local/bin:/usr/bin:/bin", "HOME=/work"},
			ExtraMounts: []sandbox.Mount{
				{Source: staging, Destination: sandbox.TrustedSkillsDestination, Options: []string{"ro"}},
			},
			Network: net,
		})
		if werr != nil {
			t.Fatalf("sandbox.Wrap (step %d): %v", stepIdx, werr)
		}
		defer func() { _ = lr.Close() }()
		if err := lr.Start(); err != nil {
			t.Fatalf("start (step %d): %v", stepIdx, err)
		}
		out, _ := io.ReadAll(lr.Stdout())
		if err := lr.Wait(); err != nil {
			t.Fatalf("step %d jail exited non-zero: %v (out: %s stderr: %s)", stepIdx, err, out, lr.Stderr())
		}
		got := string(out)
		if !strings.Contains(got, body) {
			t.Errorf("step %d did not read its own SKILL.md through ~/.claude/skills:\n%s", stepIdx, got)
		}
		return got
	}

	// Step 0: discovers its skill through the symlink → the read-only mount.
	out0 := step(t, 0, "first-step", "FIRST STEP BODY MARKER")
	slug1 := skills.SlugForBlueprintStep(1, "second-step")
	if strings.Contains(out0, slug1) {
		t.Errorf("step 0 sees step 1's skill:\n%s", out0)
	}

	// Step 1: the boundary that used to fail. TF stages outside the tree and
	// mounts; nothing writes into the (sandbox-owned) worktree.
	out1 := step(t, 1, "second-step", "SECOND STEP BODY MARKER")
	slug0 := skills.SlugForBlueprintStep(0, "first-step")
	if strings.Contains(out1, slug0) {
		t.Errorf("step 1 sees step 0's leftover skill; the mount is not per-step:\n%s", out1)
	}

	// The tree itself still carries only the symlink — proof no step materialized
	// a skill inside the agent-owned workspace.
	link := filepath.Join(wtDir, ".claude", "skills")
	fi, err := os.Lstat(link)
	if err != nil {
		t.Fatalf("lstat %s after both steps: %v", link, err)
	}
	if fi.Mode()&os.ModeSymlink == 0 {
		t.Errorf("%s is a %v after two steps; TF wrote into the run tree", link, fi.Mode())
	}
}
