package deploy

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/golang-jwt/jwt/v5"
	"gopkg.in/yaml.v3"
)

// TestInit_PlatformGate: on a host that cannot run the stack, init says which
// check failed, exits non-zero, and has written nothing — no directory
// created, nothing put in an existing empty one, no image resolved.
func TestInit_PlatformGate(t *testing.T) {
	tests := []struct {
		name     string
		arrange  func(f *fakeHost)
		wantLine string
	}{
		{"macOS", func(f *fakeHost) { f.goos = "darwin" }, "fail  Linux"},
		{"no docker CLI", func(f *fakeHost) { f.noDockerCLI() }, "fail  Docker Engine      the docker CLI is not on PATH"},
		{"daemon unreachable", func(f *fakeHost) {
			f.dockerReplies["version"] = dockerReply{stderr: "Cannot connect to the Docker daemon at unix:///var/run/docker.sock.\n", err: errors.New("exit status 1")}
		}, "fail  Docker Engine      not reachable: Cannot connect to the Docker daemon"},
		{"no compose plugin", func(f *fakeHost) {
			f.dockerReplies["compose version"] = dockerReply{stderr: "docker: 'compose' is not a docker command.\n", err: errors.New("exit status 1")}
		}, "fail  Docker Compose     `docker compose` is not available"},
		{"compose 1", func(f *fakeHost) {
			f.dockerReplies["compose version"] = dockerReply{stdout: "1.29.2\n"}
		}, "fail  Docker Compose     version 1.29.2; deploy needs 2 or later"},
		{"cgroup v1", func(f *fakeHost) { f.cgroupControllers = filepath.Join(t.TempDir(), "absent") }, "fail  cgroup v2"},
	}
	for _, tt := range tests {
		for _, existing := range []bool{false, true} {
			name := tt.name + "/new dir"
			if existing {
				name = tt.name + "/existing empty dir"
			}
			t.Run(name, func(t *testing.T) {
				f := newFakeHost(t)
				tt.arrange(f)
				dir := filepath.Join(t.TempDir(), "deploy")
				if existing {
					if err := os.Mkdir(dir, 0o755); err != nil {
						t.Fatal(err)
					}
				}
				if code := run(context.Background(), initArgs(t, dir), releaseBundle(t), f.host); code != 1 {
					t.Fatalf("exit = %d; want 1\n%s%s", code, f.out, f.errOut)
				}
				if !strings.Contains(f.out.String(), tt.wantLine) {
					t.Errorf("output does not name the failed check %q:\n%s", tt.wantLine, f.out)
				}
				if !strings.Contains(f.errOut.String(), "nothing was written") {
					t.Errorf("stderr = %q; want it to say nothing was written", f.errOut)
				}
				if existing {
					if entries, _ := os.ReadDir(dir); len(entries) != 0 {
						t.Errorf("init wrote %d entries into the directory", len(entries))
					}
				} else if _, err := os.Stat(dir); !errors.Is(err, fs.ErrNotExist) {
					t.Errorf("init created the directory: %v", err)
				}
				if len(f.resolved) > 0 {
					t.Error("init resolved the image on a host that failed the gate")
				}
			})
		}
	}
}

// TestInit_PortInUseOnlyWarns: a busy port 3000 is reported, and init goes on.
func TestInit_PortInUseOnlyWarns(t *testing.T) {
	f := newFakeHost(t)
	f.portFree = func(int) bool { return false }
	dir := filepath.Join(t.TempDir(), "deploy")
	if code := run(context.Background(), initArgs(t, dir), releaseBundle(t), f.host); code != 0 {
		t.Fatalf("exit = %d; want 0\n%s%s", code, f.out, f.errOut)
	}
	if !strings.Contains(f.out.String(), "warn  Port 3000") {
		t.Errorf("output does not warn about port 3000:\n%s", f.out)
	}
}

