package deploy

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"testing/fstest"
)

// nextRelease is b as the next patch release would carry it: the version
// one higher, and docker-compose.yml pinning that version's image.
func nextRelease(t *testing.T, b Bundle) Bundle {
	t.Helper()
	v, ok := parseRelease(b.Version)
	if !ok {
		t.Fatalf("%q is not a release", b.Version)
	}
	next := fmt.Sprintf("v%d.%d.%s", v[0], v[1], strconv.Itoa(v[2]+1))
	files := fstest.MapFS{}
	err := fs.WalkDir(b.Files, ".", func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		data, err := fs.ReadFile(b.Files, path)
		if err != nil {
			return err
		}
		if path == composeFile {
			data = []byte(strings.ReplaceAll(string(data), imageRepository+":"+b.Version, imageRepository+":"+next))
		}
		files[path] = &fstest.MapFile{Data: data}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return Bundle{Files: files, Version: next}
}

func upgradeRun(t *testing.T, f *fakeHost, b Bundle, dir string, extra ...string) int {
	t.Helper()
	return run(context.Background(), append([]string{"upgrade", "--dir", dir}, extra...), b, f.host)
}

const (
	controlRunning = "compose -p triage-factory ps --status running --quiet control"
	fleetList      = "compose -p triage-factory exec -T control triagefactory instance list --json"
	drainPrefix    = "compose -p triage-factory exec -T control triagefactory instance drain --until-restart "
)

// fleetJSON is `instance list --json` for a control instance and the
// executors given as id:drain:runs:heartbeat-age-seconds.
func fleetJSON(executors ...string) string {
	rows := []string{`{"id":"ctl","role":"control","version":"v1","drain":"none","active_runs":null,"heartbeat_age_seconds":1}`}
	for _, e := range executors {
		p := strings.Split(e, ":")
		rows = append(rows, fmt.Sprintf(`{"id":%q,"role":"executor","version":"v1","drain":%q,"active_runs":%s,"heartbeat_age_seconds":%s}`, p[0], p[1], p[2], p[3]))
	}
	return "[" + strings.Join(rows, ",") + "]\n"
}

// upgradeFixture is a deployment init rendered at the repo's release, a
// binary of the next release, and a stack that is up and healthy.
func upgradeFixture(t *testing.T) (*fakeHost, Bundle, string) {
	t.Helper()
	f, b, dir := initedFixture(t)
	healthyStack().apply(t, f, dir)
	f.dockerReplies[controlRunning] = dockerReply{stdout: "c0ffee\n"}
	return f, nextRelease(t, b), dir
}

func drainCalls(f *fakeHost) []string {
	var out []string
	for _, call := range f.dockerCalls {
		if joined := strings.Join(call, " "); strings.HasPrefix(joined, drainPrefix) {
			out = append(out, strings.TrimPrefix(joined, drainPrefix))
		}
	}
	return out
}

func TestUpgrade_ToTheNextRelease(t *testing.T) {
	f, next, dir := upgradeFixture(t)
	from, _ := readState(dir)
	// A count set by an earlier up survives an upgrade that passes none.
	from.Executors = 3
	if err := writeState(dir, from); err != nil {
		t.Fatal(err)
	}
	f.dockerSeqs[fleetList] = []dockerReply{
		// exec-a is working, exec-b an operator drained, exec-gone a
		// stopped instance whose row the registry still holds.
		{stdout: fleetJSON("exec-a:none:1:2", "exec-b:persistent:0:3", "exec-gone:none:2:86400")},
		{stdout: fleetJSON("exec-a:until_restart:1:2", "exec-b:persistent:0:3", "exec-gone:none:2:86400")},
		{stdout: fleetJSON("exec-a:until_restart:0:2", "exec-b:persistent:0:3", "exec-gone:none:2:86400")},
	}

	if code := upgradeRun(t, f, next, dir); code != 0 {
		t.Fatalf("exit = %d; want 0\n%s%s", code, f.out, f.errOut)
	}

	if got := drainCalls(f); strings.Join(got, ",") != "exec-a" {
		t.Errorf("drained %q; want exec-a alone (exec-b keeps the operator's drain, exec-gone is not live)", got)
	}
	if f.called("compose -p triage-factory exec -T control triagefactory instance undrain") {
		t.Error("upgrade undrained an executor; the drain until restart ends on its own")
	}
	if strings.Join(f.resolved, ",") != next.Version {
		t.Errorf("resolved %q; want the new version %s", f.resolved, next.Version)
	}
	wantStreamed(t, f, composeUpArgs("triage-factory", "3"))
	// The drain finished before compose recreated anything.
	lastDrainPoll, start := -1, -1
	for i, call := range f.dockerCalls {
		switch joined := strings.Join(call, " "); {
		case joined == fleetList:
			lastDrainPoll = i
		case strings.HasPrefix(joined, "compose -p triage-factory up"):
			start = i
		}
	}
	if lastDrainPoll < 0 || lastDrainPoll >= start {
		t.Errorf("want the drain wait before compose up; calls: %q", f.dockerCalls)
	}

	st, err := readState(dir)
	if err != nil {
		t.Fatalf("read state: %v", err)
	}
	wantImage := imageRepository + ":" + next.Version + "@" + testDigest
	if st.Version != next.Version || st.Image == nil || *st.Image != wantImage || st.Executors != 3 || st.Project != "triage-factory" {
		t.Errorf("state = %+v; want version %s, image %s, 3 executors", st, next.Version, wantImage)
	}
	compose, _ := os.ReadFile(filepath.Join(dir, composeFile))
	if strings.Count(string(compose), "image: "+wantImage) != 2 {
		t.Errorf("the re-rendered compose file does not pin %s for control and executor", wantImage)
	}
	if edited, missing, _ := changedFiles(dir, st); len(edited)+len(missing) > 0 {
		t.Errorf("the state's hashes do not match the files written: edited %q, missing %q", edited, missing)
	}
	out := f.out.String()
	wantLine(t, out, "Executors", pass, "drained 1 until their restart; 1 already draining (exec-b)")
	wantLine(t, out, "Runs", pass, "none in flight")
	wantLine(t, out, "Containers", pass, "running and healthy")
	if !strings.Contains(out, "from "+from.Version+" to "+next.Version) {
		t.Errorf("upgrade did not print the old and new versions:\n%s", out)
	}
}

func TestUpgrade_AlreadyAtTheVersion(t *testing.T) {
	f, b, dir := initedFixture(t)
	before := snapshotDir(t, dir)
	if code := upgradeRun(t, f, b, dir); code != 0 {
		t.Fatalf("exit = %d; want 0\n%s%s", code, f.out, f.errOut)
	}
	if !strings.Contains(f.out.String(), "is already at "+b.Version) {
		t.Errorf("stdout = %q; want it to say the deployment is already at %s", f.out, b.Version)
	}
	if len(f.resolved) > 0 || f.called("compose -p") {
		t.Errorf("an upgrade to the same version did something: resolved %q, calls %q", f.resolved, f.dockerCalls)
	}
	if after := snapshotDir(t, dir); fmt.Sprint(after) != fmt.Sprint(before) {
		t.Error("an upgrade to the same version changed the directory")
	}
}

// TestUpgrade_Refusals: every refusal comes before anything changes — no
// file written, no executor drained, compose never started.
func TestUpgrade_Refusals(t *testing.T) {
	tests := []struct {
		name    string
		arrange func(t *testing.T, f *fakeHost, next *Bundle, dir string)
		wantErr string
	}{
		{
			name:    "an older binary",
			arrange: func(_ *testing.T, _ *fakeHost, next *Bundle, _ string) { next.Version = "v0.0.1" },
			wantErr: "which is older; install the v",
		},
		{
			name:    "a binary that is not a release",
			arrange: func(_ *testing.T, _ *fakeHost, next *Bundle, _ string) { next.Version = "dev" },
			wantErr: "not a release",
		},
		{
			name: "an image that does not verify",
			arrange: func(_ *testing.T, f *fakeHost, _ *Bundle, _ string) {
				f.resolveImage = func(context.Context, string) (string, error) {
					return "", errors.New("has no signature that verifies")
				}
			},
			wantErr: "has no signature that verifies",
		},
		{
			name: "an edited rendered file",
			arrange: func(t *testing.T, _ *fakeHost, _ *Bundle, dir string) {
				mustWrite(t, filepath.Join(dir, composeFile), "services: {}\n")
			},
			wantErr: "edited: docker-compose.yml. Changes belong in compose.override.yml",
		},
		{
			name: "a deleted rendered file",
			arrange: func(t *testing.T, _ *fakeHost, _ *Bundle, dir string) {
				mustRemove(t, filepath.Join(dir, "docker", "observability", "tempo.yaml"))
			},
			wantErr: "missing: docker/observability/tempo.yaml",
		},
		{
			name:    "a host that is not Linux",
			arrange: func(_ *testing.T, f *fakeHost, _ *Bundle, _ string) { f.goos = "darwin" },
			wantErr: "failed preflight; nothing was changed",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f, next, dir := upgradeFixture(t)
			tt.arrange(t, f, &next, dir)
			before := snapshotDir(t, dir)
			if code := upgradeRun(t, f, next, dir); code != 1 {
				t.Fatalf("exit = %d; want 1\n%s%s", code, f.out, f.errOut)
			}
			if !strings.Contains(f.errOut.String(), tt.wantErr) {
				t.Errorf("stderr = %q; want it to contain %q", f.errOut, tt.wantErr)
			}
			if after := snapshotDir(t, dir); fmt.Sprint(after) != fmt.Sprint(before) {
				t.Error("a refused upgrade changed the directory")
			}
			if f.called("compose -p") || len(f.streamed) > 0 {
				t.Errorf("a refused upgrade drove the stack: %q", f.dockerCalls)
			}
		})
	}
}

