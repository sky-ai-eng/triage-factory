//go:build unix

package delegate

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/kbstore"
)

// This file is unix-only because it plants a named pipe, which Windows cannot
// create with mkfifo, and symlinks, which need a privilege there.

// TestKnowledgeForClaim_AHandedOffTreeListsOnlyItsOwnFiles: the agent can put
// anything into a tree it owns, and what the listing reads goes into the
// prompt. A link out of the tree, a link to a directory and a pipe are none of
// them knowledge: each is left out, nothing behind them is read, and the pipe
// does not hold the claim.
func TestKnowledgeForClaim_AHandedOffTreeListsOnlyItsOwnFiles(t *testing.T) {
	kb := kbstore.NewLocalAt(t.TempDir())
	writeKB(t, kb, kbTeam, kbstore.RootShared, "runbook.md", "How to roll back")
	s := stagingSpawner(t, kb, fakeTeams{})
	cwd := t.TempDir()
	if staged := s.knowledgeForClaim(context.Background(), kbOrg, kbTeam, cwd, false); staged == "" {
		t.Fatal("staging rendered no listing; this test would pass vacuously")
	}

	outside := t.TempDir()
	secret := filepath.Join(outside, "secret.txt")
	if err := os.WriteFile(secret, []byte("EXECUTOR-SECRET\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	shared := filepath.Join(cwd, scratchDirName, knowledgeDirName, "team", "shared")
	for name, target := range map[string]string{"env.md": secret, "escape": outside} {
		if err := os.Symlink(target, filepath.Join(shared, name)); err != nil {
			t.Fatalf("plant %s: %v", name, err)
		}
	}
	if err := syscall.Mkfifo(filepath.Join(shared, "pipe.md"), 0o644); err != nil {
		t.Fatalf("plant a pipe: %v", err)
	}

	done := make(chan string, 1)
	go func() { done <- s.knowledgeForClaim(context.Background(), kbOrg, kbTeam, cwd, true) }()
	var got string
	select {
	case got = <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("listing the tree blocked on what the agent planted in it")
	}
	if !strings.Contains(got, "runbook.md") {
		t.Errorf("the listing lost the tree's own file;\n%s", got)
	}
	for _, planted := range []string{"EXECUTOR-SECRET", "env.md", "escape", "secret.txt", "pipe.md"} {
		if strings.Contains(got, planted) {
			t.Errorf("the listing carries %q from what the agent planted;\n%s", planted, got)
		}
	}
}
