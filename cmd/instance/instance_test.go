package instance

import (
	"bytes"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

func TestHeartbeatAge(t *testing.T) {
	if got := heartbeatAge(time.Time{}); got != "never" {
		t.Errorf("heartbeatAge(zero) = %q, want %q", got, "never")
	}
	if got := heartbeatAge(time.Now().Add(-5 * time.Second)); got != "5s ago" {
		t.Errorf("heartbeatAge(5s ago) = %q, want %q", got, "5s ago")
	}
	// last in the future (clock skew between the DB server and this CLI's
	// host) must not render a negative duration.
	if got := heartbeatAge(time.Now().Add(5 * time.Second)); got != "just now" {
		t.Errorf("heartbeatAge(future) = %q, want %q", got, "just now")
	}
}

// TestWriteListJSON pins `instance list --json`, the shape `deploy upgrade`
// reads from the control container: every field by name, the three drain
// values, a control row's null run count, and a heartbeat age that never
// goes negative.
func TestWriteListJSON(t *testing.T) {
	now := time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)
	three := 3
	rows := []domain.Instance{
		{ID: "ctl", Role: domain.InstanceRoleControl, Version: "v1.17.0", LastHeartbeatAt: now.Add(-2 * time.Second)},
		{ID: "exec-a", Role: domain.InstanceRoleExecutor, Version: "v1.17.0", LastHeartbeatAt: now.Add(-1500 * time.Millisecond), Draining: true, ActiveRuns: &three},
		{ID: "exec-b", Role: domain.InstanceRoleExecutor, Version: "v1.17.0", LastHeartbeatAt: now.Add(time.Second), Draining: true, DrainUntilRestart: true, ActiveRuns: new(int)},
	}
	var buf bytes.Buffer
	if err := writeListJSON(&buf, rows, now); err != nil {
		t.Fatalf("writeListJSON: %v", err)
	}
	want := `[
  {"id":"ctl","role":"control","version":"v1.17.0","drain":"none","active_runs":null,"heartbeat_age_seconds":2},
  {"id":"exec-a","role":"executor","version":"v1.17.0","drain":"persistent","active_runs":3,"heartbeat_age_seconds":1.5},
  {"id":"exec-b","role":"executor","version":"v1.17.0","drain":"until_restart","active_runs":0,"heartbeat_age_seconds":0}
]`
	var got, wantVal any
	if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
		t.Fatalf("output is not JSON: %v\n%s", err, buf.String())
	}
	if err := json.Unmarshal([]byte(want), &wantVal); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, wantVal) {
		t.Errorf("writeListJSON =\n%s\nwant\n%s", buf.String(), want)
	}
}

func TestWriteListJSON_EmptyIsAnEmptyArray(t *testing.T) {
	var buf bytes.Buffer
	if err := writeListJSON(&buf, nil, time.Now()); err != nil {
		t.Fatalf("writeListJSON: %v", err)
	}
	if got := strings.TrimSpace(buf.String()); got != "[]" {
		t.Errorf("writeListJSON(nil) = %q; want []", got)
	}
}

func TestDrainingLabel(t *testing.T) {
	for _, tt := range []struct {
		inst domain.Instance
		want string
	}{
		{domain.Instance{}, "no"},
		{domain.Instance{Draining: true}, "yes"},
		{domain.Instance{Draining: true, DrainUntilRestart: true}, "until restart"},
	} {
		if got := drainingLabel(tt.inst); got != tt.want {
			t.Errorf("drainingLabel(%+v) = %q; want %q", tt.inst, got, tt.want)
		}
	}
}
