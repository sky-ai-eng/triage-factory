package deploy

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// initedFixture is a host and a deployment init rendered on it.
func initedFixture(t *testing.T) (*fakeHost, Bundle, string) {
	t.Helper()
	f := newFakeHost(t)
	b := releaseBundle(t)
	dir := filepath.Join(t.TempDir(), "deploy")
	if code := run(context.Background(), initArgs(t, dir), b, f.host); code != 0 {
		t.Fatalf("init exit = %d\n%s%s", code, f.out, f.errOut)
	}
	f.out.Reset()
	f.errOut.Reset()
	f.dockerCalls = nil
	f.resolved = nil
	// The image is not on the host and the stack is not up, until a test
	// says otherwise.
	f.dockerReplies["image inspect"] = dockerReply{stderr: "No such image\n", err: errors.New("exit status 1")}
	return f, b, dir
}

func doctorRun(t *testing.T, f *fakeHost, b Bundle, dir string) int {
	t.Helper()
	return run(context.Background(), []string{"doctor", "--dir", dir}, b, f.host)
}

// line returns the output line for the named check, or "".
func line(out, name string) string {
	for _, l := range strings.Split(out, "\n") {
		if strings.HasPrefix(strings.TrimSpace(l), "pass  "+name) ||
			strings.HasPrefix(strings.TrimSpace(l), "warn  "+name) ||
			strings.HasPrefix(strings.TrimSpace(l), "fail  "+name) ||
			strings.HasPrefix(strings.TrimSpace(l), "skip  "+name) {
			return strings.TrimSpace(l)
		}
	}
	return ""
}

func wantLine(t *testing.T, out, name string, want status, detail string) {
	t.Helper()
	l := line(out, name)
	if !strings.HasPrefix(l, string(want)+"  ") || !strings.Contains(l, detail) {
		t.Errorf("check %q = %q; want %s containing %q\n%s", name, l, want, detail, out)
	}
}

func TestDoctor_PlatformGate(t *testing.T) {
	for name, arrange := range map[string]func(*fakeHost){
		"macOS":         func(f *fakeHost) { f.goos = "darwin" },
		"no docker CLI": func(f *fakeHost) { f.noDockerCLI() },
	} {
		t.Run(name, func(t *testing.T) {
			f, b, dir := initedFixture(t)
			arrange(f)
			if code := doctorRun(t, f, b, dir); code != 1 {
				t.Fatalf("exit = %d; want 1\n%s", code, f.out)
			}
			if strings.Contains(f.out.String(), "Bundle") {
				t.Errorf("doctor went past a failed gate:\n%s", f.out)
			}
		})
	}
}

func TestDoctor_FreshDeployment(t *testing.T) {
	f, b, dir := initedFixture(t)
	if code := doctorRun(t, f, b, dir); code != 0 {
		t.Fatalf("exit = %d; want 0\n%s%s", code, f.out, f.errOut)
	}
	out := f.out.String()
	wantLine(t, out, "Deployment", pass, b.Version+", compose project triage-factory")
	wantLine(t, out, "Image", pass, "@"+testDigest)
	wantLine(t, out, "Rendered files", pass, "match what init rendered")
	wantLine(t, out, "Binary version", pass, b.Version)
	wantLine(t, out, "Seccomp profile", skip, "is not on this host yet")
	wantLine(t, out, "Containers", skip, "has no containers")
	if line(out, "Port 3000") != "" {
		t.Error("doctor checked port 3000 on a deployment whose own control service holds it")
	}
}

func TestDoctor_NoStateFile(t *testing.T) {
	f := newFakeHost(t)
	dir := t.TempDir()
	if code := doctorRun(t, f, releaseBundle(t), dir); code != 1 {
		t.Fatalf("exit = %d; want 1\n%s", code, f.out)
	}
	wantLine(t, f.out.String(), "Deployment", fail, "run deploy init there")
	wantLine(t, f.out.String(), "Port 3000", pass, "free")
}