func TestInit_RendersLayout(t *testing.T) {
	f := newFakeHost(t)
	b := releaseBundle(t)
	dir := filepath.Join(t.TempDir(), "deploy")
	if code := run(context.Background(), initArgs(t, dir), b, f.host); code != 0 {
		t.Fatalf("exit = %d; want 0\n%s%s", code, f.out, f.errOut)
	}
	pinned := imageRepository + ":" + b.Version + "@" + testDigest

	// Layout: the bundle without .env.example, plus .env and the state file.
	got := snapshotDir(t, dir)
	for _, want := range []string{composeFile, seccompFile, "docker/observability/prometheus.yml", envFile, stateFile} {
		if _, ok := got[want]; !ok {
			t.Errorf("init did not write %s", want)
		}
	}
	if _, ok := got[envExampleFile]; ok {
		t.Errorf("init rendered %s; it belongs only in .env", envExampleFile)
	}
	if extra := len(got) - countBundleFiles(t, b) - 2; extra != countDirs(got) {
		t.Errorf("init wrote %d unexpected entries: %v", extra-countDirs(got), keys(got))
	}

	info, err := os.Stat(filepath.Join(dir, envFile))
	if err != nil {
		t.Fatal(err)
	}
	if mode := info.Mode().Perm(); mode != 0o600 {
		t.Errorf(".env mode = %o; want 600", mode)
	}
	for _, sub := range []string{"docker", "docker/observability"} {
		info, err := os.Stat(filepath.Join(dir, sub))
		if err != nil {
			t.Fatal(err)
		}
		if mode := info.Mode().Perm(); mode != 0o755 {
			t.Errorf("%s mode = %o; want 755 so the containers that mount it can read it", sub, mode)
		}
	}

	// Every value boot needs is set; each generated secret is 32 random hex
	// bytes and none repeats.
	seen := map[string]string{}
	for _, key := range generatedSecrets {
		v := envOf(t, dir, key)
		if !regexp.MustCompile(`^[0-9a-f]{64}$`).MatchString(v) {
			t.Errorf("%s = %q; want 64 hex chars", key, v)
		}
		if other, dup := seen[v]; dup {
			t.Errorf("%s repeats %s's value", key, other)
		}
		seen[v] = key
	}
	for key, want := range map[string]string{"TF_PUBLIC_URL": testPublicURL, "GH_CLIENT_ID": testClientID, "GH_CLIENT_SECRET": testSecret} {
		if v := envOf(t, dir, key); v != want {
			t.Errorf("%s = %q; want %q", key, v, want)
		}
	}
	for _, key := range []string{"GOTRUE_JWT_KEYS", "GOTRUE_JWT_SECRET", "TF_GOTRUE_SERVICE_ROLE_TOKEN"} {
		if envOf(t, dir, key) == "" {
			t.Errorf("%s is not set", key)
		}
	}
	tok, _, err := jwt.NewParser().ParseUnverified(envOf(t, dir, "TF_GOTRUE_SERVICE_ROLE_TOKEN"), jwt.MapClaims{})
	if err != nil {
		t.Fatalf("parse service-role token: %v", err)
	}
	if iss := tok.Claims.(jwt.MapClaims)["iss"]; iss != testPublicURL+"/auth/v1" {
		t.Errorf("service-role token iss = %v; want %s/auth/v1", iss, testPublicURL)
	}
	// The optional settings stay visible, with their notes.
	if !strings.Contains(got[envFile], "# TF_GITHUB_APP_ID=") || !strings.Contains(got[envFile], "TF_LICENSE=") {
		t.Error(".env lost .env.example's optional settings")
	}

	// The compose file pins the verified digest on both TF services, and
	// keeps the executor's hardening.
	compose := parseCompose(t, got[composeFile])
	if compose.Name != defaultProject {
		t.Errorf("compose name = %q; want %q", compose.Name, defaultProject)
	}
	for _, svc := range []string{"control", "executor"} {
		s := compose.Services[svc]
		if s.Image != pinned || s.Build != nil || s.PullPolicy != "" {
			t.Errorf("%s: image %q build %v pull_policy %q; want image %q only", svc, s.Image, s.Build, s.PullPolicy, pinned)
		}
	}
	if !strings.Contains(strings.Join(compose.Services["executor"].SecurityOpt, " "), "seccomp=docker/seccomp-profile.json") {
		t.Error("executor lost its seccomp profile")
	}
	if strings.Contains(got[composeFile], "x-release-please-version") {
		t.Error("rendered compose keeps release-please's marker; the pin is no longer the release's tag")
	}

	// The state file records what was rendered.
	var raw map[string]any
	if err := json.Unmarshal([]byte(got[stateFile]), &raw); err != nil {
		t.Fatalf("parse state: %v", err)
	}
	if raw["build_from"] != nil {
		t.Errorf("build_from = %v; want null", raw["build_from"])
	}
	st, err := readState(dir)
	if err != nil {
		t.Fatalf("readState: %v", err)
	}
	if st.Version != b.Version || st.imageRef() != pinned || st.Project != defaultProject || st.Executors != 1 {
		t.Errorf("state = %+v", st)
	}
	if edited, missing, err := changedFiles(dir, st); err != nil || len(edited)+len(missing) > 0 {
		t.Errorf("recorded hashes do not match the rendered files: edited %v missing %v err %v", edited, missing, err)
	}
	if len(st.Files) != countBundleFiles(t, b) {
		t.Errorf("state records %d files; want %d", len(st.Files), countBundleFiles(t, b))
	}
	if len(f.resolved) != 1 || f.resolved[0] != b.Version {
		t.Errorf("resolved %v; want exactly [%s]", f.resolved, b.Version)
	}
}

