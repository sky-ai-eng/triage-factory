package deploy

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
)

func upRun(t *testing.T, f *fakeHost, b Bundle, dir string, extra ...string) int {
	t.Helper()
	return run(context.Background(), append([]string{"up", "--dir", dir}, extra...), b, f.host)
}

// composeUpArgs is the compose call up and upgrade make for a project and
// executor count.
func composeUpArgs(project, executors string) string {
	return "compose -p " + project + " up -d --scale executor=" + executors
}

// wantStreamed asserts the streamed docker calls, space-joined, in order.
func wantStreamed(t *testing.T, f *fakeHost, want ...string) {
	t.Helper()
	var got []string
	for _, c := range f.streamed {
		got = append(got, strings.Join(c.args, " "))
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("streamed docker calls = %q; want %q", got, want)
	}
}

// imagePulledByStream makes a streamed docker pull put the image on the host.
func imagePulledByStream(f *fakeHost) {
	f.onStream = func(args []string) {
		if args[0] == "pull" {
			delete(f.dockerReplies, "image inspect")
		}
	}
}

func TestUp_FreshDeployment(t *testing.T) {
	f, b, dir := initedFixture(t)
	healthyStack().apply(t, f, dir)
	imagePulledByStream(f)
	st, _ := readState(dir)

	if code := upRun(t, f, b, dir); code != 0 {
		t.Fatalf("exit = %d; want 0\n%s%s", code, f.out, f.errOut)
	}
	if want := []string{b.Version + " " + *st.Image}; strings.Join(f.verified, ",") != strings.Join(want, ",") {
		t.Errorf("verified %q; want the pinned digest re-verified: %q", f.verified, want)
	}
	wantStreamed(t, f, "pull "+*st.Image, composeUpArgs("triage-factory", "1"))
	if f.streamed[1].dir != dir {
		t.Errorf("compose ran in %q; want the deployment directory %q", f.streamed[1].dir, dir)
	}
	out := f.out.String()
	wantLine(t, out, "Signature", pass, "verified: "+signerIdentity(b.Version))
	wantLine(t, out, "Seccomp profile", pass, "Docker starts a container")
	wantLine(t, out, "Containers", pass, "running and healthy")
	wantLine(t, out, "Control health", pass, "answered 200")
	wantLine(t, out, "GoTrue JWKS", pass, "1 signing key")
	publicURL := envOf(t, dir, "TF_PUBLIC_URL")
	if !strings.Contains(out, "is up at "+publicURL) || !strings.Contains(out, "sign in with GitHub") {
		t.Errorf("up did not print the public URL and the next step:\n%s", out)
	}
	// The seccomp probe ran on the pulled image, before compose started it.
	pull, probe, start := -1, -1, -1
	for i, call := range f.dockerCalls {
		switch joined := strings.Join(call, " "); {
		case strings.HasPrefix(joined, "pull "):
			pull = i
		case strings.HasPrefix(joined, "run --rm"):
			probe = i
		case strings.HasPrefix(joined, "compose -p triage-factory up"):
			start = i
		}
	}
	if pull < 0 || pull >= probe || probe >= start {
		t.Errorf("want pull, then the seccomp probe, then compose up; calls: %q", f.dockerCalls)
	}
}

// TestUp_SecondUpChangesNothing: on a stack already up, up writes nothing,
// pulls nothing, and asks compose for the same state, which compose leaves
// as it is.
func TestUp_SecondUpChangesNothing(t *testing.T) {
	f, b, dir := initedFixture(t)
	healthyStack().apply(t, f, dir)
	delete(f.dockerReplies, "image inspect")
	if code := upRun(t, f, b, dir); code != 0 {
		t.Fatalf("first up exit = %d\n%s%s", code, f.out, f.errOut)
	}
	before := snapshotDir(t, dir)
	f.streamed = nil
	if code := upRun(t, f, b, dir); code != 0 {
		t.Fatalf("second up exit = %d\n%s%s", code, f.out, f.errOut)
	}
	wantStreamed(t, f, composeUpArgs("triage-factory", "1"))
	after := snapshotDir(t, dir)
	for path, data := range before {
		if after[path] != data {
			t.Errorf("a second up changed %s", path)
		}
	}
}

