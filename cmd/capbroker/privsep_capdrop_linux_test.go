//go:build linux

package capbroker

import (
	"os"
	"os/exec"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/capinfo"
)

// capReportPattern matches TestMain's capReportEnv branch's output line:
// "uid=<N> CapEff=<names-or-(empty)>".
var capReportPattern = regexp.MustCompile(`^uid=(\d+) CapEff=(.*)$`)

func parseCapReport(t *testing.T, out []byte) (uid int, caps []string) {
	t.Helper()
	line := strings.TrimSpace(string(out))
	m := capReportPattern.FindStringSubmatch(line)
	if m == nil {
		t.Fatalf("unrecognized cap-report output: %q", out)
	}
	uid, err := strconv.Atoi(m[1])
	if err != nil {
		t.Fatalf("parse reported uid %q: %v", m[1], err)
	}
	if m[2] == "(empty)" {
		return uid, nil
	}
	return uid, strings.Split(m[2], ",")
}

func runCapReport(t *testing.T, setprivPath string, setprivArgs ...string) (uid int, caps []string) {
	t.Helper()
	args := append(append([]string{}, setprivArgs...), os.Args[0])
	cmd := exec.Command(setprivPath, args...)
	cmd.Env = append(os.Environ(), capReportEnv+"=1")
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("setpriv %v: %v (stderr: %s)", setprivArgs, err, stderr.String())
	}
	return parseCapReport(t, out)
}

func hasCap(caps []string, name string) bool {
	for _, c := range caps {
		if c == name {
			return true
		}
	}
	return false
}

// TestCapabilityDrop_BrokerKeepsExactPairOrchestratorEndsEmpty exercises,
// against a real setpriv binary and real kernel capability enforcement
// (not a simulation), the exact mechanism docker/entrypoint.sh applies at
// container boot:
//
//   - the cap-broker, spawned while this "container" holds exactly
//     {CAP_SYS_ADMIN, CAP_NET_ADMIN}, keeps exactly that pair;
//   - the orchestrator, exec'd via setpriv with its capability bounding
//     set cleared, inheritable capabilities dropped, no_new_privs set,
//     and switched to a different uid, ends up with an empty effective
//     capability set.
//
// A regression here — e.g. a future edit to the setpriv invocation that
// stops clearing the bounding set — would be caught by an actual
// kernel-level assertion, not just a code review, "so the boundary can't
// silently erode."
//
// Skipped unless running as a process that already holds at least
// CAP_SYS_ADMIN and CAP_NET_ADMIN in its own capability bounding set (the
// container's granted ceiling in the real deployment) and setpriv is on
// PATH — neither is true for a typical unprivileged `go test` run; this
// is meant to run inside a container configured to mirror the compose
// service's exact capability grant (cap_drop: ALL, cap_add: [SYS_ADMIN,
// NET_ADMIN]), the same shape docs/self-host-setup.md's sandbox
// integration-test harness already uses.
func TestCapabilityDrop_BrokerKeepsExactPairOrchestratorEndsEmpty(t *testing.T) {
	setprivPath, err := exec.LookPath("setpriv")
	if err != nil {
		t.Skip("setpriv not found on PATH")
	}

	bounding, err := capinfo.Bounding()
	if err != nil {
		t.Fatalf("read own capability bounding set: %v", err)
	}
	if !hasCap(bounding, "cap_sys_admin") || !hasCap(bounding, "cap_net_admin") {
		t.Skipf("this process's own capability bounding set (%v) doesn't include both cap_sys_admin and cap_net_admin; run inside a container configured with cap_drop: ALL, cap_add: [SYS_ADMIN, NET_ADMIN] to exercise this test", bounding)
	}

	// The broker: bounding set trimmed to exactly the container's granted
	// pair before spawn — setpriv can only ever DROP bounding-set
	// capabilities, never add one back, so this only narrows down to
	// {sys_admin, net_admin} because the skip-guard above already
	// confirmed both are present to narrow *from*.
	uid, caps := runCapReport(t, setprivPath, "--bounding-set=-all,+sys_admin,+net_admin", "--")
	if uid != 0 {
		t.Errorf("broker-shaped process uid = %d, want 0 (root)", uid)
	}
	wantBroker := []string{"cap_net_admin", "cap_sys_admin"}
	if !reflect.DeepEqual(caps, wantBroker) {
		t.Errorf("broker-shaped process CapEff = %v, want exactly %v", caps, wantBroker)
	}

	// The orchestrator: capability bounding set cleared entirely,
	// inheritable capabilities dropped, no_new_privs set, and switched to
	// a different, arbitrary unprivileged uid — mirroring
	// docker/entrypoint.sh's exec-time drop exactly. The uid needs no
	// /etc/passwd entry: setresuid works on any numeric uid.
	const orchestratorTestUID = 10001
	uid, caps = runCapReport(t, setprivPath,
		"--reuid="+strconv.Itoa(orchestratorTestUID),
		"--regid="+strconv.Itoa(orchestratorTestUID),
		"--clear-groups", "--inh-caps=-all", "--bounding-set=-all", "--no-new-privs", "--",
	)
	if uid != orchestratorTestUID {
		t.Errorf("orchestrator-shaped process uid = %d, want %d", uid, orchestratorTestUID)
	}
	if len(caps) != 0 {
		t.Errorf("orchestrator-shaped process CapEff = %v, want empty", caps)
	}
}
