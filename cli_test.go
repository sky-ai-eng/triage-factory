package main

import "testing"

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
