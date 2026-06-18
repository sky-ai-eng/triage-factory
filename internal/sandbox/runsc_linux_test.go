//go:build linux

package sandbox

import (
	"context"
	"slices"
	"strings"
	"testing"
)

// TestNewRunscCommand_EnablesHostUDS locks in the global host-uds=open flag
// that lets the jailed agent connect() to the bind-mounted per-run agenthost
// socket (/run/tf.sock). Without it gVisor defaults to host-uds=none and the
// connect is refused with ECONNREFUSED, which breaks every `triagefactory
// exec` subcommand.
//
// This is a pure-argv assertion so it runs in the normal `go test ./...`
// path. The end-to-end gVisor crossing is covered only by the
// integration-tagged suite, which CI does not run — which is exactly how a
// missing global flag could ship unnoticed. This guards it cheaply.
//
// It asserts the *intent* (host-uds set to open, as a global flag), not a
// literal token: host-uds takes a value, so runsc parses `--host-uds=open`
// and the split `--host-uds open` identically. A serialization-only refactor
// between those forms must not fail this test.
func TestNewRunscCommand_EnablesHostUDS(t *testing.T) {
	cmd := newRunscCommand(context.Background(), "/tmp/bundle", "cid-test")

	value, flagIdx, found := hostUDSArg(cmd.Args)
	if !found {
		t.Fatalf("runsc invocation is missing --host-uds; the agent cannot reach /run/tf.sock without it. args=%v", cmd.Args)
	}
	// open is connect-only — what a pure client needs. none is the broken
	// default; create/all would let the jail *bind* host sockets. Anything
	// but open fails here.
	if value != "open" {
		t.Fatalf("--host-uds must be open, got %q. args=%v", value, cmd.Args)
	}
	// Must be a *global* flag (before the `run` subcommand), or runsc rejects
	// it / treats it as a run flag and the default none silently wins.
	runIdx := slices.Index(cmd.Args, "run")
	if runIdx == -1 || flagIdx > runIdx {
		t.Fatalf("--host-uds must precede the run subcommand (it is a global flag): flagIdx=%d runIdx=%d. args=%v", flagIdx, runIdx, cmd.Args)
	}
}

// TestHostUDSArg_FormAgnostic proves the matcher accepts every form runsc
// treats as equivalent, so the assertion above tracks behavior rather than
// one particular spelling.
func TestHostUDSArg_FormAgnostic(t *testing.T) {
	cases := []struct {
		name string
		args []string
		want string
		ok   bool
	}{
		{"combined", []string{"runsc", "--host-uds=open", "run"}, "open", true},
		{"split", []string{"runsc", "--host-uds", "open", "run"}, "open", true},
		{"single-dash combined", []string{"runsc", "-host-uds=open", "run"}, "open", true},
		{"single-dash split", []string{"runsc", "-host-uds", "open", "run"}, "open", true},
		{"absent", []string{"runsc", "--network=sandbox", "run"}, "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, _, ok := hostUDSArg(tc.args)
			if ok != tc.ok || got != tc.want {
				t.Fatalf("hostUDSArg(%v) = (%q, _, %v); want (%q, _, %v)", tc.args, got, ok, tc.want, tc.ok)
			}
		})
	}
}

// hostUDSArg extracts the host-uds flag value from a runsc argv, accepting
// both the combined (`--host-uds=open`) and split (`--host-uds open`) forms
// and any leading-dash style. Returns the value, the index of the flag
// token, and whether it was found.
func hostUDSArg(args []string) (value string, flagIdx int, found bool) {
	for i, a := range args {
		norm := strings.TrimLeft(a, "-")
		if v, ok := strings.CutPrefix(norm, "host-uds="); ok {
			return v, i, true
		}
		if norm == "host-uds" && i+1 < len(args) {
			return args[i+1], i, true
		}
	}
	return "", -1, false
}
