package deploy

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"testing/fstest"
	"time"
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
// binary of the next release whose image a pull puts on the host, and a
// stack that is up and healthy.
func upgradeFixture(t *testing.T) (*fakeHost, Bundle, string) {
	t.Helper()
	f, b, dir := initedFixture(t)
	healthyStack().apply(t, f, dir)
	imagePulledByStream(f)
	f.dockerReplies[controlRunning] = dockerReply{stdout: "c0ffee\n"}
	return f, nextRelease(t, b), dir
}

// pinnedImage is the image the fake resolver pins for version.
func pinnedImage(version string) string {
	return imageRepository + ":" + version + "@" + testDigest
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
	wantStreamed(t, f, "pull "+pinnedImage(next.Version), composeUpArgs("triage-factory", "3"))
	// The new image was pulled and probed before the drain, and the drain
	// finished before compose recreated anything.
	probe, firstDrainPoll, lastDrainPoll, start := -1, -1, -1, -1
	for i, call := range f.dockerCalls {
		switch joined := strings.Join(call, " "); {
		case strings.HasPrefix(joined, "run --rm") && strings.HasSuffix(joined, pinnedImage(next.Version)):
			probe = i
		case joined == fleetList:
			if firstDrainPoll < 0 {
				firstDrainPoll = i
			}
			lastDrainPoll = i
		case strings.HasPrefix(joined, "compose -p triage-factory up"):
			start = i
		}
	}
	if probe < 0 || probe >= firstDrainPoll {
		t.Errorf("want the seccomp probe of the new image before the drain; calls: %q", f.dockerCalls)
	}
	if lastDrainPoll < 0 || lastDrainPoll >= start {
		t.Errorf("want the drain wait before compose up; calls: %q", f.dockerCalls)
	}
	wantLine(t, f.out.String(), "Seccomp profile", pass, "Docker starts a container")

	st, err := readState(dir)
	if err != nil {
		t.Fatalf("read state: %v", err)
	}
	wantImage := pinnedImage(next.Version)
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
			name: "a required value the new release adds that deploy cannot generate",
			arrange: func(t *testing.T, _ *fakeHost, next *Bundle, _ string) {
				requireInNewCompose(t, next, "TF_NEW_REQUIRED")
			},
			wantErr: "requires TF_NEW_REQUIRED, which .env does not set and deploy cannot generate",
		},
		{
			name: "an operator input .env lacks, with no terminal to ask on",
			arrange: func(t *testing.T, _ *fakeHost, _ *Bundle, dir string) {
				setEnvLine(t, dir, "GH_CLIENT_ID", "")
			},
			wantErr: ".env does not set GH_CLIENT_ID, which only you can supply",
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
		{
			name: "a new image that does not pull",
			arrange: func(_ *testing.T, f *fakeHost, _ *Bundle, _ string) {
				f.dockerReplies["pull"] = dockerReply{err: errors.New("exit status 1")}
			},
			wantErr: "Nothing was changed",
		},
		{
			name: "a new seccomp profile the host refuses",
			arrange: func(_ *testing.T, f *fakeHost, _ *Bundle, _ string) {
				f.dockerReplies["run"] = dockerReply{stderr: "unknown syscall\n", err: errors.New("exit status 125")}
			},
			wantErr: "seccomp profile is refused on this host, so its executor would not start; nothing was changed",
		},
		{
			name: "a pulled image docker cannot inspect",
			arrange: func(_ *testing.T, f *fakeHost, _ *Bundle, _ string) {
				f.onStream = nil
				f.dockerReplies["image inspect"] = dockerReply{err: errors.New("exit status 1")}
			},
			wantErr: "could not test v",
		},
		{
			name: "a value the deployment uses that .env has lost",
			arrange: func(t *testing.T, _ *fakeHost, _ *Bundle, dir string) {
				setEnvLine(t, dir, "POSTGRES_PASSWORD", "")
			},
			wantErr: ".env does not set POSTGRES_PASSWORD, which this deployment already uses",
		},
		{
			name: "a default GitHub host set to github.com",
			arrange: func(t *testing.T, _ *fakeHost, _ *Bundle, dir string) {
				p := filepath.Join(dir, envFile)
				raw, err := os.ReadFile(p)
				if err != nil {
					t.Fatal(err)
				}
				mustWrite(t, p, string(raw)+"\nTF_DEFAULT_GITHUB_HOST=https://github.com\n")
			},
			wantErr: ".env sets TF_DEFAULT_GITHUB_HOST to github.com",
		},
		{
			name: "a .env that is gone",
			arrange: func(t *testing.T, _ *fakeHost, _ *Bundle, dir string) {
				mustRemove(t, filepath.Join(dir, envFile))
			},
			wantErr: ".env does not set GOTRUE_JWT_KEYS, POSTGRES_PASSWORD, TF_SECRET_ENCRYPTION_KEY, TF_SESSION_ENCRYPTION_KEY, which",
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
			// Every compose call names the project, so this covers the
			// drain, which runs in the control container, and the start.
			if f.called("compose -p") {
				t.Errorf("a refused upgrade drove the stack: %q", f.dockerCalls)
			}
		})
	}
}