func TestInit_Project(t *testing.T) {
	f := newFakeHost(t)
	dir := filepath.Join(t.TempDir(), "deploy")
	if code := run(context.Background(), initArgs(t, dir, "--project", "tf-prod"), releaseBundle(t), f.host); code != 0 {
		t.Fatalf("exit = %d; want 0\n%s%s", code, f.out, f.errOut)
	}
	st, err := readState(dir)
	if err != nil {
		t.Fatal(err)
	}
	if st.Project != "tf-prod" {
		t.Errorf("state project = %q; want tf-prod", st.Project)
	}
	raw, _ := os.ReadFile(filepath.Join(dir, composeFile))
	if parseCompose(t, string(raw)).Name != "tf-prod" {
		t.Error("rendered compose does not carry the chosen project name")
	}
}

func TestInit_SecondRunChangesNothing(t *testing.T) {
	f := newFakeHost(t)
	b := releaseBundle(t)
	dir := filepath.Join(t.TempDir(), "deploy")
	args := initArgs(t, dir)
	if code := run(context.Background(), args, b, f.host); code != 0 {
		t.Fatalf("first init exit = %d\n%s%s", code, f.out, f.errOut)
	}
	before := snapshotDir(t, dir)
	envInfo, _ := os.Stat(filepath.Join(dir, envFile))

	for _, rerun := range [][]string{args, {"init", "--dir", dir}} {
		f.out.Reset()
		if code := run(context.Background(), rerun, b, f.host); code != 0 {
			t.Fatalf("re-run %q exit = %d\n%s%s", rerun, code, f.out, f.errOut)
		}
		if !strings.Contains(f.out.String(), "nothing changed") {
			t.Errorf("re-run did not say nothing changed:\n%s", f.out)
		}
	}
	after := snapshotDir(t, dir)
	if len(before) != len(after) {
		t.Fatalf("re-run changed the layout: %v -> %v", keys(before), keys(after))
	}
	for path, data := range before {
		if after[path] != data {
			t.Errorf("re-run changed %s", path)
		}
	}
	afterInfo, _ := os.Stat(filepath.Join(dir, envFile))
	if !afterInfo.ModTime().Equal(envInfo.ModTime()) {
		t.Error("re-run rewrote .env")
	}
	if len(f.resolved) != 1 {
		t.Errorf("re-run resolved the image again: %v", f.resolved)
	}
}

