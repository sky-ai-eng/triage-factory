package install

import (
	"os"
	"runtime"
	"testing"
)

// TestDefaultDestination locks in the per-OS defaults so a future
// refactor (e.g. switching macOS to ~/.local/bin too) doesn't slip
// past review unannounced. The defaults matter — they're what users
// see when they run `triagefactory install` with no args.
func TestDefaultDestination(t *testing.T) {
	got := defaultDestination()
	switch runtime.GOOS {
	case "darwin":
		if got != "/usr/local/bin/triagefactory" {
			t.Errorf("darwin default = %q, want /usr/local/bin/triagefactory", got)
		}
	default:
		// Linux + everything else share the XDG userland default.
		if got != "~/.local/bin/triagefactory" {
			t.Errorf("default = %q, want ~/.local/bin/triagefactory", got)
		}
	}
}

// Tilde expansion for the install destination now lives in
// internal/paths.ExpandHome (keeping os.UserHomeDir confined to that
// package); its behavior is covered by internal/paths' TestExpandHome.

// TestOnPath checks the "is this dir on $PATH" detection used by the
// post-install warning. False positives or negatives would either
// silence the warning when the user really needs it (worse) or warn
// when everything's fine (just noisy).
func TestOnPath(t *testing.T) {
	bin := t.TempDir()
	other := t.TempDir()

	t.Setenv("PATH", bin+string(os.PathListSeparator)+"/usr/local/bin")
	if !onPath(bin) {
		t.Errorf("onPath(%q) = false, want true (PATH contains it)", bin)
	}
	if onPath(other) {
		t.Errorf("onPath(%q) = true, want false (PATH doesn't contain it)", other)
	}
}