// TestUpgrade_StatePathOutsideTheDirectory: a state file that records a
// path outside the deployment directory is refused before anything runs,
// so the removal of files the new bundle dropped never reaches outside it.
func TestUpgrade_StatePathOutsideTheDirectory(t *testing.T) {
	f, next, dir := upgradeFixture(t)
	outside := filepath.Join(filepath.Dir(dir), "outside.txt")
	mustWrite(t, outside, "not the deployment's\n")
	st, err := readState(dir)
	if err != nil {
		t.Fatal(err)
	}
	st.Files["../outside.txt"] = sha256Hex([]byte("not the deployment's\n"))
	if err := writeState(dir, st); err != nil {
		t.Fatal(err)
	}
	if code := upgradeRun(t, f, next, dir); code != 1 {
		t.Fatalf("exit = %d; want 1\n%s%s", code, f.out, f.errOut)
	}
	if !strings.Contains(f.errOut.String(), "not a path inside the deployment directory") {
		t.Errorf("stderr = %q; want the path refused", f.errOut)
	}
	if _, err := os.Stat(outside); err != nil {
		t.Errorf("a file outside the deployment directory is gone: %v", err)
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
			check: "Executors", status: warn, detail: "control is not running, and the drain runs there",
		},
		{
			name: "an executor that reports no run count",
			arrange: func(f *fakeHost) {
				f.dockerReplies[fleetList] = dockerReply{stdout: fleetJSON("exec-a:none:null:1")}
			},
			args:  []string{"--drain-timeout", "1m"},
			check: "Runs", status: warn, detail: "1 executor(s) have reported no run count since the drain took effect after 1m0s",
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
			wantStreamed(t, f, "pull "+pinnedImage(next.Version), composeUpArgs("triage-factory", "1"))
			if tt.args != nil && f.clock.Sub(start) < defaultDrainTimeout/10 {
				t.Errorf("the drain waited %s; want the 1m timeout", f.clock.Sub(start))
			}
		})
	}
}

// TestUpgrade_DrainWaitsOnTheDrained: the wait counts only executors that
// are draining and still heartbeating, and trusts a zero run count only
// from a heartbeat that landed drainSettle after the drain, once the
// executor has stopped claiming.
func TestUpgrade_DrainWaitsOnTheDrained(t *testing.T) {
	f, next, dir := upgradeFixture(t)
	// exec-a's drain fails, so it keeps claiming and its runs never settle.
	f.dockerReplies[drainPrefix+"exec-a"] = dockerReply{stderr: "boom\n", err: errors.New("exit status 1")}
	f.dockerSeqs[fleetList] = []dockerReply{
		{stdout: fleetJSON("exec-a:none:2:1", "exec-b:none:0:1", "exec-c:none:3:1")},
		// exec-b reports no runs from a heartbeat older than its drain, and
		// exec-c has stopped heartbeating with its last count frozen.
		{stdout: fleetJSON("exec-a:none:2:1", "exec-b:until_restart:0:1", "exec-c:until_restart:3:120")},
		{stdout: fleetJSON("exec-a:none:2:1", "exec-b:until_restart:0:1", "exec-c:until_restart:3:125")},
	}
	start := f.clock
	if code := upgradeRun(t, f, next, dir); code != 0 {
		t.Fatalf("exit = %d; want 0\n%s%s", code, f.out, f.errOut)
	}
	if got := strings.Join(drainCalls(f), ","); got != "exec-a,exec-b,exec-c" {
		t.Errorf("drained %q; want exec-a, exec-b and exec-c tried", got)
	}
	out := f.out.String()
	wantLine(t, out, "Executors", warn, "could not drain exec-a (boom)")
	if !strings.Contains(out, "drained 2 until their restart; 1 not drained (exec-a)") {
		t.Errorf("the drain summary does not name the executor it could not drain:\n%s", out)
	}
	wantLine(t, out, "Runs", pass, "none in flight")
	// exec-b's heartbeat is always 1s old when listed, so its zero counts at
	// the first poll it landed drainSettle after the drain.
	want := pollInterval
	for want-time.Second < drainSettle {
		want += pollInterval
	}
	if waited := f.clock.Sub(start); waited != want {
		t.Errorf("waited %s; want %s: no trusting exec-b's zero before the drain settled, no waiting on exec-a or the stopped exec-c", waited, want)
	}
}

