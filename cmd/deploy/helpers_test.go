package deploy

import (
	"bytes"
	"context"
	"io/fs"
	"maps"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"testing/fstest"
	"time"
)

const (
	testPublicURL = "https://tf.example.com"
	testClientID  = "Iv1.0123456789abcdef"
	testSecret    = "0123456789abcdef0123456789abcdef01234567"
	testDigest    = "sha256:1111111111111111111111111111111111111111111111111111111111111111"
)

// repoBundle is the bundle the binary embeds, read from the repository: the
// same files the root package's go:embed names.
func repoBundle(t *testing.T, version string) Bundle {
	t.Helper()
	repo := filepath.Join("..", "..")
	files := fstest.MapFS{}
	add := func(rel string) {
		data, err := os.ReadFile(filepath.Join(repo, filepath.FromSlash(rel)))
		if err != nil {
			t.Fatalf("read %s: %v", rel, err)
		}
		files[rel] = &fstest.MapFile{Data: data}
	}
	add(composeFile)
	add(envExampleFile)
	add(seccompFile)
	err := filepath.WalkDir(filepath.Join(repo, "docker", "observability"), func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel, err := filepath.Rel(repo, p)
		if err != nil {
			return err
		}
		add(filepath.ToSlash(rel))
		return nil
	})
	if err != nil {
		t.Fatalf("walk observability: %v", err)
	}
	return Bundle{Files: files, Version: version}
}

// releaseBundle is repoBundle with the version the repo's compose file pins,
// as a release binary built from the release commit has.
func releaseBundle(t *testing.T) Bundle {
	t.Helper()
	b := repoBundle(t, "")
	raw, _ := fs.ReadFile(b.Files, composeFile)
	m := composeImageLine.FindSubmatch(raw)
	if m == nil {
		t.Fatal("docker-compose.yml pins no release image")
	}
	b.Version = string(m[2])
	return b
}

// fakeHost is a Linux Docker host with every check passing; tests break the
// piece they are about. dockerCalls records every docker invocation, the
// streamed ones included, in order.
type fakeHost struct {
	*host
	out         *bytes.Buffer
	errOut      *bytes.Buffer
	dockerCalls [][]string
	// dockerReplies answers a docker call whose args start with the key
	// (space-joined); unmatched calls succeed with no output.
	dockerReplies map[string]dockerReply
	// dockerSeqs answers successive calls whose args start with the key, one
	// reply per call with the last repeating, ahead of dockerReplies.
	dockerSeqs map[string][]dockerReply
	// streamed records the streamed docker calls, each with its directory,
	// and onStream runs on each before it is answered.
	streamed []streamCall
	onStream func(args []string)
	resolved []string
	// verified records each verifyImage call as version + " " + ref, and
	// verifyErr is what verifyImage answers.
	verified  []string
	verifyErr error
	// clock is the time now reports; sleep advances it.
	clock time.Time
}

type streamCall struct {
	dir  string
	args []string
}

type dockerReply struct {
	stdout string
	stderr string
	err    error
}

func newFakeHost(t *testing.T) *fakeHost {
	t.Helper()
	cgroup := filepath.Join(t.TempDir(), "cgroup.controllers")
	if err := os.WriteFile(cgroup, []byte("cpu memory\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	f := &fakeHost{
		out:    &bytes.Buffer{},
		errOut: &bytes.Buffer{},
		dockerReplies: map[string]dockerReply{
			"version":         {stdout: "29.9.2\n"},
			"compose version": {stdout: "2.40.3\n"},
		},
		dockerSeqs: map[string][]dockerReply{},
	}
	f.host = &host{
		goos:              "linux",
		stdin:             strings.NewReader(""),
		stdout:            f.out,
		stderr:            f.errOut,
		cgroupControllers: cgroup,
		portFree:          func(int) bool { return true },
		http:              http.DefaultClient,
		controlURL:        "http://127.0.0.1:1",
		gotrueURL:         "http://127.0.0.1:1",
	}
	f.docker = func(_ context.Context, _ string, args ...string) ([]byte, []byte, error) {
		f.dockerCalls = append(f.dockerCalls, args)
		r := f.reply(args)
		return []byte(r.stdout), []byte(r.stderr), r.err
	}
	f.stream = func(_ context.Context, dir string, args ...string) error {
		f.dockerCalls = append(f.dockerCalls, args)
		f.streamed = append(f.streamed, streamCall{dir: dir, args: args})
		if f.onStream != nil {
			f.onStream(args)
		}
		return f.reply(args).err
	}
	f.resolveImage = func(_ context.Context, version string) (string, error) {
		f.resolved = append(f.resolved, version)
		return imageRepository + ":" + version + "@" + testDigest, nil
	}
	f.verifyImage = func(_ context.Context, version, ref string) error {
		f.verified = append(f.verified, version+" "+ref)
		return f.verifyErr
	}
	f.clock = time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)
	f.now = func() time.Time { return f.clock }
	f.sleep = func(_ context.Context, d time.Duration) error {
		f.clock = f.clock.Add(d)
		return nil
	}
	return f
}

