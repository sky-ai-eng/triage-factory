package linear

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
	linearclient "github.com/sky-ai-eng/triage-factory/internal/linear"
)

// stubHost answers the person verbs and records what they were asked. Every
// other agenthost method is the nil embedded interface, so reaching one fails
// the test loudly.
type stubHost struct {
	agenthost.Client
	assigned [][2]string
	searched []string
	queried  []string
}

func (h *stubHost) LinearAssign(_ context.Context, issue, userID string) (linearclient.User, error) {
	h.assigned = append(h.assigned, [2]string{issue, userID})
	return linearclient.User{ID: userID, Name: "Ana Lyst", Active: true}, nil
}

func (h *stubHost) LinearSearch(_ context.Context, req agenthost.LinearSearchRequest) ([]linearclient.Issue, error) {
	h.searched = append(h.searched, req.Assignee)
	return nil, nil
}

func (h *stubHost) LinearSearchUsers(_ context.Context, query string) (agenthost.LinearUserMatches, error) {
	h.queried = append(h.queried, query)
	return agenthost.LinearUserMatches{Users: []linearclient.User{{ID: "user-ana", Name: "Ana Lyst", Active: true}}, Truncated: true}, nil
}

func TestAssign_SendsTheNamedUser(t *testing.T) {
	h := &stubHost{}
	out := captureStdout(t, func() { handleIssue(h, []string{"assign", "ENG-1", "--user", "user-ana"}) })
	if !reflect.DeepEqual(h.assigned, [][2]string{{"ENG-1", "user-ana"}}) {
		t.Errorf("host was asked to assign %v, want ENG-1 to user-ana", h.assigned)
	}
	var got struct {
		Assigned linearclient.User `json:"assigned"`
	}
	if err := json.Unmarshal([]byte(out), &got); err != nil || got.Assigned.ID != "user-ana" || got.Assigned.Name != "Ana Lyst" {
		t.Errorf("output %q does not name the assigned user (%v)", out, err)
	}
}

// TestAssign_WithoutUserIsRefused re-runs the test binary, because the refusal
// exits the process: an assign that names nobody exits 1 with the reason and
// never reaches the host.
func TestAssign_WithoutUserIsRefused(t *testing.T) {
	if os.Getenv("TF_LINEAR_ASSIGN_CHILD") == "1" {
		h := &stubHost{}
		handleIssue(h, []string{"assign", "ENG-1"})
		fmt.Printf("host asked to assign %v\n", h.assigned)
		return
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestAssign_WithoutUserIsRefused$")
	cmd.Env = append(os.Environ(), "TF_LINEAR_ASSIGN_CHILD=1")
	out, err := cmd.CombinedOutput()
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() != 1 {
		t.Fatalf("child = %v, want exit status 1; output:\n%s", err, out)
	}
	if !strings.Contains(string(out), "--user is required") || !strings.Contains(string(out), "linear user search") {
		t.Errorf("refusal does not name the problem and the lookup verb:\n%s", out)
	}
	if strings.Contains(string(out), "host asked") {
		t.Errorf("the refused assign reached the host:\n%s", out)
	}
}

func TestSearch_PassesTheAssigneeThrough(t *testing.T) {
	h := &stubHost{}
	captureStdout(t, func() { handleIssue(h, []string{"search", "--team", "ENG", "--assignee", "user-ana"}) })
	captureStdout(t, func() { handleIssue(h, []string{"search", "--team", "ENG", "--assignee", "none"}) })
	if !reflect.DeepEqual(h.searched, []string{"user-ana", "none"}) {
		t.Errorf("host searched for assignees %v, want [user-ana none]", h.searched)
	}
}

func TestUserSearch_SendsTheQuery(t *testing.T) {
	h := &stubHost{}
	out := captureStdout(t, func() { handleUser(h, []string{"search", "ana lyst"}) })
	if !reflect.DeepEqual(h.queried, []string{"ana lyst"}) {
		t.Errorf("host was queried with %v, want [ana lyst]", h.queried)
	}
	var got agenthost.LinearUserMatches
	if err := json.Unmarshal([]byte(out), &got); err != nil || len(got.Users) != 1 || got.Users[0].ID != "user-ana" || !got.Truncated {
		t.Errorf("output %q is not the users the host answered, with its truncated flag (%v)", out, err)
	}
}

func TestParseUserSearchArgs(t *testing.T) {
	if q, err := parseUserSearchArgs([]string{"ana"}); err != nil || q != "ana" {
		t.Errorf("parse [ana] = %q, %v", q, err)
	}
	for name, args := range map[string][]string{
		"no query":        nil,
		"two words":       {"ana", "lyst"},
		"a flag":          {"--max", "5"},
		"a blank query":   {" "},
		"a flag and more": {"ana", "--team", "ENG"},
	} {
		if _, err := parseUserSearchArgs(args); err == nil {
			t.Errorf("%s: %v was accepted", name, args)
		}
	}
}

func TestUserHelp(t *testing.T) {
	for _, args := range [][]string{{"search", "--help"}, {"search", "-h"}} {
		out := captureStdout(t, func() { handleUser(nil, args) })
		if !strings.Contains(out, "linear user search <query>") {
			t.Errorf("`linear user %v` printed %q, want the search usage", args, out)
		}
	}
	if !strings.Contains(HelpText, "linear user search ") {
		t.Error("HelpText does not list `linear user search`")
	}
}