// TestUpgrade_InterruptedDrain: an interrupt during the drain wait undrains
// the executors the upgrade drained, and changes nothing else.
func TestUpgrade_InterruptedDrain(t *testing.T) {
	f, next, dir := upgradeFixture(t)
	f.dockerReplies[fleetList] = dockerReply{stdout: fleetJSON("exec-a:none:2:1", "exec-b:persistent:1:1")}
	interruptOnSleep(f)
	before := snapshotDir(t, dir)
	if code := upgradeRun(t, f, next, dir); code != 1 {
		t.Fatalf("exit = %d; want 1\n%s%s", code, f.out, f.errOut)
	}
	if !strings.Contains(f.errOut.String(), "interrupted; the drain was undone (1 executor(s)); nothing else was changed") {
		t.Errorf("stderr = %q; want it to say the drain was undone", f.errOut)
	}
	if got := undrainCalls(f); strings.Join(got, ",") != "exec-a" {
		t.Errorf("undrained %q; want exec-a alone, the one the upgrade drained", got)
	}
	if len(f.streamed) != 1 {
		t.Errorf("streamed %q; want the pull alone", f.streamed)
	}
	if after := snapshotDir(t, dir); !maps.Equal(before, after) {
		t.Error("the interrupted upgrade changed the deployment directory")
	}
}

// interruptOnSleep makes the next wait's sleep send the test process SIGINT,
// as Ctrl-C would, and return once that ends the wait's context. A sleep
// outside a drain, where nothing catches the signal, ends the test binary.
func interruptOnSleep(f *fakeHost) {
	f.sleep = func(ctx context.Context, _ time.Duration) error {
		if err := syscall.Kill(os.Getpid(), syscall.SIGINT); err != nil {
			panic(err)
		}
		<-ctx.Done()
		return ctx.Err()
	}
}

const undrainPrefix = "compose -p triage-factory exec -T control triagefactory instance undrain "

func undrainCalls(f *fakeHost) []string {
	var out []string
	for _, call := range f.dockerCalls {
		if joined := strings.Join(call, " "); strings.HasPrefix(joined, undrainPrefix) {
			out = append(out, strings.TrimPrefix(joined, undrainPrefix))
		}
	}
	return out
}

func TestLostValues(t *testing.T) {
	dir := t.TempDir()
	// GOTRUE_JWT_KEYS is not read by this compose file, so it is a value
	// generating adds rather than replaces.
	mustWrite(t, filepath.Join(dir, composeFile), `services:
  control:
    environment:
      DSN: postgres://u:${POSTGRES_PASSWORD:?missing}@db/x
      TF_SESSION_ENCRYPTION_KEY: ${TF_SESSION_ENCRYPTION_KEY:-}
      TF_SECRET_ENCRYPTION_KEY: ${TF_SECRET_ENCRYPTION_KEY:-}
`)
	env := envValues("POSTGRES_PASSWORD=\nTF_SESSION_ENCRYPTION_KEY_FILE=/run/secrets/k\n")
	if got := strings.Join(lostValues(env, dir), ","); got != "POSTGRES_PASSWORD,TF_SECRET_ENCRYPTION_KEY" {
		t.Errorf("lostValues = %q; want POSTGRES_PASSWORD and TF_SECRET_ENCRYPTION_KEY", got)
	}
	mustRemove(t, filepath.Join(dir, composeFile))
	if got := len(lostValues(env, dir)); got != 3 {
		t.Errorf("lostValues without a compose file = %d keys; want every unset one (3)", got)
	}
}