func TestInit_RerunFillsOnlyMissing(t *testing.T) {
	tests := []struct {
		name  string
		blank string
		check func(t *testing.T, dir string, before map[string]string)
	}{
		{name: "a generated secret", blank: "TF_COOKIE_SECRET"},
		{name: "the service-role token", blank: "TF_GOTRUE_SERVICE_ROLE_TOKEN", check: func(t *testing.T, dir string, before map[string]string) {
			// Minted against the keypair .env already had, which stays.
			keys := envOf(t, dir, "GOTRUE_JWT_KEYS")
			if keys != envValues(before[envFile]).get("GOTRUE_JWT_KEYS") {
				t.Fatal("GOTRUE_JWT_KEYS changed")
			}
			tok, _, err := jwt.NewParser().ParseUnverified(envOf(t, dir, "TF_GOTRUE_SERVICE_ROLE_TOKEN"), jwt.MapClaims{})
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(keys, tok.Header["kid"].(string)) {
				t.Error("new token is not signed by the existing key")
			}
		}},
		{name: "a required input", blank: "GH_CLIENT_ID"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFakeHost(t)
			b := releaseBundle(t)
			dir := filepath.Join(t.TempDir(), "deploy")
			args := initArgs(t, dir)
			if code := run(context.Background(), args, b, f.host); code != 0 {
				t.Fatalf("first init exit = %d\n%s%s", code, f.out, f.errOut)
			}
			setEnvLine(t, dir, tt.blank, "")
			before := snapshotDir(t, dir)

			f.out.Reset()
			if code := run(context.Background(), args, b, f.host); code != 0 {
				t.Fatalf("re-run exit = %d\n%s%s", code, f.out, f.errOut)
			}
			after := snapshotDir(t, dir)
			for path := range before {
				if path != envFile && after[path] != before[path] {
					t.Errorf("re-run changed %s", path)
				}
			}
			if envOf(t, dir, tt.blank) == "" {
				t.Fatalf("re-run did not fill %s", tt.blank)
			}
			beforeLines, afterLines := strings.Split(before[envFile], "\n"), strings.Split(after[envFile], "\n")
			if len(beforeLines) != len(afterLines) {
				t.Fatalf(".env went from %d to %d lines", len(beforeLines), len(afterLines))
			}
			for i := range beforeLines {
				if beforeLines[i] != afterLines[i] && !strings.HasPrefix(beforeLines[i], tt.blank+"=") {
					t.Errorf(".env line %d changed: %q -> %q", i+1, beforeLines[i], afterLines[i])
				}
			}
			if !strings.Contains(f.out.String(), "Added to") || !strings.Contains(f.out.String(), tt.blank) {
				t.Errorf("re-run did not name what it added:\n%s", f.out)
			}
			if tt.check != nil {
				tt.check(t, dir, before)
			}
		})
	}
}

// TestInit_RerunWarnsWhenAReplacementLosesData: a value whose replacement
// strands what a running deployment made under the old one is filled — init
// cannot know the old value — but the operator is told.
func TestInit_RerunWarnsWhenAReplacementLosesData(t *testing.T) {
	f := newFakeHost(t)
	b := releaseBundle(t)
	dir := filepath.Join(t.TempDir(), "deploy")
	args := initArgs(t, dir)
	if code := run(context.Background(), args, b, f.host); code != 0 {
		t.Fatalf("first init exit = %d\n%s%s", code, f.out, f.errOut)
	}
	setEnvLine(t, dir, "TF_SECRET_ENCRYPTION_KEY", "")
	f.out.Reset()
	if code := run(context.Background(), args, b, f.host); code != 0 {
		t.Fatalf("re-run exit = %d\n%s%s", code, f.out, f.errOut)
	}
	if !strings.Contains(f.out.String(), "warning: these values were missing and are new: TF_SECRET_ENCRYPTION_KEY") {
		t.Errorf("no warning for a lossy replacement:\n%s", f.out)
	}
}

// TestInit_RerunHonorsFileForm: a secret moved to a mounted file is set, and
// init does not put a plain copy back beside it.
func TestInit_RerunHonorsFileForm(t *testing.T) {
	f := newFakeHost(t)
	b := releaseBundle(t)
	dir := filepath.Join(t.TempDir(), "deploy")
	args := initArgs(t, dir)
	if code := run(context.Background(), args, b, f.host); code != 0 {
		t.Fatalf("first init exit = %d\n%s%s", code, f.out, f.errOut)
	}
	setEnvLine(t, dir, "TF_SECRET_ENCRYPTION_KEY", "")
	p := filepath.Join(dir, envFile)
	raw, _ := os.ReadFile(p)
	if err := os.WriteFile(p, append(raw, []byte("TF_SECRET_ENCRYPTION_KEY_FILE=/run/secrets/enc\n")...), 0o600); err != nil {
		t.Fatal(err)
	}
	f.out.Reset()
	if code := run(context.Background(), args, b, f.host); code != 0 {
		t.Fatalf("re-run exit = %d\n%s%s", code, f.out, f.errOut)
	}
	if envOf(t, dir, "TF_SECRET_ENCRYPTION_KEY") != "" {
		t.Error("init generated a plain TF_SECRET_ENCRYPTION_KEY beside its _FILE form")
	}
}

