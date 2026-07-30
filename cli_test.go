package main

import (
	"path/filepath"
	"slices"
	"testing"
)

// TestDispatchCLI covers the routing decision only — known subcommands run
// their own handlers (and os.Exit), so they're deliberately not exercised
// here. What matters is that flags fall through to server mode while a
// mistyped subcommand is rejected rather than silently booting the server.
func TestDispatchCLI(t *testing.T) {
	tests := []struct {
		name        string
		args        []string
		wantHandled bool
		wantErr     bool
	}{
		{"empty falls through to server mode", nil, false, false},
		{"flag falls through to server mode", []string{"--port", "8080"}, false, false},
		{"unknown subcommand errors", []string{"bogus"}, true, true},
		{"unknown subcommand with trailing flags errors", []string{"bogus", "--port", "8080"}, true, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			handled, err := dispatchCLI(tt.args)
			if handled != tt.wantHandled {
				t.Errorf("handled = %v, want %v", handled, tt.wantHandled)
			}
			if (err != nil) != tt.wantErr {
				t.Errorf("err = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

// TestResolveCLIArgs pins the argv0 applet dispatch: what the `tfac` name
// resolves to before dispatchCLI ever sees it. Asserting the resolved args
// keeps this a pure routing test — no DB, no subcommand execution.
func TestResolveCLIArgs(t *testing.T) {
	tests := []struct {
		name string
		argv []string
		want []string
	}{
		{
			"applet prepends the implicit exec",
			[]string{"/opt/tf/bin/tfac", "workspace", "list"},
			[]string{"exec", "workspace", "list"},
		},
		{
			"applet does not double an explicit exec",
			[]string{"/opt/tf/bin/tfac", "exec", "workspace", "list"},
			[]string{"exec", "workspace", "list"},
		},
		{
			"canonical name is untouched",
			[]string{"/usr/local/bin/triagefactory", "exec", "workspace", "list"},
			[]string{"exec", "workspace", "list"},
		},
		{
			"bare applet resolves to a bare exec",
			[]string{"tfac"},
			[]string{"exec"},
		},
		{
			"applet exec with no verb matches the bare applet",
			[]string{"tfac", "exec"},
			[]string{"exec"},
		},
		{
			"applet help resolves to exec --help",
			[]string{"tfac", "--help"},
			[]string{"exec", "--help"},
		},
		{
			// Nobody types this; only one `exec` collapses, so it reaches the
			// unknown-command error rather than being silently normalized.
			"only one leading exec collapses",
			[]string{"tfac", "exec", "exec", "foo"},
			[]string{"exec", "exec", "foo"},
		},
		{
			// The applet only rewrites the leading position — a verb's own
			// argument that happens to be "exec" is not a prefix.
			"a later exec is not collapsed",
			[]string{"tfac", "gh", "exec"},
			[]string{"exec", "gh", "exec"},
		},
		{
			"server flags under the canonical name fall through unchanged",
			[]string{"triagefactory", "--port", "8080"},
			[]string{"--port", "8080"},
		},
		{
			"canonical server subcommands are untouched",
			[]string{"triagefactory", "install"},
			[]string{"install"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := resolveCLIArgs(tt.argv)
			if !slices.Equal(got, tt.want) {
				t.Errorf("resolveCLIArgs(%q) = %q, want %q", tt.argv, got, tt.want)
			}
		})
	}
}

// TestResolveCLIArgsEquivalentSpellings states the acceptance criterion
// directly: the three spellings of one command are indistinguishable by the
// time dispatch runs.
func TestResolveCLIArgsEquivalentSpellings(t *testing.T) {
	canonical := resolveCLIArgs([]string{filepath.Join("/usr/local/bin", "triagefactory"), "exec", "workspace", "list"})
	for _, argv := range [][]string{
		{"/opt/tf/bin/tfac", "workspace", "list"},
		{"/opt/tf/bin/tfac", "exec", "workspace", "list"},
	} {
		if got := resolveCLIArgs(argv); !slices.Equal(got, canonical) {
			t.Errorf("resolveCLIArgs(%q) = %q, want %q", argv, got, canonical)
		}
	}
}