func TestUpgrade_NegativeDrainTimeout(t *testing.T) {
	f, next, dir := upgradeFixture(t)
	if code := upgradeRun(t, f, next, dir, "--drain-timeout", "-1s"); code != 2 {
		t.Fatalf("exit = %d; want 2\n%s%s", code, f.out, f.errOut)
	}
}

// requireInNewCompose makes next's docker-compose.yml require name, the way
// a release that adds a required setting would.
func requireInNewCompose(t *testing.T, next *Bundle, name string) {
	t.Helper()
	files := next.Files.(fstest.MapFS)
	compose := string(files[composeFile].Data)
	const anchor = "      TF_MODE: multi\n"
	if !strings.Contains(compose, anchor) {
		t.Fatalf("docker-compose.yml has no %q line to add a required value after", strings.TrimSpace(anchor))
	}
	compose = strings.Replace(compose, anchor, anchor+"      "+name+": ${"+name+":?missing "+name+"}\n", 1)
	files[composeFile] = &fstest.MapFile{Data: []byte(compose)}
}

// TestUpgrade_FillsWhatTheNewReleaseNeeds: a value boot needs that .env
// lacks, as when a release adds a generated secret, is filled before compose
// starts the new release, and nothing .env had changes.
func TestUpgrade_FillsWhatTheNewReleaseNeeds(t *testing.T) {
	f, next, dir := upgradeFixture(t)
	before, err := os.ReadFile(filepath.Join(dir, envFile))
	if err != nil {
		t.Fatal(err)
	}
	setEnvLine(t, dir, "TF_COOKIE_SECRET", "")

	if code := upgradeRun(t, f, next, dir); code != 0 {
		t.Fatalf("exit = %d; want 0\n%s%s", code, f.out, f.errOut)
	}
	wantLine(t, f.out.String(), "Environment", pass, next.Version+" adds 1 value(s) to .env: TF_COOKIE_SECRET")
	if got := envOf(t, dir, "TF_COOKIE_SECRET"); len(got) != 64 {
		t.Errorf("TF_COOKIE_SECRET = %q; want a generated 32-byte hex value", got)
	}
	for _, key := range append([]string{"TF_PUBLIC_URL", "GH_CLIENT_ID", "GH_CLIENT_SECRET", "GOTRUE_JWT_KEYS"}, generatedSecrets...) {
		if key == "TF_COOKIE_SECRET" {
			continue
		}
		if want := envValues(before).get(key); envOf(t, dir, key) != want {
			t.Errorf("upgrade changed %s, which .env already had", key)
		}
	}
	if info, err := os.Stat(filepath.Join(dir, envFile)); err != nil || info.Mode().Perm() != 0o600 {
		t.Errorf(".env mode = %v (%v); want 0600", info.Mode().Perm(), err)
	}
}

// TestUpgrade_RequiredValueFromTheEnvironment: compose interpolates from the
// environment deploy runs it in as well as .env, so a required value set
// there is not missing.
func TestUpgrade_RequiredValueFromTheEnvironment(t *testing.T) {
	f, next, dir := upgradeFixture(t)
	requireInNewCompose(t, &next, "TF_NEW_REQUIRED")
	f.getenv = func(key string) string {
		if key == "TF_NEW_REQUIRED" {
			return "set"
		}
		return ""
	}
	if code := upgradeRun(t, f, next, dir); code != 0 {
		t.Fatalf("exit = %d; want 0\n%s%s", code, f.out, f.errOut)
	}
}

func TestMissingRequired(t *testing.T) {
	compose := []byte(`services:
  control:
    environment:
      A: ${A:?missing A}
      B: ${B?unset B}
      C: ${C:-default}
      D: "${D:?} and ${A:?again}"
      # E: ${E:?only in a comment}
      F: ${F:?}
`)
	env := []byte("B=set\nF=\n")
	got := missingRequired(compose, env, func(key string) string {
		if key == "D" {
			return "from the environment"
		}
		return ""
	})
	if want := []string{"A", "F"}; strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("missingRequired = %q; want %q", got, want)
	}
}
