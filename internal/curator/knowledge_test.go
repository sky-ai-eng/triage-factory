package curator

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/paths"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// orgSegment is the literal subtree segment multi mode inserts per
// tenant; the local layout must never contain it.
var orgSegment = string(filepath.Separator) + "orgs" + string(filepath.Separator)

// TestKnowledgeDir_LocalLayout pins the local on-disk layout: the curator
// project dir resolves under <state-root>/projects/<id> with no org
// segment — byte-for-byte what it was before SKY-402, so existing local
// installs need no migration.
func TestKnowledgeDir_LocalLayout(t *testing.T) {
	root := t.TempDir()
	runmode.SetForTest(t, runmode.ModeLocal)
	paths.SetForTest(t, root)

	const projectID = "proj-local"
	got, err := KnowledgeDir(runmode.LocalDefaultOrgID, projectID)
	if err != nil {
		t.Fatalf("KnowledgeDir: %v", err)
	}
	if want := filepath.Join(root, "projects", projectID); got != want {
		t.Errorf("KnowledgeDir(local) = %q, want %q", got, want)
	}
	if strings.Contains(got, orgSegment) {
		t.Errorf("local layout must not carry an org segment: %q", got)
	}

	gotRoot, err := ProjectsRoot(runmode.LocalDefaultOrgID)
	if err != nil {
		t.Fatalf("ProjectsRoot: %v", err)
	}
	if want := filepath.Join(root, "projects"); gotRoot != want {
		t.Errorf("ProjectsRoot(local) = %q, want %q", gotRoot, want)
	}
}

// TestKnowledgeDir_MultiLayout pins the multi-mode layout: the project
// dir lands under the per-org subtree
// <state-root>/orgs/<orgID>/projects/<id> that the shared-RO-worktree
// (SKY-403) and per-org eviction (SKY-406) work build on.
func TestKnowledgeDir_MultiLayout(t *testing.T) {
	root := t.TempDir()
	runmode.SetForTest(t, runmode.ModeMulti)
	paths.SetForTest(t, root)

	const orgID = "11111111-1111-1111-1111-111111111111"
	const projectID = "proj-multi"
	got, err := KnowledgeDir(orgID, projectID)
	if err != nil {
		t.Fatalf("KnowledgeDir: %v", err)
	}
	if want := filepath.Join(root, "orgs", orgID, "projects", projectID); got != want {
		t.Errorf("KnowledgeDir(multi) = %q, want %q", got, want)
	}

	gotRoot, err := ProjectsRoot(orgID)
	if err != nil {
		t.Fatalf("ProjectsRoot: %v", err)
	}
	if want := filepath.Join(root, "orgs", orgID, "projects"); gotRoot != want {
		t.Errorf("ProjectsRoot(multi) = %q, want %q", gotRoot, want)
	}
}

// TestKnowledgeDir_MultiPerOrgIsolation is the tenancy guard: two
// projects in different orgs must resolve to non-overlapping per-org
// subtrees — even when they share a project id — so one tenant's curator
// state can never collide with or nest inside another's on a shared pod.
func TestKnowledgeDir_MultiPerOrgIsolation(t *testing.T) {
	root := t.TempDir()
	runmode.SetForTest(t, runmode.ModeMulti)
	paths.SetForTest(t, root)

	const orgA = "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa"
	const orgB = "bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb"
	const projectID = "shared-id" // identical id across tenants must still split

	dirA, err := KnowledgeDir(orgA, projectID)
	if err != nil {
		t.Fatalf("KnowledgeDir(orgA): %v", err)
	}
	dirB, err := KnowledgeDir(orgB, projectID)
	if err != nil {
		t.Fatalf("KnowledgeDir(orgB): %v", err)
	}
	if dirA == dirB {
		t.Fatalf("per-org dirs collided: %q", dirA)
	}
	// Neither may be a prefix of the other — a tenant's subtree nested
	// inside another's would still straddle the boundary.
	if strings.HasPrefix(dirA, dirB+string(filepath.Separator)) ||
		strings.HasPrefix(dirB, dirA+string(filepath.Separator)) {
		t.Errorf("per-org dirs must not nest: %q vs %q", dirA, dirB)
	}
	if orgARoot := filepath.Join(root, "orgs", orgA); !strings.HasPrefix(dirA, orgARoot+string(filepath.Separator)) {
		t.Errorf("orgA dir %q not under %q", dirA, orgARoot)
	}
	if orgBRoot := filepath.Join(root, "orgs", orgB); !strings.HasPrefix(dirB, orgBRoot+string(filepath.Separator)) {
		t.Errorf("orgB dir %q not under %q", dirB, orgBRoot)
	}
}
