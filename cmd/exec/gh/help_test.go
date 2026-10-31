package gh

import (
	"context"
	"testing"

	"github.com/sky-ai-eng/triage-factory/cmd/exec/execflags"
	"github.com/sky-ai-eng/triage-factory/cmd/exec/prog"
)

// TestHelpBelowTheResourceLevel is the dead-end this closes: --help used to be
// recognized only as args[0] of the whole exec command, so `gh pr --help` fell
// through to the action switch and died on "unknown pr action: --help" — while
// the agent envelope explicitly tells agents to run --help on any subcommand.
//
// host is nil throughout: a help route that touched the agenthost would panic
// here, which is the point — help must answer without run identity, so it
// works outside a conversation too.
func TestHelpBelowTheResourceLevel(t *testing.T) {
	resource := prHelpText(prog.Prefix())
	action := func(a string) string { return "usage: " + prog.Prefix() + " " + prHelp[a] + "\n" }
	cases := []struct {
		name string
		args []string
		want string
	}{
		{"resource level", []string{"--help"}, resource},
		{"resource level short", []string{"-h"}, resource},
		{"known action", []string{"start-review", "--help"}, action("start-review")},
		{"known action short", []string{"finalize-review", "-h"}, action("finalize-review")},
		// A verb that isn't ours gets the resource help, which lists the verbs
		// that are — the useful answer to "what can I actually run here".
		{"unknown action", []string{"edit", "--help"}, resource},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if out := captureStdout(t, func() { handlePR(context.Background(), nil, tc.args) }); out != tc.want {
				t.Errorf("help for %q printed the wrong text;\ngot  %.200q\nwant %.200q", tc.args, out, tc.want)
			}
		})
	}
}

// TestActionsHelpBelowTheResourceLevel is the same contract for `gh actions`.
func TestActionsHelpBelowTheResourceLevel(t *testing.T) {
	want := actionsHelpText(prog.Prefix())
	for _, args := range [][]string{{"--help"}, {"-h"}, {"download-logs", "--help"}, {"nonsense", "--help"}} {
		if out := captureStdout(t, func() { handleActions(context.Background(), nil, args) }); out != want {
			t.Errorf("`gh actions %v` did not print the actions help; got:\n%s", args, out)
		}
	}
}

// TestResourceHelpRoute is the same contract one level up, for `gh` itself.
func TestResourceHelpRoute(t *testing.T) {
	if out := captureStdout(t, func() { Handle(context.Background(), nil, []string{"--help"}) }); out != helpText(prog.Prefix()) {
		t.Errorf("`gh --help` did not print the resource help; got:\n%s", out)
	}
}

// TestHelpFlagIsNotAFlagValue pins the other half: a body, severity, or repo
// whose literal value is "--help" must not be read as a help request. Without
// this the shared scan would turn `gh pr add-comment 7 --body "--help"` into a
// usage dump instead of a comment.
func TestHelpFlagIsNotAFlagValue(t *testing.T) {
	valueCases := [][]string{
		{"add-comment", "7", "--body", "--help"},
		{"view", "--repo", "--help"},
		{"comment-update", "abc", "--severity", "--help"},
	}
	for _, args := range valueCases {
		if execflags.HasHelpFlag(args[1:], ValueFlags) {
			t.Errorf("%q was read as a help request; the --help there is a flag's value", args)
		}
	}
	// And a boolean flag ahead of --help must NOT swallow it: `gh pr diff 7
	// --stdout --help` is a real help request, which is the correction the
	// jira-local scan needed before it could serve gh.
	if !execflags.HasHelpFlag([]string{"7", "--stdout", "--help"}, ValueFlags) {
		t.Error("a --help after a boolean flag must still be a help request")
	}
}

// TestValueFlagsCoversFirstPositional pins that firstPositional still skips
// every flag value it used to, now that it reads the shared ValueFlags set: a
// PR number must not be confused with the value of a preceding flag.
func TestValueFlagsCoversFirstPositional(t *testing.T) {
	if got := firstPositional([]string{"--repo", "octo/repo", "42"}); got != "42" {
		t.Errorf("firstPositional = %q, want 42 (the repo value must be skipped)", got)
	}
	if got := firstPositional([]string{"--body", "42", "--repo", "o/r", "7"}); got != "7" {
		t.Errorf("firstPositional = %q, want 7 (a numeric body value is not the positional)", got)
	}
	if got := firstPositional([]string{"--stdout", "9"}); got != "9" {
		t.Errorf("firstPositional = %q, want 9 (a boolean consumes no value)", got)
	}
}