func TestUp_Executors(t *testing.T) {
	f, b, dir := initedFixture(t)
	healthyStack().apply(t, f, dir)
	delete(f.dockerReplies, "image inspect")

	if code := upRun(t, f, b, dir, "--executors", "3"); code != 0 {
		t.Fatalf("up --executors 3 exit = %d\n%s%s", code, f.out, f.errOut)
	}
	if st, _ := readState(dir); st.Executors != 3 {
		t.Errorf("state executors = %d; want 3", st.Executors)
	}
	if code := upRun(t, f, b, dir); code != 0 {
		t.Fatalf("up exit = %d\n%s%s", code, f.out, f.errOut)
	}
	wantStreamed(t, f, composeUpArgs("triage-factory", "3"), composeUpArgs("triage-factory", "3"))

	for _, bad := range []string{"0", "-1"} {
		f.streamed = nil
		if code := upRun(t, f, b, dir, "--executors", bad); code != 2 {
			t.Errorf("up --executors %s exit = %d; want 2", bad, code)
		}
		if len(f.streamed) > 0 {
			t.Errorf("up --executors %s started the stack", bad)
		}
	}
}

// TestUp_WarnsOnEditedRenderedFiles: compose runs the files as they are on
// disk, so up says when they are not the ones deploy rendered and verified.
func TestUp_WarnsOnEditedRenderedFiles(t *testing.T) {
	f, b, dir := initedFixture(t)
	healthyStack().apply(t, f, dir)
	delete(f.dockerReplies, "image inspect")
	mustWrite(t, filepath.Join(dir, composeFile), "services: {}\n")
	if code := upRun(t, f, b, dir); code != 0 {
		t.Fatalf("exit = %d; want 0\n%s%s", code, f.out, f.errOut)
	}
	wantLine(t, f.out.String(), "Rendered files", warn, "edited: docker-compose.yml; compose runs them as they are now")
}

func TestUp_ProjectFromState(t *testing.T) {
	f := newFakeHost(t)
	b := releaseBundle(t)
	dir := filepath.Join(t.TempDir(), "deploy")
	if code := run(context.Background(), initArgs(t, dir, "--project", "tf-prod"), b, f.host); code != 0 {
		t.Fatalf("init exit = %d\n%s%s", code, f.out, f.errOut)
	}
	healthyStack().apply(t, f, dir)
	if code := upRun(t, f, b, dir); code != 0 {
		t.Fatalf("exit = %d\n%s%s", code, f.out, f.errOut)
	}
	if !f.called(composeUpArgs("tf-prod", "1")) || !f.called("ps --all --quiet --filter label=com.docker.compose.project=tf-prod") {
		t.Errorf("up did not use the stored project; calls: %q", f.dockerCalls)
	}
}

func TestUp_Refusals(t *testing.T) {
	tests := []struct {
		name    string
		arrange func(t *testing.T, f *fakeHost, b *Bundle, dir string)
		wantErr string
	}{
		{
			name:    "a newer binary",
			arrange: func(_ *testing.T, _ *fakeHost, b *Bundle, _ string) { b.Version = "v99.0.0" },
			wantErr: "run `triagefactory deploy upgrade` to move the deployment to v99.0.0",
		},
		{
			name:    "an older binary",
			arrange: func(_ *testing.T, _ *fakeHost, b *Bundle, _ string) { b.Version = "v0.0.1" },
			wantErr: "install the v",
		},
		{
			name:    "a binary that is not a release",
			arrange: func(_ *testing.T, _ *fakeHost, b *Bundle, _ string) { b.Version = "dev" },
			wantErr: "run the v",
		},
		{
			name: "a pinned digest that does not verify",
			arrange: func(_ *testing.T, f *fakeHost, _ *Bundle, _ string) {
				f.verifyErr = errors.New("has no signature that verifies")
			},
			wantErr: "has no signature that verifies",
		},
		{
			name: "a seccomp profile the host refuses",
			arrange: func(_ *testing.T, f *fakeHost, _ *Bundle, _ string) {
				delete(f.dockerReplies, "image inspect")
				f.dockerReplies["run"] = dockerReply{stderr: "unknown syscall\n", err: errors.New("exit status 125")}
			},
			wantErr: "seccomp profile is refused",
		},
		{
			name:    "a host that is not Linux",
			arrange: func(_ *testing.T, f *fakeHost, _ *Bundle, _ string) { f.goos = "darwin" },
			wantErr: "failed preflight",
		},
		{
			name:    "a directory init never rendered",
			arrange: func(t *testing.T, _ *fakeHost, _ *Bundle, dir string) { mustRemove(t, filepath.Join(dir, stateFile)) },
			wantErr: "run `triagefactory deploy init` there first",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f, b, dir := initedFixture(t)
			tt.arrange(t, f, &b, dir)
			if code := upRun(t, f, b, dir); code != 1 {
				t.Fatalf("exit = %d; want 1\n%s%s", code, f.out, f.errOut)
			}
			if !strings.Contains(f.errOut.String(), tt.wantErr) {
				t.Errorf("stderr = %q; want it to contain %q", f.errOut, tt.wantErr)
			}
			if f.called("compose -p") || len(f.streamed) > 0 {
				t.Errorf("a refused up drove the stack: %q", f.dockerCalls)
			}
		})
	}
}