func TestDoctor_BundleWarnings(t *testing.T) {
	t.Run("edited and missing files", func(t *testing.T) {
		f, b, dir := initedFixture(t)
		mustWrite(t, filepath.Join(dir, composeFile), "services: {}\n")
		if err := os.Remove(filepath.Join(dir, "docker", "observability", "tempo.yaml")); err != nil {
			t.Fatal(err)
		}
		if code := doctorRun(t, f, b, dir); code != 0 {
			t.Fatalf("exit = %d; want 0 (warnings do not fail)\n%s", code, f.out)
		}
		wantLine(t, f.out.String(), "Rendered files", warn, "edited: docker-compose.yml; missing: docker/observability/tempo.yaml")
	})
	t.Run("another binary version", func(t *testing.T) {
		f, b, dir := initedFixture(t)
		b.Version = "v99.0.0"
		if code := doctorRun(t, f, b, dir); code != 0 {
			t.Fatalf("exit = %d; want 0\n%s", code, f.out)
		}
		wantLine(t, f.out.String(), "Binary version", warn, "and this binary is v99.0.0; run `triagefactory deploy upgrade`")
	})
	t.Run("source build", func(t *testing.T) {
		f := newFakeHost(t)
		b := repoBundle(t, "dev")
		dir := filepath.Join(t.TempDir(), "deploy")
		if code := run(context.Background(), initArgs(t, dir, "--build-from", filepath.Join("..", "..")), b, f.host); code != 0 {
			t.Fatalf("init exit = %d\n%s%s", code, f.out, f.errOut)
		}
		f.out.Reset()
		if code := doctorRun(t, f, b, dir); code != 0 {
			t.Fatalf("exit = %d; want 0\n%s", code, f.out)
		}
		wantLine(t, f.out.String(), "Image", warn, "unverified source build")
	})
	t.Run("unreadable state file", func(t *testing.T) {
		f, b, dir := initedFixture(t)
		mustWrite(t, filepath.Join(dir, stateFile), `{"version":""}`)
		if code := doctorRun(t, f, b, dir); code != 1 {
			t.Fatalf("exit = %d; want 1\n%s", code, f.out)
		}
		wantLine(t, f.out.String(), "Deployment", fail, "version is empty")
	})
}

func TestDoctor_Seccomp(t *testing.T) {
	t.Run("accepted", func(t *testing.T) {
		f, b, dir := initedFixture(t)
		delete(f.dockerReplies, "image inspect")
		if code := doctorRun(t, f, b, dir); code != 0 {
			t.Fatalf("exit = %d; want 0\n%s", code, f.out)
		}
		wantLine(t, f.out.String(), "Seccomp profile", pass, "Docker starts a container")
		st, _ := readState(dir)
		want := []string{"run", "--rm", "--pull=never", "--network=none", "--security-opt",
			"seccomp=" + filepath.Join(dir, "docker", "seccomp-profile.json"), "--entrypoint", "true", st.imageRef()}
		found := false
		for _, call := range f.dockerCalls {
			if strings.Join(call, " ") == strings.Join(want, " ") {
				found = true
			}
		}
		if !found {
			t.Errorf("doctor did not run %q; calls: %q", want, f.dockerCalls)
		}
	})
	t.Run("refused", func(t *testing.T) {
		f, b, dir := initedFixture(t)
		delete(f.dockerReplies, "image inspect")
		f.dockerReplies["run"] = dockerReply{stderr: "docker: Error response from daemon: unknown syscall\n", err: errors.New("exit status 125")}
		if code := doctorRun(t, f, b, dir); code != 1 {
			t.Fatalf("exit = %d; want 1\n%s", code, f.out)
		}
		wantLine(t, f.out.String(), "Seccomp profile", fail, "unknown syscall")
	})
	t.Run("profile missing", func(t *testing.T) {
		f, b, dir := initedFixture(t)
		delete(f.dockerReplies, "image inspect")
		if err := os.Remove(filepath.Join(dir, "docker", "seccomp-profile.json")); err != nil {
			t.Fatal(err)
		}
		if code := doctorRun(t, f, b, dir); code != 1 {
			t.Fatalf("exit = %d; want 1\n%s", code, f.out)
		}
		wantLine(t, f.out.String(), "Seccomp profile", fail, "is missing")
	})
}

// stackFixture is a running stack: containers as docker inspect reports
// them, and HTTP servers standing in for the control service, GoTrue and the
// public URL.
type stackFixture struct {
	containers []map[string]any
	control    int
	jwks       string
	public     int
}

func healthyStack() *stackFixture {
	c := func(name, status string, exit int, health, restart string) map[string]any {
		state := map[string]any{"Status": status, "ExitCode": exit}
		if health != "" {
			state["Health"] = map[string]any{"Status": health}
		}
		return map[string]any{"Name": "/" + name, "State": state, "HostConfig": map[string]any{"RestartPolicy": map[string]any{"Name": restart}}}
	}
	return &stackFixture{
		containers: []map[string]any{
			c("triage-factory-postgres-1", "running", 0, "healthy", "unless-stopped"),
			c("triage-factory-postgres-postinit-1", "exited", 0, "", "no"),
			c("triage-factory-control-1", "running", 0, "healthy", "unless-stopped"),
			c("triage-factory-executor-1", "running", 0, "healthy", "unless-stopped"),
			c("triage-factory-tempo-1", "running", 0, "", "unless-stopped"),
		},
		control: http.StatusOK,
		jwks:    `{"keys":[{"kty":"RSA","kid":"a"}]}`,
		public:  http.StatusOK,
	}
}