func TestInit_BuildFrom(t *testing.T) {
	f := newFakeHost(t)
	b := repoBundle(t, "dev")
	checkout, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(t.TempDir(), "deploy")
	if code := run(context.Background(), initArgs(t, dir, "--build-from", filepath.Join("..", "..")), b, f.host); code != 0 {
		t.Fatalf("exit = %d; want 0\n%s%s", code, f.out, f.errOut)
	}
	if len(f.resolved) > 0 {
		t.Error("a source build resolved a release image")
	}
	if !strings.Contains(f.out.String(), "unverified source build") {
		t.Errorf("init did not say the build is unverified:\n%s", f.out)
	}
	raw, _ := os.ReadFile(filepath.Join(dir, composeFile))
	compose := parseCompose(t, string(raw))
	for _, svc := range []string{"control", "executor"} {
		s := compose.Services[svc]
		if s.Image != devImage || s.PullPolicy != "build" || s.Build == nil {
			t.Fatalf("%s: image %q pull_policy %q build %v; want a build tagged %s", svc, s.Image, s.PullPolicy, s.Build, devImage)
		}
		if s.Build.Context != checkout || s.Build.Dockerfile != "docker/Dockerfile" {
			t.Errorf("%s build = %+v; want context %s", svc, s.Build, checkout)
		}
		if s.Build.Args["TF_LICENSE_PUBLIC_KEY"] != "${TF_LICENSE_PUBLIC_KEY:-}" {
			t.Errorf("%s build args = %v", svc, s.Build.Args)
		}
	}
	st, err := readState(dir)
	if err != nil {
		t.Fatal(err)
	}
	if st.Image != nil || st.BuildFrom == nil || *st.BuildFrom != checkout {
		t.Errorf("state image %v build_from %v; want build_from %s", st.Image, st.BuildFrom, checkout)
	}

	// Every later command says so too.
	f.out.Reset()
	if code := run(context.Background(), []string{"init", "--dir", dir}, b, f.host); code != 0 {
		t.Fatalf("re-run exit = %d\n%s%s", code, f.out, f.errOut)
	}
	if !strings.Contains(f.out.String(), "unverified source build") {
		t.Errorf("re-run did not say the build is unverified:\n%s", f.out)
	}
}

// TestBuildBlock_QuotesThePath: a checkout path is one YAML scalar whatever
// it holds, and compose's interpolation reads no variable out of it.
func TestBuildBlock_QuotesThePath(t *testing.T) {
	path := `/srv/odd: dir #1/$HOME/"q"`
	var svc struct {
		Build struct {
			Context string `yaml:"context"`
		} `yaml:"build"`
	}
	if err := yaml.Unmarshal([]byte(buildBlock("", path)), &svc); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if want := strings.ReplaceAll(path, "$", "$$"); svc.Build.Context != want {
		t.Errorf("context = %q; want %q", svc.Build.Context, want)
	}
}

func TestInit_Prompts(t *testing.T) {
	f := newFakeHost(t)
	f.terminal = true
	// A URL with a path is refused and asked for again.
	f.stdin = strings.NewReader("https://tf.example.com/app\n" + testPublicURL + "\n" + testClientID + "\n")
	f.readSecret = func() (string, error) { return testSecret, nil }
	dir := filepath.Join(t.TempDir(), "deploy")
	if code := run(context.Background(), []string{"init", "--dir", dir}, releaseBundle(t), f.host); code != 0 {
		t.Fatalf("exit = %d; want 0\n%s%s", code, f.out, f.errOut)
	}
	out := f.out.String()
	if !strings.Contains(out, "must not have a path") {
		t.Errorf("the invalid URL was not refused:\n%s", out)
	}
	callback := strings.Index(out, testPublicURL+"/auth/v1/callback")
	idPrompt := strings.Index(out, "GitHub OAuth app client ID")
	if callback < 0 || idPrompt < 0 || callback > idPrompt {
		t.Errorf("the callback URL is not printed before the client ID is asked for:\n%s", out)
	}
	if strings.Contains(out, testSecret) {
		t.Error("the client secret was echoed")
	}
	for key, want := range map[string]string{"TF_PUBLIC_URL": testPublicURL, "GH_CLIENT_ID": testClientID, "GH_CLIENT_SECRET": testSecret} {
		if v := envOf(t, dir, key); v != want {
			t.Errorf("%s = %q; want %q", key, v, want)
		}
	}
}

