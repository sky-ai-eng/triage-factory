//go:build linux

package procname

import (
	"os"
	"os/exec"
	"strings"
	"testing"
)

// helperEnv gates TestMain's re-exec branch, the standard os/exec
// "TestHelperProcess" pattern (see cmd/capbroker/orchestrator_linux_test.go
// for the same idiom): a test re-invokes this test binary with the env var
// set, and TestMain calls SetTitle then reports /proc/self/comm instead of
// running the test suite — needed because PR_SET_NAME only renames the
// calling thread's comm, and a plain in-process call from within `go
// test`'s own many-goroutine runtime has no guarantee of landing on the
// thread-group leader that /proc/<pid>/comm reads back.
const helperEnv = "PROCNAME_TEST_HELPER_TITLE"

func TestMain(m *testing.M) {
	if title := os.Getenv(helperEnv); title != "" {
		SetTitle(title)
		comm, err := os.ReadFile("/proc/self/comm")
		if err != nil {
			os.Stderr.WriteString(err.Error())
			os.Exit(1)
		}
		os.Stdout.Write(comm)
		os.Exit(0)
	}
	os.Exit(m.Run())
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
			cmd := exec.Command(os.Args[0])
			cmd.Env = append(os.Environ(), helperEnv+"="+tt.title)
			out, err := cmd.Output()
			if err != nil {
				t.Fatalf("helper subprocess: %v", err)
			}
			got := strings.TrimRight(string(out), "\x00\n")
			if got != tt.want {
				t.Errorf("comm after SetTitle(%q) = %q, want %q", tt.title, got, tt.want)
			}
		})
	}
}
