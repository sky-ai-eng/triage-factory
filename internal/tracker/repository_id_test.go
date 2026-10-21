package tracker

import (
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// Every GitHub event's metadata names its entity's repository registry row,
// which is what repo_in filters match. A literal that leaves RepositoryID out
// emits events no repo_in filter can ever match, and compiles, so the package
// source is checked directly: every events.GitHub…Metadata composite literal
// outside tests must set it.
func TestGitHubMetadataLiteralsSetRepositoryID(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	checked := 0
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		ast.Inspect(f, func(n ast.Node) bool {
			lit, ok := n.(*ast.CompositeLit)
			if !ok {
				return true
			}
			sel, ok := lit.Type.(*ast.SelectorExpr)
			if !ok || !strings.HasPrefix(sel.Sel.Name, "GitHub") || !strings.HasSuffix(sel.Sel.Name, "Metadata") {
				return true
			}
			checked++
			for _, elt := range lit.Elts {
				if kv, ok := elt.(*ast.KeyValueExpr); ok {
					if key, ok := kv.Key.(*ast.Ident); ok && key.Name == "RepositoryID" {
						return true
					}
				}
			}
			t.Errorf("%s: events.%s literal does not set RepositoryID", fset.Position(lit.Pos()), sel.Sel.Name)
			return true
		})
	}
	if checked == 0 {
		t.Fatal("found no GitHub metadata literals: the guard checked nothing")
	}
}

// The diff stamps the repository id it is given on the events it emits.
func TestDiff_EventsCarryTheEntitysRepositoryID(t *testing.T) {
	prev := basePRSnapshot()
	curr := basePRSnapshot()
	curr.Labels = []string{"needs-review"}
	curr.CheckRuns = []domain.CheckRun{{ID: 1, Name: "build", Conclusion: "failure"}}

	evts := DiffPRSnapshots(prev, curr, testEntityID, testRepositoryID, testSelf, nil, nil, nil)
	if len(evts) < 2 {
		t.Fatalf("got %d events, want a label and a CI event", len(evts))
	}
	for _, evt := range evts {
		var meta struct {
			RepositoryID string `json:"repository_id"`
		}
		if err := json.Unmarshal([]byte(evt.MetadataJSON), &meta); err != nil {
			t.Fatalf("%s: decode metadata: %v", evt.EventType, err)
		}
		if meta.RepositoryID != testRepositoryID {
			t.Errorf("%s: repository_id = %q, want %q", evt.EventType, meta.RepositoryID, testRepositoryID)
		}
	}
}