// TestInit_Refusals: each refusal exits non-zero and leaves the directory as
// it was.
func TestInit_Refusals(t *testing.T) {
	notCheckout := t.TempDir()
	tests := []struct {
		name     string
		arrange  func(t *testing.T, f *fakeHost, dir string) // runs before init
		args     func(t *testing.T, dir string) []string
		bundle   func(t *testing.T) Bundle
		wantCode int
		wantErr  string
	}{
		{
			name: "non-empty directory with no state file",
			arrange: func(t *testing.T, _ *fakeHost, dir string) {
				mustWrite(t, filepath.Join(dir, composeFile), "services: {}\n")
			},
			wantErr: "is not empty and holds no .triagefactory-deploy.json",
		},
		{
			name:    "missing parent directory",
			args:    func(t *testing.T, dir string) []string { return initArgs(t, filepath.Join(dir, "a", "b")) },
			wantErr: "init creates the deployment directory but not its parents",
		},
		{
			name: "http public URL",
			args: func(t *testing.T, dir string) []string {
				return withFlag(initArgs(t, dir), "--public-url", "http://tf.example.com")
			},
			wantErr: "--public-url: \"http://tf.example.com\" must start with https://",
		},
		{
			name: "public URL with a trailing slash",
			args: func(t *testing.T, dir string) []string {
				return withFlag(initArgs(t, dir), "--public-url", "https://tf.example.com/")
			},
			wantErr: "must not end with a slash",
		},
		{
			name: "public URL with a path",
			args: func(t *testing.T, dir string) []string {
				return withFlag(initArgs(t, dir), "--public-url", "https://tf.example.com/tf")
			},
			wantErr: "must not have a path",
		},
		{
			name: "public URL with a query",
			args: func(t *testing.T, dir string) []string {
				return withFlag(initArgs(t, dir), "--public-url", "https://tf.example.com?a=b")
			},
			wantErr: "must not have a query or fragment",
		},
		{
			name: "public URL with an empty fragment",
			args: func(t *testing.T, dir string) []string {
				return withFlag(initArgs(t, dir), "--public-url", "https://tf.example.com#")
			},
			wantErr: "must not have a query or fragment",
		},
		{
			name: "public URL with an empty port",
			args: func(t *testing.T, dir string) []string {
				return withFlag(initArgs(t, dir), "--public-url", "https://tf.example.com:")
			},
			wantErr: "has an empty port",
		},
		{
			name: "public URL with an out-of-range port",
			args: func(t *testing.T, dir string) []string {
				return withFlag(initArgs(t, dir), "--public-url", "https://tf.example.com:99999")
			},
			wantErr: "is not a TCP port",
		},
		{
			name: "public URL with credentials",
			args: func(t *testing.T, dir string) []string {
				return withFlag(initArgs(t, dir), "--public-url", "https://u:p@tf.example.com")
			},
			wantErr: "must not carry credentials",
		},
		{
			name:    "missing inputs without a terminal",
			args:    func(t *testing.T, dir string) []string { return []string{"init", "--dir", dir} },
			wantErr: "missing --public-url, --github-client-id, --github-client-secret-file (stdin is not a terminal",
		},
		{
			name: "client ID with whitespace",
			args: func(t *testing.T, dir string) []string {
				return withFlag(initArgs(t, dir), "--github-client-id", "two words")
			},
			wantErr: "--github-client-id: must be a single word",
		},
		{
			name: "empty client secret file",
			args: func(t *testing.T, dir string) []string {
				return withFlag(initArgs(t, dir), "--github-client-secret-file", writeSecretFile(t, ""))
			},
			wantErr: "--github-client-secret-file:",
		},
		{
			name: "unreadable client secret file",
			args: func(t *testing.T, dir string) []string {
				return withFlag(initArgs(t, dir), "--github-client-secret-file", filepath.Join(dir, "absent"))
			},
			wantErr: "--github-client-secret-file:",
		},
		{
			name:    "bad project name",
			args:    func(t *testing.T, dir string) []string { return initArgs(t, dir, "--project", "Triage Factory") },
			wantErr: "is not a compose project name",
		},
		{
			name:    "non-release binary without --build-from",
			bundle:  func(t *testing.T) Bundle { return repoBundle(t, "dev") },
			wantErr: "not a release, so no published image matches it; pass --build-from",
		},
		{
			name:    "--build-from that is not a checkout",
			args:    func(t *testing.T, dir string) []string { return initArgs(t, dir, "--build-from", notCheckout) },
			wantErr: "is not a Triage Factory source checkout",
		},
		{
			name: "release binary whose embedded pin is another version",
			bundle: func(t *testing.T) Bundle {
				b := releaseBundle(t)
				b.Version = "v99.0.0"
				return b
			},
			wantErr: "it was not built from that release's commit",
		},
		{
			name: "binary without a bundle",
			bundle: func(t *testing.T) Bundle {
				b := releaseBundle(t)
				files := fstest.MapFS{}
				maps.Copy(files, b.Files.(fstest.MapFS))
				files[composeFile] = &fstest.MapFile{}
				b.Files = files
				return b
			},
			wantErr: "this binary carries no deployment bundle",
		},
		{
			name: "image signature does not verify",
			arrange: func(_ *testing.T, f *fakeHost, _ string) {
				f.resolveImage = func(context.Context, string) (string, error) {
					return "", errors.New("has no signature that verifies")
				}
			},
			wantErr: "has no signature that verifies",
		},
		{
			name:     "positional argument",
			args:     func(t *testing.T, dir string) []string { return append(initArgs(t, dir), "extra") },
			wantCode: 2,
			wantErr:  "takes no arguments",
		},
		{
			name:     "unknown flag",
			args:     func(t *testing.T, dir string) []string { return initArgs(t, dir, "--github-client-secret", testSecret) },
			wantCode: 2,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFakeHost(t)
			dir := filepath.Join(t.TempDir(), "deploy")
			if err := os.Mkdir(dir, 0o755); err != nil {
				t.Fatal(err)
			}
			if tt.arrange != nil {
				tt.arrange(t, f, dir)
			}
			before := snapshotDir(t, dir)
			args := initArgs(t, dir)
			if tt.args != nil {
				args = tt.args(t, dir)
			}
			b := releaseBundle(t)
			if tt.bundle != nil {
				b = tt.bundle(t)
			}
			want := tt.wantCode
			if want == 0 {
				want = 1
			}
			if code := run(context.Background(), args, b, f.host); code != want {
				t.Fatalf("exit = %d; want %d\n%s%s", code, want, f.out, f.errOut)
			}
			if tt.wantErr != "" && !strings.Contains(f.errOut.String(), tt.wantErr) {
				t.Errorf("stderr = %q; want it to contain %q", f.errOut, tt.wantErr)
			}
			after := snapshotDir(t, dir)
			if len(after) != len(before) {
				t.Errorf("refused init changed the directory: %v -> %v", keys(before), keys(after))
			}
		})
	}
}