func (s *stackFixture) apply(t *testing.T, f *fakeHost, dir string) {
	t.Helper()
	ids := make([]string, len(s.containers))
	for i := range ids {
		ids[i] = strings.Repeat(string(rune('a'+i)), 12)
	}
	inspect, _ := json.Marshal(s.containers)
	f.dockerReplies["ps"] = dockerReply{stdout: strings.Join(ids, "\n") + "\n"}
	f.dockerReplies["inspect"] = dockerReply{stdout: string(inspect)}

	control := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/health" {
			http.NotFound(w, r)
			return
		}
		w.WriteHeader(s.control)
	}))
	t.Cleanup(control.Close)
	gotrue := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/.well-known/jwks.json" {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte(s.jwks))
	}))
	t.Cleanup(gotrue.Close)
	public := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(s.public) }))
	t.Cleanup(public.Close)

	f.controlURL, f.gotrueURL = control.URL, gotrue.URL
	setEnvLine(t, dir, "TF_PUBLIC_URL", public.URL)
}

func TestDoctor_RunningStack(t *testing.T) {
	tests := []struct {
		name     string
		arrange  func(s *stackFixture)
		wantCode int
		check    string
		status   status
		detail   string
	}{
		{name: "healthy", check: "Containers", status: pass, detail: "all 5 running and healthy, or finished"},
		{name: "control health", check: "Control health", status: pass, detail: "answered 200"},
		{name: "jwks", check: "GoTrue JWKS", status: pass, detail: "1 signing key"},
		{name: "public URL", check: "Public URL", status: pass, detail: "answered 200"},
		{
			name: "unhealthy service",
			arrange: func(s *stackFixture) {
				s.containers[3]["State"].(map[string]any)["Health"] = map[string]any{"Status": "unhealthy"}
			},
			wantCode: 1, check: "Containers", status: fail, detail: "triage-factory-executor-1 is unhealthy",
		},
		{
			name:     "failed one-shot",
			arrange:  func(s *stackFixture) { s.containers[1]["State"].(map[string]any)["ExitCode"] = 1 },
			wantCode: 1, check: "Containers", status: fail, detail: "triage-factory-postgres-postinit-1 exited 1",
		},
		{
			name: "stopped service",
			arrange: func(s *stackFixture) {
				s.containers[2]["State"] = map[string]any{"Status": "exited", "ExitCode": 3}
			},
			wantCode: 1, check: "Containers", status: fail, detail: "triage-factory-control-1 exited 3",
		},
		{
			name: "restarting service",
			arrange: func(s *stackFixture) {
				s.containers[2]["State"] = map[string]any{"Status": "restarting", "ExitCode": 3}
			},
			wantCode: 1, check: "Containers", status: fail, detail: "triage-factory-control-1 is restarting",
		},
		{
			name: "starting service",
			arrange: func(s *stackFixture) {
				s.containers[3]["State"].(map[string]any)["Health"] = map[string]any{"Status": "starting"}
			},
			check: "Containers", status: warn, detail: "still starting: triage-factory-executor-1",
		},
		{
			name:     "control health failing",
			arrange:  func(s *stackFixture) { s.control = http.StatusServiceUnavailable },
			wantCode: 1, check: "Control health", status: fail, detail: "answered 503",
		},
		{
			name:     "no signing key",
			arrange:  func(s *stackFixture) { s.jwks = `{"keys":[]}` },
			wantCode: 1, check: "GoTrue JWKS", status: fail, detail: "publishes no signing key",
		},
		{
			name:    "public URL not reachable",
			arrange: func(s *stackFixture) { s.public = http.StatusBadGateway },
			check:   "Public URL", status: warn, detail: "answered 502; check the reverse proxy",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f, b, dir := initedFixture(t)
			s := healthyStack()
			if tt.arrange != nil {
				tt.arrange(s)
			}
			s.apply(t, f, dir)
			if code := doctorRun(t, f, b, dir); code != tt.wantCode {
				t.Fatalf("exit = %d; want %d\n%s", code, tt.wantCode, f.out)
			}
			wantLine(t, f.out.String(), tt.check, tt.status, tt.detail)
			for _, call := range f.dockerCalls {
				if call[0] == "ps" && !strings.Contains(strings.Join(call, " "), "label=com.docker.compose.project=triage-factory") {
					t.Errorf("docker ps did not select the deployment's project: %q", call)
				}
			}
		})
	}
}
