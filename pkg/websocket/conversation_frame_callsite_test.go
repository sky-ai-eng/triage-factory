package websocket_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestConversationFramesGoThroughTheNotifier keeps every websocket frame
// about one conversation inside internal/wsevent/conversationframe, which
// sends each to the conversation's REST audience. The hub scopes a
// connection by org and user only, so a websocket.Event{...} literal that
// names a conversation anywhere else would reach every member of the org,
// whatever their team, and a conversation's frames carry its transcript.
//
// Allowed places: the notifier itself; pkg/websocket, which defines the
// type; the backplane envelope, which rebuilds an event another pod already
// scoped; and _test.go files, which build frames to assert behavior.
func TestConversationFramesGoThroughTheNotifier(t *testing.T) {
	repoRoot, err := filepath.Abs("../..")
	if err != nil {
		t.Fatalf("resolve repo root: %v", err)
	}
	allowedDirs := []string{
		filepath.Join(repoRoot, "internal", "wsevent", "conversationframe") + string(filepath.Separator),
		filepath.Join(repoRoot, "pkg", "websocket") + string(filepath.Separator),
	}
	allowedFiles := map[string]bool{
		filepath.Join(repoRoot, "internal", "pgnotify", "wsbackplane", "envelope.go"): true,
	}

	var offenders []string
	fset := token.NewFileSet()
	err = filepath.Walk(repoRoot, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil
		}
		if info.IsDir() {
			switch info.Name() {
			case "node_modules", "frontend", ".git":
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") || allowedFiles[path] {
			return nil
		}
		for _, dir := range allowedDirs {
			if strings.HasPrefix(path, dir) {
				return nil
			}
		}
		src, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			// The build catches a syntax error in a production file; a
			// fixture that is not Go source is not this test's concern.
			return nil
		}
		ast.Inspect(src, func(n ast.Node) bool {
			cl, ok := n.(*ast.CompositeLit)
			if !ok {
				return true
			}
			sel, ok := cl.Type.(*ast.SelectorExpr)
			if !ok || sel.Sel.Name != "Event" {
				return true
			}
			if ident, ok := sel.X.(*ast.Ident); !ok || ident.Name != "websocket" {
				return true
			}
			for _, elt := range cl.Elts {
				kv, ok := elt.(*ast.KeyValueExpr)
				if !ok {
					continue
				}
				if key, ok := kv.Key.(*ast.Ident); ok && key.Name == "ConversationID" {
					pos := fset.Position(cl.Pos())
					rel, relErr := filepath.Rel(repoRoot, pos.Filename)
					if relErr != nil {
						rel = pos.Filename
					}
					offenders = append(offenders, rel+":"+itoa(pos.Line))
				}
			}
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatalf("walk repo: %v", err)
	}
	if len(offenders) > 0 {
		t.Fatalf("websocket.Event{...} literal naming a conversation outside internal/wsevent/conversationframe at:\n  %s\n\nSend the frame through a conversationframe.Notifier, which delivers it to the conversation's REST audience. A new conversation frame gets a method there.", strings.Join(offenders, "\n  "))
	}
}