// TestInit_RerunRefusals: on a deployment init rendered, a flag that names a
// different value than the deployment has is refused, not applied, and the
// deployment is left as it was.
func TestInit_RerunRefusals(t *testing.T) {
	tests := []struct {
		name    string
		args    func(t *testing.T, dir string) []string
		bundle  func(t *testing.T) Bundle
		wantErr string
	}{
		{
			name: "a newer binary",
			bundle: func(t *testing.T) Bundle {
				b := releaseBundle(t)
				b.Version = "v99.0.0"
				return b
			},
			wantErr: "init does not re-render a deployment. To fix it, run `triagefactory deploy upgrade` to move the deployment to v99.0.0",
		},
		{
			name: "an older binary",
			bundle: func(t *testing.T) Bundle {
				b := releaseBundle(t)
				b.Version = "v0.0.1"
				return b
			},
			wantErr: "init does not re-render a deployment. To fix it, install the v",
		},
		{
			name:    "another project",
			args:    func(t *testing.T, dir string) []string { return initArgs(t, dir, "--project", "other") },
			wantErr: "this deployment's compose project is triage-factory",
		},
		{
			name: "a source build of a pulled deployment",
			args: func(t *testing.T, dir string) []string {
				return initArgs(t, dir, "--build-from", filepath.Join("..", ".."))
			},
			wantErr: "init never changes where its image comes from",
		},
		{
			name: "a different public URL",
			args: func(t *testing.T, dir string) []string {
				return withFlag(initArgs(t, dir), "--public-url", "https://other.example.com")
			},
			wantErr: ".env already sets TF_PUBLIC_URL to a different value",
		},
		{
			name: "a different client secret",
			args: func(t *testing.T, dir string) []string {
				return withFlag(initArgs(t, dir), "--github-client-secret-file", writeSecretFile(t, "another"))
			},
			wantErr: ".env already sets GH_CLIENT_SECRET to a different value",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFakeHost(t)
			dir := filepath.Join(t.TempDir(), "deploy")
			if code := run(context.Background(), initArgs(t, dir), releaseBundle(t), f.host); code != 0 {
				t.Fatalf("first init exit = %d\n%s%s", code, f.out, f.errOut)
			}
			before := snapshotDir(t, dir)
			args := initArgs(t, dir)
			if tt.args != nil {
				args = tt.args(t, dir)
			}
			b := releaseBundle(t)
			if tt.bundle != nil {
				b = tt.bundle(t)
			}
			if code := run(context.Background(), args, b, f.host); code != 1 {
				t.Fatalf("exit = %d; want 1\n%s%s", code, f.out, f.errOut)
			}
			if !strings.Contains(f.errOut.String(), tt.wantErr) {
				t.Errorf("stderr = %q; want it to contain %q", f.errOut, tt.wantErr)
			}
			after := snapshotDir(t, dir)
			for path, data := range before {
				if after[path] != data {
					t.Errorf("refused re-run changed %s", path)
				}
			}
		})
	}
}

