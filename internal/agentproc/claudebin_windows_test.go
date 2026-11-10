//go:build windows

package agentproc

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// Windows files carry no execute bit, so TF_CLAUDE_BINARY is judged by what
// the file is: a regular .exe or .js file passes, anything else is refused.
func TestClaudeBinaryOverride_WindowsGoesByExtension(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeLocal)
	dir := t.TempDir()
	for name, wantOK := range map[string]bool{
		"claude.exe":  true,
		"CLAUDE2.EXE": true,
		"cli.js":      true,
		"claude":      false,
		"claude.cmd":  false,
		"claude.txt":  false,
	} {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
		t.Setenv(envClaudeBinary, p)
		got, err := claudeBinaryOverride()
		if wantOK && (err != nil || got != p) {
			t.Errorf("%s: got (%q, %v), want it accepted", name, got, err)
		}
		if !wantOK && err == nil {
			t.Errorf("%s: accepted as %q, want an error", name, got)
		}
	}
}