func TestUpgrade_SourceBuildRefuses(t *testing.T) {
	f := newFakeHost(t)
	b := repoBundle(t, "dev")
	dir := filepath.Join(t.TempDir(), "deploy")
	if code := run(context.Background(), initArgs(t, dir, "--build-from", filepath.Join("..", "..")), b, f.host); code != 0 {
		t.Fatalf("init exit = %d\n%s%s", code, f.out, f.errOut)
	}
	if code := upgradeRun(t, f, releaseBundle(t), dir); code != 1 {
		t.Fatalf("exit = %d; want 1\n%s%s", code, f.out, f.errOut)
	}
	if !strings.Contains(f.errOut.String(), "run `triagefactory deploy up`, which rebuilds it") {
		t.Errorf("stderr = %q; want it to point at deploy up", f.errOut)
	}
}

func TestUpgrade_ForceOverwritesEditedFiles(t *testing.T) {
	f, next, dir := upgradeFixture(t)
	mustWrite(t, filepath.Join(dir, composeFile), "services: {}\n")
	if code := upgradeRun(t, f, next, dir, "--force"); code != 0 {
		t.Fatalf("exit = %d; want 0\n%s%s", code, f.out, f.errOut)
	}
	wantLine(t, f.out.String(), "Rendered files", warn, "edited: docker-compose.yml; --force overwrites them")
	st, _ := readState(dir)
	if edited, _, _ := changedFiles(dir, st); len(edited) > 0 {
		t.Errorf("--force left %q as the operator edited it", edited)
	}
}

