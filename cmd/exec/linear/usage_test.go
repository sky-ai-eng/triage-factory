package linear

import (
	"io"
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/sky-ai-eng/triage-factory/cmd/exec/prog"
)

// TestHelpTextNamesInvokedPrefix pins the `linear` usage line for each invoked
// form: the applet, the canonical name, and a path.
func TestHelpTextNamesInvokedPrefix(t *testing.T) {
	for _, prefix := range []string{"tfac", "triagefactory exec", "./triagefactory exec"} {
		want := "Usage: " + prefix + " linear <resource> <action> [flags]\n"
		if got := helpText(prefix); !strings.HasPrefix(got, want) {
			t.Errorf("helpText(%q) first line = %q, want %q", prefix, firstLine(got), want)
		}
	}
	if strings.Contains(helpText("tfac"), "triagefactory exec") {
		t.Error("applet `linear` help mentions 'triagefactory exec'")
	}
}

// TestIssueHelpUsesResolvedPrefix pins that both the resource help and a
// per-action usage line name the prefix resolved from argv0. host is nil: the
// help routes answer before any agenthost call, which is what makes `linear
// issue view --help` work outside a run.
func TestIssueHelpUsesResolvedPrefix(t *testing.T) {
	cases := map[string][]string{
		"resource level":  {"--help"},
		"per-action":      {"view", "--help"},
		"after a value":   {"comment", "ENG-1", "--body", "x", "--help"},
		"search per-verb": {"search", "-h"},
	}
	for name, args := range cases {
		t.Run(name, func(t *testing.T) {
			out := captureStdout(t, func() { handleIssue(nil, args) })
			if !strings.Contains(out, prog.Prefix()) {
				t.Errorf("`linear issue %v` help does not name the resolved prefix %q; got:\n%s", args, prog.Prefix(), out)
			}
		})
	}
}

// TestHelpCoversEveryAction holds the overview, the per-action usage table and
// the dispatch to one another: every action has a usage line, and the overview
// names it.
func TestHelpCoversEveryAction(t *testing.T) {
	want := []string{
		"assign", "comment", "create", "edit", "list-children", "list-states",
		"search", "set-parent", "set-priority", "transition", "unassign", "view",
	}
	if got := issueActions(); !reflect.DeepEqual(got, want) {
		t.Errorf("issueActions() = %v, want %v", got, want)
	}
	for _, action := range want {
		if !strings.Contains(HelpText, "linear issue "+action+" ") {
			t.Errorf("HelpText does not list `linear issue %s`", action)
		}
	}
}

// TestUnknownActionMessageNamesInvokedPrefix pins the mistyped-verb hint.
func TestUnknownActionMessageNamesInvokedPrefix(t *testing.T) {
	msg := unknownActionMessage("tfac", "vew")
	for _, want := range []string{
		"unknown issue action: vew",
		"view",
		"Run 'tfac linear issue --help' for usage.",
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("message missing %q:\n%s", want, msg)
		}
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

// TestIssueFlagsMatchUsage holds the per-action flag table to the usage lines:
// an action accepts exactly the flags its usage names, a flag the usage shows
// repeating ("--label <name>]...") is the one the table lets repeat, and every
// flag is a value flag for the help scan.
func TestIssueFlagsMatchUsage(t *testing.T) {
	for action, usage := range issueHelp {
		named := map[string]bool{}
		fields := strings.Fields(usage)
		for i, tok := range fields {
			flag := strings.Trim(tok, "[]")
			if !strings.HasPrefix(flag, "--") {
				continue
			}
			repeats := i+1 < len(fields) && strings.HasSuffix(fields[i+1], "]...")
			named[flag] = repeats
			if !ValueFlags[flag] {
				t.Errorf("%s usage names %s, which is not in ValueFlags", action, flag)
			}
		}
		if got := issueFlags[action]; !reflect.DeepEqual(nonEmpty(got), named) {
			t.Errorf("issueFlags[%q] = %v, usage names %v", action, got, named)
		}
	}
}

func nonEmpty(m map[string]bool) map[string]bool {
	if m == nil {
		return map[string]bool{}
	}
	return m
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i+1]
	}
	return s
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
