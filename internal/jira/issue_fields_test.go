package jira

import (
	"reflect"
	"slices"
	"strings"
	"testing"
)

// TestIssueFields_MatchesIssue holds IssueFields to the fields Issue decodes.
// A field Issue gains without IssueFields gaining it decodes empty on every
// search that relies on the default, with no error; a name IssueFields keeps
// after Issue drops it is payload nothing reads.
func TestIssueFields_MatchesIssue(t *testing.T) {
	ft, ok := reflect.TypeOf(Issue{}).FieldByName("Fields")
	if !ok {
		t.Fatal("Issue has no Fields")
	}
	var decoded []string
	for f := range ft.Type.Fields() {
		name, _, _ := strings.Cut(f.Tag.Get("json"), ",")
		if name == "" || name == "-" {
			t.Fatalf("Issue.Fields.%s has no json name", f.Name)
		}
		decoded = append(decoded, name)
	}

	for _, name := range decoded {
		if !slices.Contains(IssueFields, name) {
			t.Errorf("Issue decodes %q but IssueFields does not request it", name)
		}
	}
	for _, name := range IssueFields {
		if !slices.Contains(decoded, name) {
			t.Errorf("IssueFields requests %q but Issue does not decode it", name)
		}
	}
}
