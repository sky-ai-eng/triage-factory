package app

import (
	"context"
	"database/sql"
	"go/ast"
	"go/parser"
	"go/token"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	sqlitestore "github.com/sky-ai-eng/triage-factory/internal/db/sqlite"
	"github.com/sky-ai-eng/triage-factory/internal/notify"
	"github.com/sky-ai-eng/triage-factory/internal/notify/notifytest"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
	"github.com/sky-ai-eng/triage-factory/pkg/websocket"
)

type bootParams struct {
	ID string `json:"id"`
}

var bootCondition = notify.Condition(notify.ConditionSpec[bootParams]{
	Name: "app_test.boot", Priority: notify.Always(notify.High),
	Key:      func(p bootParams) string { return p.ID },
	Audience: notify.OrgAdmins(),
	Render:   func(p bootParams, _ notify.Recipient) notify.Content { return notify.Content{Title: "boot " + p.ID} },
	Example:  bootParams{ID: "x"},
})

// TestBuildNotify_InstallsTheVerbs: after the server's notify step, a verb
// writes through the server's own stores.
func TestBuildNotify_InstallsTheVerbs(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeLocal)
	conn, err := sql.Open("sqlite", db.TestDSNMemory)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	conn.SetMaxOpenConns(1)
	if err := db.BootstrapSchemaForTest(conn); err != nil {
		t.Fatalf("bootstrap schema: %v", err)
	}
	stores := sqlitestore.New(conn)

	// The engine is bound to a connection this test closes.
	notifytest.Isolate(t)
	a := &App{stores: stores, wsHub: websocket.NewHub()}
	a.buildNotify()
	if !notify.Installed() {
		t.Fatal("buildNotify did not install notify")
	}

	ctx := context.Background()
	org, user := runmode.LocalDefaultOrgID, runmode.LocalDefaultUserID
	bootCondition.Set(ctx, org, bootParams{ID: "a"}, true)
	items, _, err := stores.Notifications.ListForViewer(ctx, org, user, db.NotificationListFilter{}, db.ListOpts{Limit: 50})
	if err != nil {
		t.Fatalf("ListForViewer: %v", err)
	}
	if len(items) != 1 || items[0].Kind != bootCondition.Name() || items[0].Title != "boot a" {
		t.Fatalf("inbox after Set = %+v; want the one row the verb wrote", items)
	}
}

// TestNew_CallsBuildNotify pins that the server's boot sequence runs the step
// above. New itself needs an identity lock, pools and a sandbox probe, which
// is more host than a unit test should take.
func TestNew_CallsBuildNotify(t *testing.T) {
	file, err := parser.ParseFile(token.NewFileSet(), "app.go", nil, 0)
	if err != nil {
		t.Fatalf("parse app.go: %v", err)
	}
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Recv != nil || fn.Name.Name != "New" {
			continue
		}
		called := false
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			if call, ok := n.(*ast.CallExpr); ok {
				if sel, ok := call.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == "buildNotify" {
					called = true
				}
			}
			return !called
		})
		if !called {
			t.Fatal("New does not call buildNotify; every notify verb would be a no-op in the server")
		}
		return
	}
	t.Fatal("app.go declares no func New")
}