// reply answers a docker call: the next reply of the longest matching
// sequence, else the longest matching reply.
func (f *fakeHost) reply(args []string) dockerReply {
	joined := strings.Join(args, " ")
	longest := func(keys []string) string {
		best := ""
		for _, prefix := range keys {
			if strings.HasPrefix(joined, prefix) && len(prefix) > len(best) {
				best = prefix
			}
		}
		return best
	}
	if key := longest(slices.Collect(maps.Keys(f.dockerSeqs))); key != "" {
		seq := f.dockerSeqs[key]
		r := seq[0]
		if len(seq) > 1 {
			f.dockerSeqs[key] = seq[1:]
		}
		return r
	}
	return f.dockerReplies[longest(slices.Collect(maps.Keys(f.dockerReplies)))]
}

// called reports whether a docker call's args, space-joined, start with
// prefix.
func (f *fakeHost) called(prefix string) bool {
	for _, call := range f.dockerCalls {
		if strings.HasPrefix(strings.Join(call, " "), prefix) {
			return true
		}
	}
	return false
}

// noDockerCLI makes the docker CLI missing from PATH.
func (f *fakeHost) noDockerCLI() {
	f.docker = func(context.Context, string, ...string) ([]byte, []byte, error) {
		return nil, nil, &exec.Error{Name: "docker", Err: exec.ErrNotFound}
	}
}

// writeSecretFile writes the client secret to a file, as an operator would
// for --github-client-secret-file.
func writeSecretFile(t *testing.T, secret string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "gh-client-secret")
	if err := os.WriteFile(p, []byte(secret+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// initArgs are a complete non-interactive init into dir.
func initArgs(t *testing.T, dir string, extra ...string) []string {
	t.Helper()
	return append([]string{
		"init", "--dir", dir,
		"--public-url", testPublicURL,
		"--github-client-id", testClientID,
		"--github-client-secret-file", writeSecretFile(t, testSecret),
	}, extra...)
}

// snapshotDir reads every file under dir, keyed by slash path, so a test can
// assert a command left it exactly as it was.
func snapshotDir(t *testing.T, dir string) map[string]string {
	t.Helper()
	files := map[string]string{}
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(dir, p)
		if d.IsDir() {
			files[filepath.ToSlash(rel)+"/"] = ""
			return nil
		}
		data, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		files[filepath.ToSlash(rel)] = string(data)
		return nil
	})
	if err != nil {
		t.Fatalf("snapshot %s: %v", dir, err)
	}
	return files
}

// envOf reads a .env value from dir.
func envOf(t *testing.T, dir, key string) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(dir, envFile))
	if err != nil {
		t.Fatalf("read .env: %v", err)
	}
	return envValues(raw).get(key)
}

// setEnvLine rewrites key's line in dir's .env to KEY=value.
func setEnvLine(t *testing.T, dir, key, value string) {
	t.Helper()
	p := filepath.Join(dir, envFile)
	raw, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, line := range strings.Split(string(raw), "\n") {
		if strings.HasPrefix(line, key+"=") {
			line = key + "=" + value
		}
		out = append(out, line)
	}
	if err := os.WriteFile(p, []byte(strings.Join(out, "\n")), 0o600); err != nil {
		t.Fatal(err)
	}
}

func mustRemove(t *testing.T, path string) {
	t.Helper()
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
}
