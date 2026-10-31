//go:build linux

package procinfo

import (
	"os"
	"runtime"
	"strings"
	"testing"

	"golang.org/x/sys/unix"
)

// readComm returns the process's comm as `ps` and the compose smoke test read
// it: /proc/self/comm, which is the thread-group leader's name.
func readComm(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile("/proc/self/comm")
	if err != nil {
		t.Fatalf("read /proc/self/comm: %v", err)
	}
	return strings.TrimRight(string(b), "\x00\n")
}

// onNonLeaderThread runs fn on an OS thread that is not the thread-group
// leader. A goroutine locked to a thread keeps that thread to itself, so when
// the first locked goroutine lands on the leader, a second one locked while
// the first still holds it cannot.
func onNonLeaderThread(t *testing.T, fn func()) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		defer close(done)
		runtime.LockOSThread()
		defer runtime.UnlockOSThread()
		if unix.Gettid() != unix.Getpid() {
			fn()
			return
		}
		inner := make(chan struct{})
		go func() {
			defer close(inner)
			runtime.LockOSThread()
			defer runtime.UnlockOSThread()
			if unix.Gettid() == unix.Getpid() {
				t.Error("second locked goroutine also landed on the thread-group leader")
				return
			}
			fn()
		}()
		<-inner
	}()
	<-done
}

// TestSetTitle_NamesTheProcessFromAnyThread pins that the name lands on the
// thread-group leader whichever OS thread the caller is on. The Go scheduler
// can move the main goroutine off the leader before main() reaches SetTitle,
// and a name set on another thread is invisible to /proc/<pid>/comm.
func TestSetTitle_NamesTheProcessFromAnyThread(t *testing.T) {
	onNonLeaderThread(t, func() { SetTitle("tf-cap-broker") })
	if got := readComm(t); got != "tf-cap-broker" {
		t.Errorf("comm after SetTitle from a non-leader thread = %q, want tf-cap-broker", got)
	}
}

func TestSetTitle_VisibleViaProcComm(t *testing.T) {
	tests := []struct {
		title string
		want  string
	}{
		{"tf-cap-broker", "tf-cap-broker"},
		{"tf-orchestrator", "tf-orchestrator"},                    // exactly 15 chars: the TASK_COMM_LEN boundary
		{"this-name-is-way-too-long-for-comm", "this-name-is-wa"}, // truncated to 15 usable bytes
	}
	for _, tt := range tests {
		t.Run(tt.title, func(t *testing.T) {
			SetTitle(tt.title)
			if got := readComm(t); got != tt.want {
				t.Errorf("comm after SetTitle(%q) = %q, want %q", tt.title, got, tt.want)
			}
		})
	}
}