func TestCheckCompose_EmptyBundle(t *testing.T) {
	err := checkCompose(nil, "v1.2.3", true)
	if err == nil || !strings.Contains(err.Error(), "carries no deployment bundle") {
		t.Errorf("checkCompose(empty) = %v; want the no-bundle refusal", err)
	}
}

// composeDoc is the part of a rendered docker-compose.yml the tests read.
type composeDoc struct {
	Name     string `yaml:"name"`
	Services map[string]struct {
		Image      string `yaml:"image"`
		PullPolicy string `yaml:"pull_policy"`
		Build      *struct {
			Context    string            `yaml:"context"`
			Dockerfile string            `yaml:"dockerfile"`
			Args       map[string]string `yaml:"args"`
		} `yaml:"build"`
		SecurityOpt []string `yaml:"security_opt"`
	} `yaml:"services"`
}

func parseCompose(t *testing.T, raw string) composeDoc {
	t.Helper()
	var doc composeDoc
	if err := yaml.Unmarshal([]byte(raw), &doc); err != nil {
		t.Fatalf("rendered compose is not YAML: %v", err)
	}
	return doc
}

func countBundleFiles(t *testing.T, b Bundle) int {
	t.Helper()
	n := 0
	_ = fs.WalkDir(b.Files, ".", func(p string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() && p != envExampleFile {
			n++
		}
		return err
	})
	return n
}

func countDirs(snapshot map[string]string) int {
	n := 0
	for p := range snapshot {
		if strings.HasSuffix(p, "/") {
			n++
		}
	}
	return n
}

func keys(m map[string]string) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	return out
}

func mustWrite(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// withFlag replaces flag's value in args.
func withFlag(args []string, flag, value string) []string {
	out := append([]string(nil), args...)
	for i := range out {
		if out[i] == flag && i+1 < len(out) {
			out[i+1] = value
		}
	}
	return out
}
