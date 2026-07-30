package prog

import (
	"os"
	"os/exec"
	"strings"
	"testing"
)

// TestFor pins the whole prefix rule. The applet collapses to its bare name
// (the `exec` word is implicit under it); everything else echoes argv0
// verbatim, because that is the string the reader can actually run.
func TestFor(t *testing.T) {
	tests := []struct {
		argv0 string
		want  string
	}{
		{"tfac", "tfac"},
		{"/opt/tf/bin/tfac", "tfac"},
		{"./tfac", "tfac"},
		{"triagefactory", "triagefactory exec"},
		{"./triagefactory", "./triagefactory exec"},
		{"/usr/local/bin/triagefactory", "/usr/local/bin/triagefactory exec"},
		// A path that merely contains the applet name is not the applet.
		{"/opt/tfac/bin/triagefactory", "/opt/tfac/bin/triagefactory exec"},
		// The SDK sandbox teaches an absolute per-run binary path; help output
		// has to agree with it rather than naming a PATH entry that may not exist.
		{"/opt/tf/run/abc123/triagefactory", "/opt/tf/run/abc123/triagefactory exec"},
		// Pathological exec with no argv[0] at all: nothing invocable to echo,
		// so name the canonical binary.
		{"", "triagefactory exec"},
	}
	for _, tt := range tests {
		if got := For(tt.argv0); got != tt.want {
			t.Errorf("For(%q) = %q, want %q", tt.argv0, got, tt.want)
		}
	}
}

// TestPrefixMatchesArgv0 pins that Prefix() reads this process's own argv0
// rather than a constant, and is stable across calls (it is memoized).
func TestPrefixMatchesArgv0(t *testing.T) {
	if got, want := Prefix(), For(os.Args[0]); got != want {
		t.Errorf("Prefix() = %q, want %q (derived from os.Args[0] = %q)", got, want, os.Args[0])
	}
	if first, second := Prefix(), Prefix(); first != second {
		t.Errorf("Prefix() is not stable: %q then %q", first, second)
	}
}

// prefixMarker prefixes the child's reported value so it survives the test
// framework's own output on the same stream.
const prefixMarker = "RESOLVED-PREFIX="

// TestPrefixHonorsArgv0 is the end-to-end half: re-exec this test binary with a
// rewritten argv[0] and read back what Prefix() resolved to. Nothing else can
// prove the applet and path rules hold for a *really* invoked process — the
// unit test above can only prove the pure rule.
//
// Re-execing the test binary (rather than building the real one) keeps it cheap
// and hermetic: the binary already exists, and Prefix() is the whole subject.
func TestPrefixHonorsArgv0(t *testing.T) {
	if os.Getenv("TF_PROG_ARGV0_CHILD") == "1" {
		// Child: report what argv0 resolved to and exit through the normal
		// test-pass path.
		os.Stdout.WriteString(prefixMarker + Prefix() + "\n")
		return
	}
	tests := []struct {
		argv0 string
		want  string
	}{
		{"tfac", "tfac"},
		{"/opt/tf/bin/tfac", "tfac"},
		{"triagefactory", "triagefactory exec"},
		{"./triagefactory", "./triagefactory exec"},
		{"/usr/local/bin/triagefactory", "/usr/local/bin/triagefactory exec"},
	}
	for _, tt := range tests {
		t.Run(tt.argv0, func(t *testing.T) {
			cmd := exec.Command(os.Args[0], "-test.run=TestPrefixHonorsArgv0")
			// The binary to run stays cmd.Path; only the name it sees as
			// argv[0] changes — exactly what a second bind mount or a
			// relative invocation does.
			cmd.Args = []string{tt.argv0, "-test.run=TestPrefixHonorsArgv0"}
			cmd.Env = append(os.Environ(), "TF_PROG_ARGV0_CHILD=1")
			out, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("child run failed: %v\n%s", err, out)
			}
			got, ok := reportedPrefix(string(out))
			if !ok {
				t.Fatalf("child did not report a prefix; output:\n%s", out)
			}
			if got != tt.want {
				t.Errorf("invoked as %q: prefix = %q, want %q", tt.argv0, got, tt.want)
			}
		})
	}
}

func reportedPrefix(out string) (string, bool) {
	for _, line := range strings.Split(out, "\n") {
		if rest, ok := strings.CutPrefix(strings.TrimSpace(line), prefixMarker); ok {
			return rest, true
		}
	}
	return "", false
}
