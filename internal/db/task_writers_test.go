package db

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// taskUpdateRE matches a statement that writes the tasks table. The one write
// that names the table through a variable, the entity merge's move of
// entity_id, writes no column a person acts on.
var taskUpdateRE = regexp.MustCompile(`(?i)\bUPDATE\s+tasks\b`)

// taskWriter is one function that writes the tasks table: whether it renders
// its SET list through TaskSetSQL, and, for one that does not, why the
// columns it writes are none of TaskVersionColumns.
type taskWriter struct {
	bumps bool
	why   string
}

func bumping() taskWriter              { return taskWriter{bumps: true} }
func notBumping(why string) taskWriter { return taskWriter{why: why} }

// taskWriters is every function in either dialect that writes the tasks
// table. A writer that changes a column of TaskVersionColumns has to go
// through TaskSetSQL, or its write leaves the version where it was and a
// person's stale write lands over the change.
//
// A new task writer fails this test until it is listed here, so whoever adds
// it decides, in review, whether it moves the version.
var taskWriters = map[string]taskWriter{
	"postgres/entities.go moveEntityReferences":    bumping(),
	"postgres/scores.go MarkScoring":               notBumping("scoring_status"),
	"postgres/scores.go ResetScoringToPending":     notBumping("scoring_status"),
	"postgres/scores.go ResetStaleScoring":         notBumping("scoring_status"),
	"postgres/scores.go UpdateTaskScores":          notBumping("the scores, summaries, scoring_status and score_revision"),
	"postgres/tasks.go AdvanceStatusForUser":       bumping(),
	"postgres/tasks.go ClaimQueuedForUser":         bumping(),
	"postgres/tasks.go ClearCloseDeferralSystem":   notBumping("the held close"),
	"postgres/tasks.go ClearSnooze":                bumping(),
	"postgres/tasks.go CloseByUser":                bumping(),
	"postgres/tasks.go HandoffAgentClaim":          bumping(),
	"postgres/tasks.go RequeueTask":                bumping(),
	"postgres/tasks.go SnoozeTask":                 bumping(),
	"postgres/tasks.go TakeoverClaimFromAgent":     bumping(),
	"postgres/tasks.go bumpTask":                   bumping(),
	"postgres/tasks.go closeTaskOnEvent":           notBumping("the held close; the close it applies goes through closeTaskRow"),
	"postgres/tasks.go closeTaskRow":               bumping(),
	"postgres/tasks.go reassignClaimToUser":        bumping(),
	"postgres/tasks.go reopenClosedTask":           bumping(),
	"postgres/tasks.go setOwnerTeam":               bumping(),
	"postgres/tasks.go stampAgentClaimIfUnclaimed": bumping(),
	"sqlite/entities.go moveEntityReferences":      bumping(),
	"sqlite/scores.go MarkScoring":                 notBumping("scoring_status"),
	"sqlite/scores.go ResetScoringToPending":       notBumping("scoring_status"),
	"sqlite/scores.go ResetStaleScoring":           notBumping("scoring_status"),
	"sqlite/scores.go applyScoresChunk":            notBumping("the scores, summaries, scoring_status and score_revision"),
	"sqlite/tasks.go AdvanceStatusForUser":         bumping(),
	"sqlite/tasks.go Bump":                         bumping(),
	"sqlite/tasks.go ClaimQueuedForUser":           bumping(),
	"sqlite/tasks.go ClearCloseDeferralSystem":     notBumping("the held close"),
	"sqlite/tasks.go ClearSnooze":                  bumping(),
	"sqlite/tasks.go CloseByUser":                  bumping(),
	"sqlite/tasks.go HandoffAgentClaim":            bumping(),
	"sqlite/tasks.go RequeueTask":                  bumping(),
	"sqlite/tasks.go SnoozeTask":                   bumping(),
	"sqlite/tasks.go TakeoverClaimFromAgent":       bumping(),
	"sqlite/tasks.go closeTaskOnEvent":             notBumping("the held close; the close it applies goes through closeTaskRow"),
	"sqlite/tasks.go closeTaskRow":                 bumping(),
	"sqlite/tasks.go reassignClaimToUserSQLite":    bumping(),
	"sqlite/tasks.go reopenClosedTask":             bumping(),
	"sqlite/tasks.go setOwnerTeam":                 bumping(),
	"sqlite/tasks.go stampAgentClaimIfUnclaimed":   bumping(),
}

