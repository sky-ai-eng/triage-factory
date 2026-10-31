package jira

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"reflect"
	"strings"
	"testing"

	"github.com/sky-ai-eng/triage-factory/cmd/exec/agenthost"
	"github.com/sky-ai-eng/triage-factory/cmd/exec/prog"
	jiraclient "github.com/sky-ai-eng/triage-factory/internal/jira"
)

// stubHost answers the person verbs and records what they were asked. Every
// other agenthost method is the nil embedded interface, so reaching one fails
// the test loudly.
type stubHost struct {
	agenthost.Client
	assigned [][2]string
	searched [][2]string
}

func (h *stubHost) JiraAssign(_ context.Context, key, userID string) (jiraclient.User, error) {
	h.assigned = append(h.assigned, [2]string{key, userID})
	return jiraclient.User{ID: userID, DisplayName: "Ana Lyst", Active: true}, nil
}

func (h *stubHost) JiraSearchUsers(_ context.Context, key, query string) (agenthost.JiraUserMatches, error) {
	h.searched = append(h.searched, [2]string{key, query})
	return agenthost.JiraUserMatches{Users: []jiraclient.User{{ID: "JIRAUSER10100", DisplayName: "Ana Lyst", Active: true}}, Truncated: true}, nil
}

func TestAssign_SendsTheNamedUser(t *testing.T) {
	h := &stubHost{}
	out := captureStdout(t, func() { handleTicket(h, []string{"assign", "SKY-1", "--user", "JIRAUSER10100"}) })
	if !reflect.DeepEqual(h.assigned, [][2]string{{"SKY-1", "JIRAUSER10100"}}) {
		t.Errorf("host was asked to assign %v, want SKY-1 to JIRAUSER10100", h.assigned)
	}
	var got struct {
		Assigned jiraclient.User `json:"assigned"`
	}
	if err := json.Unmarshal([]byte(out), &got); err != nil || got.Assigned.ID != "JIRAUSER10100" || got.Assigned.DisplayName != "Ana Lyst" {
		t.Errorf("output %q does not name the assigned user (%v)", out, err)
	}
}

// TestAssign_WithoutUserIsRefused re-runs the test binary, because the refusal
// exits the process: an assign that names nobody exits 1 with the reason and
// never reaches the host.
func TestAssign_WithoutUserIsRefused(t *testing.T) {
	if os.Getenv("TF_JIRA_ASSIGN_CHILD") == "1" {
		h := &stubHost{}
		handleTicket(h, []string{"assign", "SKY-1"})
		fmt.Printf("host asked to assign %v\n", h.assigned)
		return
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestAssign_WithoutUserIsRefused$")
	cmd.Env = append(os.Environ(), "TF_JIRA_ASSIGN_CHILD=1")
	out, err := cmd.CombinedOutput()
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() != 1 {
		t.Fatalf("child = %v, want exit status 1; output:\n%s", err, out)
	}
	if !strings.Contains(string(out), "--user is required") || !strings.Contains(string(out), "jira user search") {
		t.Errorf("refusal does not name the problem and the lookup verb:\n%s", out)
	}
	if strings.Contains(string(out), "host asked") {
		t.Errorf("the refused assign reached the host:\n%s", out)
	}
}

func TestUserSearch_SendsTheQueryAndIssue(t *testing.T) {
	for _, args := range [][]string{
		{"search", "ana lyst", "--issue", "SKY-1"},
		{"search", "--issue", "SKY-1", "ana lyst"},
	} {
		h := &stubHost{}
		out := captureStdout(t, func() { handleUser(h, args) })
		if !reflect.DeepEqual(h.searched, [][2]string{{"SKY-1", "ana lyst"}}) {
			t.Errorf("%v: host searched %v, want SKY-1 for \"ana lyst\"", args, h.searched)
		}
		var got agenthost.JiraUserMatches
		if err := json.Unmarshal([]byte(out), &got); err != nil || len(got.Users) != 1 || got.Users[0].ID != "JIRAUSER10100" || !got.Truncated {
			t.Errorf("%v: output %q is not the users the host answered, with its truncated flag (%v)", args, out, err)
		}
	}
}

func TestParseUserSearchArgs(t *testing.T) {
	refusals := map[string]struct {
		args []string
		want string
	}{
		"no issue":      {[]string{"ana"}, "--issue is required"},
		"blank issue":   {[]string{"ana", "--issue", " "}, "--issue is required"},
		"issue twice":   {[]string{"ana", "--issue", "SKY-1", "--issue", "SKY-2"}, "more than once"},
		"no value":      {[]string{"ana", "--issue"}, "requires a value"},
		"no query":      {[]string{"--issue", "SKY-1"}, "usage:"},
		"two words":     {[]string{"ana", "lyst", "--issue", "SKY-1"}, "usage:"},
		"another flag":  {[]string{"ana", "--issue", "SKY-1", "--max", "5"}, "does not take --max"},
		"a blank query": {[]string{" ", "--issue", "SKY-1"}, "must not be blank"},
	}
	for name, tc := range refusals {
		if _, _, err := parseUserSearchArgs(tc.args); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err = %v, want one containing %q", name, err, tc.want)
		}
	}
}

func TestUserHelp(t *testing.T) {
	want := "usage: " + prog.Prefix() + " " + userHelp["search"] + "\n"
	for _, args := range [][]string{{"search", "--help"}, {"search", "ana", "--issue", "SKY-1", "-h"}} {
		if out := captureStdout(t, func() { handleUser(nil, args) }); out != want {
			t.Errorf("`jira user %v` printed %q, want the search usage", args, out)
		}
	}
}
