package gh

import (
	"context"
	"strings"
	"testing"

	"github.com/sky-ai-eng/triage-factory/cmd/exec/execflags"
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
	cases := []struct {
		name     string
		args     []string
		contains string
	}{
		{"resource level", []string{"--help"}, "gh pr create"},
		{"resource level short", []string{"-h"}, "gh pr create"},
		{"known action", []string{"start-review", "--help"}, "gh pr start-review"},
		{"known action short", []string{"finalize-review", "-h"}, "gh pr finalize-review"},
		// A verb that isn't ours gets the resource help, which lists the verbs
		// that are — the useful answer to "what can I actually run here".
		{"unknown action", []string{"edit", "--help"}, "gh pr create"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out := captureStdout(t, func() { handlePR(context.Background(), nil, tc.args) })
			if !strings.Contains(out, tc.contains) {
				t.Errorf("help for %q did not mention %q; got:\n%s", tc.args, tc.contains, out)
			}
		})
	}
}

// TestActionsHelpBelowTheResourceLevel is the same contract for `gh actions`.
func TestActionsHelpBelowTheResourceLevel(t *testing.T) {
	for _, args := range [][]string{{"--help"}, {"-h"}, {"download-logs", "--help"}, {"nonsense", "--help"}} {
		out := captureStdout(t, func() { handleActions(context.Background(), nil, args) })
		if !strings.Contains(out, "gh actions download-logs") {
			t.Errorf("`gh actions %v` help did not list the actions; got:\n%s", args, out)
		}
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

// TestUnknownVerbMessageRoutes pins criterion the dead-end fix exists for: a
// mistyped verb must name the valid set and the help command that expands it,
// and must assert nothing about tooling outside this binary. The real `gh` is
// available on the native path and withheld on the SDK one, and exec cannot
// tell which is driving — so a message claiming either is wrong half the time.
func TestUnknownVerbMessageRoutes(t *testing.T) {
	// The hint is pinned under the applet spelling: the call site composes it
	// from prog.Prefix(), so the taught name is whatever the process was
	// invoked as.
	msg := unknownVerbMessage("pr action", "pr actions", "add-review-commnt", prActions,
		"tfac gh pr --help")
	for _, want := range []string{
		"unknown pr action: add-review-commnt",
		"add-review-comment", // the verb they meant, in the valid list
		"start-review",       // and the rest of the review lifecycle
		"tfac gh pr --help",  // where to read the usage, under the invoked name
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("message missing %q:\n%s", want, msg)
		}
	}
	// Harness-neutral: no claim about `gh`, the GitHub API, or reachability.
	for _, forbidden := range []string{"unreachable", "not available", "gh api", "GitHub REST"} {
		if strings.Contains(msg, forbidden) {
			t.Errorf("message asserts something it cannot know (%q):\n%s", forbidden, msg)
		}
	}
	// Every action the dispatch switch serves must appear in the advertised
	// set, or the message sends the agent looking for a verb that doesn't exist
	// (or hides one that does).
	if len(prActions) == 0 {
		t.Fatal("prActions is empty")
	}
	for _, a := range prActions {
		if !strings.Contains(HelpText, "gh pr "+a) {
			t.Errorf("advertised action %q has no usage line in the help text", a)
		}
	}
}