// TestTaskWriters_EveryWriterIsListed finds every function in the two
// dialects whose SQL writes the tasks table, and checks it against
// taskWriters: each is listed, and one listed as moving the version builds
// its SET list with db.TaskSetSQL, and none of that SQL sits at package level,
// where no function owns it. What each writer does to the version is
// RunTaskVersionConformance's business; this keeps the set of writers it has
// to cover from growing unseen.
func TestTaskWriters_EveryWriterIsListed(t *testing.T) {
	found := map[string]bool{} // writer → whether it calls db.TaskSetSQL
	for _, dialect := range []string{"postgres", "sqlite"} {
		paths, err := filepath.Glob(filepath.Join(dialect, "*.go"))
		if err != nil {
			t.Fatal(err)
		}
		for _, path := range paths {
			if strings.HasSuffix(path, "_test.go") {
				continue
			}
			src, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			file, err := parser.ParseFile(token.NewFileSet(), path, src, 0)
			if err != nil {
				t.Fatalf("parse %s: %v", path, err)
			}
			for _, decl := range file.Decls {
				fn, ok := decl.(*ast.FuncDecl)
				if !ok {
					// A writer is listed by the function that runs it, so SQL
					// held at package level would write the table from a
					// function this scan cannot name.
					ast.Inspect(decl, func(n ast.Node) bool {
						if lit, ok := n.(*ast.BasicLit); ok && lit.Kind == token.STRING {
							if s, err := strconv.Unquote(lit.Value); err == nil && taskUpdateRE.MatchString(s) {
								t.Errorf("%s holds a statement that writes the tasks table outside a function: "+
									"keep it in the function that runs it, so this test can list that writer", filepath.ToSlash(path))
							}
						}
						return true
					})
					continue
				}
				if fn.Body == nil {
					continue
				}
				writes, renders := false, false
				ast.Inspect(fn.Body, func(n ast.Node) bool {
					switch n := n.(type) {
					case *ast.BasicLit:
						if n.Kind == token.STRING {
							if s, err := strconv.Unquote(n.Value); err == nil && taskUpdateRE.MatchString(s) {
								writes = true
							}
						}
					case *ast.SelectorExpr:
						if x, ok := n.X.(*ast.Ident); ok && x.Name == "db" && n.Sel.Name == "TaskSetSQL" {
							renders = true
						}
					}
					return true
				})
				if writes {
					found[filepath.ToSlash(path)+" "+fn.Name.Name] = renders
				}
			}
		}
	}
	if len(found) == 0 {
		t.Fatal("found no task writer: the scan verified nothing")
	}

	names := make([]string, 0, len(found))
	for name := range found {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		w, listed := taskWriters[name]
		switch {
		case !listed:
			t.Errorf("%s writes the tasks table and is not in taskWriters: list it, as bumping() if it can change "+
				"one of TaskVersionColumns (and build its SET list with db.TaskSetSQL), or notBumping(the columns it writes)", name)
		case w.bumps && !found[name]:
			t.Errorf("%s is listed as moving the version but does not build its SET list with db.TaskSetSQL", name)
		case !w.bumps && found[name]:
			t.Errorf("%s builds its SET list with db.TaskSetSQL but is listed as notBumping(%q)", name, w.why)
		}
	}
	for name := range taskWriters {
		if _, ok := found[name]; !ok {
			t.Errorf("taskWriters lists %s, which writes no task row: remove it", name)
		}
	}
}