func TestUp_SourceBuild(t *testing.T) {
	f := newFakeHost(t)
	b := repoBundle(t, "dev")
	dir := filepath.Join(t.TempDir(), "deploy")
	if code := run(context.Background(), initArgs(t, dir, "--build-from", filepath.Join("..", "..")), b, f.host); code != 0 {
		t.Fatalf("init exit = %d\n%s%s", code, f.out, f.errOut)
	}
	f.dockerReplies["image inspect"] = dockerReply{err: errors.New("No such image")}
	healthyStack().apply(t, f, dir)
	// A source build has no release version, so a binary of any version
	// runs it.
	b.Version = "v99.0.0"
	if code := upRun(t, f, b, dir); code != 0 {
		t.Fatalf("exit = %d\n%s%s", code, f.out, f.errOut)
	}
	if len(f.verified) > 0 {
		t.Errorf("up verified a source build: %q", f.verified)
	}
	wantStreamed(t, f, composeUpArgs("triage-factory", "1")+" --build")
	wantLine(t, f.out.String(), "Source build", warn, "unverified source build")
	wantLine(t, f.out.String(), "Seccomp profile", skip, "is not on this host yet")
}

// TestUp_WaitsForTheStack: up keeps checking until the stack is healthy,
// and fails once healthTimeout passes without that.
func TestUp_WaitsForTheStack(t *testing.T) {
	t.Run("comes up", func(t *testing.T) {
		f, b, dir := initedFixture(t)
		healthyStack().apply(t, f, dir)
		delete(f.dockerReplies, "image inspect")
		f.dockerSeqs["ps --all"] = []dockerReply{{}, {}, f.dockerReplies["ps"]}
		start := f.clock
		if code := upRun(t, f, b, dir); code != 0 {
			t.Fatalf("exit = %d\n%s%s", code, f.out, f.errOut)
		}
		if waited := f.clock.Sub(start); waited != 2*pollInterval {
			t.Errorf("waited %s; want two polls (%s)", waited, 2*pollInterval)
		}
	})
	t.Run("never healthy", func(t *testing.T) {
		f, b, dir := initedFixture(t)
		s := healthyStack()
		s.containers[3]["State"].(map[string]any)["Health"] = map[string]any{"Status": "unhealthy"}
		s.apply(t, f, dir)
		delete(f.dockerReplies, "image inspect")
		start := f.clock
		if code := upRun(t, f, b, dir); code != 1 {
			t.Fatalf("exit = %d; want 1\n%s%s", code, f.out, f.errOut)
		}
		if waited := f.clock.Sub(start); waited < healthTimeout {
			t.Errorf("gave up after %s; want at least %s", waited, healthTimeout)
		}
		if !strings.Contains(f.errOut.String(), "not healthy after "+healthTimeout.String()) {
			t.Errorf("stderr = %q; want the health timeout", f.errOut)
		}
		wantLine(t, f.out.String(), "Containers", fail, "triage-factory-executor-1 is unhealthy")
	})
}

func TestCompareReleases(t *testing.T) {
	for _, tt := range []struct {
		a, b string
		want int
		ok   bool
	}{
		{"v1.2.3", "v1.2.3", 0, true},
		{"v1.10.0", "v1.9.9", 1, true},
		{"v1.2.3", "v2.0.0", -1, true},
		{"v1.2.10", "v1.2.9", 1, true},
		{"dev", "v1.2.3", 0, false},
		{"v1.2.3", "1.2.3", 0, false},
	} {
		got, ok := compareReleases(tt.a, tt.b)
		if got != tt.want || ok != tt.ok {
			t.Errorf("compareReleases(%q, %q) = (%d, %t); want (%d, %t)", tt.a, tt.b, got, ok, tt.want, tt.ok)
		}
	}
}
