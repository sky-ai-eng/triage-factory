package skills

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// TestStageStepSkill_ContentParityWithWorktreeForm is the no-behavior-delta
// guard for the two materialization roots: staging a step's skill into an
// orchestrator-owned dir (the sandboxed path) must produce byte-identical
// content to writing it inside the worktree (the local path). Only the root
// differs, so a future change to one form can't silently change what the model
// reads on the other.
func TestStageStepSkill_ContentParityWithWorktreeForm(t *testing.T) {
	const slug = "chain-step-1-review-the-diff"
	for _, tc := range []struct {
		name   string
		prompt func() *domain.Prompt
	}{
		{"user_prompt", func() *domain.Prompt { return userPrompt("Review the diff", "Look at every hunk.\n") }},
		{"imported_skill", func() *domain.Prompt {
			return importedPrompt("Review the diff", "---\nname: something-else\ndescription: reviews\n---\n\nBody.\n")
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			wt := t.TempDir()
			if err := MaterializeStepSkill(wt, slug, tc.prompt(), "the brief"); err != nil {
				t.Fatalf("MaterializeStepSkill: %v", err)
			}
			staging := filepath.Join(t.TempDir(), "run-id")
			if err := StageStepSkill(staging, slug, tc.prompt(), "the brief"); err != nil {
				t.Fatalf("StageStepSkill: %v", err)
			}

			inWorktree := readFileT(t, filepath.Join(wt, ".claude", "skills", slug, "SKILL.md"))
			staged := readFileT(t, filepath.Join(staging, slug, "SKILL.md"))
			if string(inWorktree) != string(staged) {
				t.Errorf("staged SKILL.md differs from the in-worktree form\nstaged:    %q\nworktree:  %q", staged, inWorktree)
			}
		})
	}
}

// TestStageStepSkill_WipesByRecreating is the step-isolation property: the
// staging dir a launch mounts holds EXACTLY the step being staged, so step N+1
// can never expose step N's SKILL.md even when a prior attempt left one behind.
// This replaces the old in-worktree wipe, which the capability-less orchestrator
// cannot perform once the tree belongs to the sandbox uid.
func TestStageStepSkill_WipesByRecreating(t *testing.T) {
	staging := filepath.Join(t.TempDir(), "run-id")

	if err := StageStepSkill(staging, "chain-step-0-first", userPrompt("First", "first body"), ""); err != nil {
		t.Fatalf("stage step 0: %v", err)
	}
	if err := StageStepSkill(staging, "chain-step-1-second", userPrompt("Second", "second body"), ""); err != nil {
		t.Fatalf("stage step 1: %v", err)
	}

	entries, err := os.ReadDir(staging)
	if err != nil {
		t.Fatalf("read staging dir: %v", err)
	}
	if len(entries) != 1 || entries[0].Name() != "chain-step-1-second" {
		var names []string
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Fatalf("staging dir holds %v, want exactly [chain-step-1-second] — step 1 must not see step 0's skill", names)
	}
}

// TestRemoveStagedSkills_MissingIsSuccess: the step-end reclaim runs on every
// disposition (including one where staging never happened, e.g. local mode), so
// a missing dir must not be an error.
func TestRemoveStagedSkills_MissingIsSuccess(t *testing.T) {
	if err := RemoveStagedSkills(filepath.Join(t.TempDir(), "never-created")); err != nil {
		t.Errorf("RemoveStagedSkills on a missing dir = %v, want nil", err)
	}
	if err := RemoveStagedSkills(""); err != nil {
		t.Errorf("RemoveStagedSkills(\"\") = %v, want nil", err)
	}
}

func readFileT(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return b
}
