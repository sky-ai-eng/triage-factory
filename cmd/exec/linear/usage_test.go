package linear

import (
	"io"
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/sky-ai-eng/triage-factory/cmd/exec/prog"
)

// TestIssueHelpUsesResolvedPrefix pins the help routes: the resource help and
// a per-action usage line, each rendered under the prefix resolved from argv0.
// host is nil: the help routes answer before any agenthost call, which is what
// makes `linear issue view --help` work outside a run.
func TestIssueHelpUsesResolvedPrefix(t *testing.T) {
	action := func(a string) string { return "usage: " + prog.Prefix() + " " + issueHelp[a] + "\n" }
	cases := map[string]struct {
		args []string
		want string
	}{
		"resource level":  {[]string{"--help"}, helpText(prog.Prefix())},
		"per-action":      {[]string{"view", "--help"}, action("view")},
		"after a value":   {[]string{"comment", "ENG-1", "--body", "x", "--help"}, action("comment")},
		"search per-verb": {[]string{"search", "-h"}, action("search")},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			if out := captureStdout(t, func() { handleIssue(nil, tc.args) }); out != tc.want {
				t.Errorf("`linear issue %v` printed the wrong help;\ngot  %.200q\nwant %.200q", tc.args, out, tc.want)
			}
		})
	}
}

func TestParseIssueArgs(t *testing.T) {
	t.Run("positionals and values", func(t *testing.T) {
		a, err := parseIssueArgs("create", []string{"ENG", "--title", "t", "--label", "bug", "--label", "ui"}, 1)
		if err != nil {
			t.Fatal(err)
		}
		if a.positional[0] != "ENG" || a.value("--title") != "t" {
			t.Errorf("parsed %+v", a)
		}
		if !reflect.DeepEqual(a.values["--label"], []string{"bug", "ui"}) {
			t.Errorf("labels = %v, want [bug ui]", a.values["--label"])
		}
	})

	t.Run("a value that looks like a flag is a value", func(t *testing.T) {
		a, err := parseIssueArgs("comment", []string{"ENG-1", "--body", "--title is wrong"}, 1)
		if err != nil {
			t.Fatal(err)
		}
		if got := a.value("--body"); got != "--title is wrong" {
			t.Errorf("body = %q", got)
		}
	})

	t.Run("an explicit empty value is present", func(t *testing.T) {
		a, err := parseIssueArgs("edit", []string{"ENG-1", "--description", ""}, 1)
		if err != nil {
			t.Fatal(err)
		}
		if !a.has("--description") || a.value("--description") != "" {
			t.Errorf("an empty --description must read as present and empty, got %+v", a.values)
		}
	})

	t.Run("a repeatable flag repeats", func(t *testing.T) {
		a, err := parseIssueArgs("search", []string{"--team", "ENG", "--state", "Todo", "--state", "In Progress"}, 0)
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(a.values["--state"], []string{"Todo", "In Progress"}) {
			t.Errorf("states = %v", a.values["--state"])
		}
	})

	refusals := map[string]struct {
		action      string
		args        []string
		positionals int
		want        string
	}{
		"flag the action does not take": {"view", []string{"ENG-1", "--state", "Done"}, 1, "does not take --state"},
		"flag with no value":            {"transition", []string{"ENG-1", "--state"}, 1, "--state requires a value"},
		"single-value flag twice":       {"transition", []string{"ENG-1", "--state", "a", "--state", "b"}, 1, "given more than once"},
		"missing positional":            {"view", nil, 1, "usage: linear issue view <id>"},
		"extra positional":              {"view", []string{"ENG-1", "ENG-2"}, 1, "usage: linear issue view <id>"},
		"positional on search":          {"search", []string{"ENG", "--team", "ENG"}, 0, "usage: linear issue search"},
		// A blank value would read as the flag being absent: any assignee,
		// no parent, no title change.
		"blank assignee":   {"search", []string{"--team", "ENG", "--assignee", ""}, 0, "--assignee must not be blank"},
		"blank user":       {"assign", []string{"ENG-1", "--user", " "}, 1, "--user must not be blank"},
		"user twice":       {"assign", []string{"ENG-1", "--user", "a", "--user", "b"}, 1, "given more than once"},
		"blank state":      {"search", []string{"--team", "ENG", "--state", " "}, 0, "--state must not be blank"},
		"blank parent":     {"create", []string{"ENG", "--title", "t", "--parent", ""}, 1, "--parent must not be blank"},
		"blank edit title": {"edit", []string{"ENG-1", "--title", ""}, 1, "--title must not be blank"},
	}
	for name, tc := range refusals {
		t.Run(name, func(t *testing.T) {
			_, err := parseIssueArgs(tc.action, tc.args, tc.positionals)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("err = %v, want one containing %q", err, tc.want)
			}
		})
	}
}

func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	orig := os.Stdout
	os.Stdout = w
	defer func() { os.Stdout = orig }()

	done := make(chan string, 1)
	go func() {
		b, _ := io.ReadAll(r)
		done <- string(b)
	}()
	fn()
	if err := w.Close(); err != nil {
		t.Fatalf("close pipe: %v", err)
	}
	return <-done
}