// TestUpgrade_RemovesFilesTheNewBundleDropped: a file the old release
// rendered and the new one does not ship is deleted, and leaves the state.
func TestUpgrade_RemovesFilesTheNewBundleDropped(t *testing.T) {
	f, next, dir := upgradeFixture(t)
	const dropped = "docker/observability/tempo.yaml"
	delete(next.Files.(fstest.MapFS), dropped)
	if code := upgradeRun(t, f, next, dir); code != 0 {
		t.Fatalf("exit = %d; want 0\n%s%s", code, f.out, f.errOut)
	}
	if _, err := os.Stat(filepath.Join(dir, filepath.FromSlash(dropped))); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("%s is still there (stat: %v)", dropped, err)
	}
	if st, _ := readState(dir); st.Files[dropped] != "" {
		t.Errorf("the state still records %s", dropped)
	}
}

// TestUpgrade_DrainNeverStopsTheUpgrade: whatever the drain runs into, the
// upgrade carries on, because stopping an executor hands its runs off.
func TestUpgrade_DrainNeverStopsTheUpgrade(t *testing.T) {
	tests := []struct {
		name    string
		arrange func(f *fakeHost)
		args    []string
		check   string
		status  status
		detail  string
	}{
		{
			name: "runs outlast the timeout",
			arrange: func(f *fakeHost) {
				f.dockerReplies[fleetList] = dockerReply{stdout: fleetJSON("exec-a:none:2:1")}
			},
			args:  []string{"--drain-timeout", "1m"},
			check: "Runs", status: warn, detail: "2 still in flight after 1m0s; continuing",
		},
		{
			name: "a control binary that has no --json",
			arrange: func(f *fakeHost) {
				f.dockerReplies[fleetList] = dockerReply{stdout: "ID  ROLE  VERSION\nctl  control  v1\n"}
			},
			check: "Executors", status: warn, detail: "a control binary from before --json existed prints a table",
		},
		{
			name: "a drain that fails",
			arrange: func(f *fakeHost) {
				f.dockerSeqs[fleetList] = []dockerReply{{stdout: fleetJSON("exec-a:none:0:1")}}
				f.dockerReplies[drainPrefix] = dockerReply{stderr: "boom\n", err: errors.New("exit status 1")}
			},
			check: "Executors", status: warn, detail: "could not drain exec-a (boom)",
		},
		{
			name: "control is not running",
			arrange: func(f *fakeHost) {
				f.dockerReplies[controlRunning] = dockerReply{}
			},
			check: "Executors", status: skip, detail: "control is not running",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f, next, dir := upgradeFixture(t)
			tt.arrange(f)
			start := f.clock
			if code := upgradeRun(t, f, next, dir, tt.args...); code != 0 {
				t.Fatalf("exit = %d; want 0\n%s%s", code, f.out, f.errOut)
			}
			wantLine(t, f.out.String(), tt.check, tt.status, tt.detail)
			wantStreamed(t, f, composeUpArgs("triage-factory", "1"))
			if tt.args != nil && f.clock.Sub(start) < defaultDrainTimeout/10 {
				t.Errorf("the drain waited %s; want the 1m timeout", f.clock.Sub(start))
			}
		})
	}
}

func TestUpgrade_NegativeDrainTimeout(t *testing.T) {
	f, next, dir := upgradeFixture(t)
	if code := upgradeRun(t, f, next, dir, "--drain-timeout", "-1s"); code != 2 {
		t.Fatalf("exit = %d; want 2\n%s%s", code, f.out, f.errOut)
	}
}
