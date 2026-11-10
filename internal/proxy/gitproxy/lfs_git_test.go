package gitproxy_test

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/cgi"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/githooks"
	"github.com/sky-ai-eng/triage-factory/internal/proxy/gitproxy"
)

// storageCredential is the header the fake storage host demands, standing in
// for the signed-URL credential a real batch answer carries. The proxy must
// present it to storage and must never hand it to the client.
const storageCredential = "RemoteAuth storage-secret"

// fakeLFSHost is a git host with a Git LFS API in front of a separate storage
// host, shaped the way GitHub's is: git smart-HTTP via `git http-backend`, the
// batch and lock-verification API beside it, and storage URLs on another host
// that want their own header.
type fakeLFSHost struct {
	git     *httptest.Server
	storage *httptest.Server

	mu          sync.Mutex
	objects     map[string][]byte
	apiAuth     []string   // Authorization on every batch and lock call
	transfers   [][]string // the transfers each batch request offered
	storageAuth []string   // Authorization on every storage request
	verified    []string   // oids the verify action confirmed
}

func newFakeLFSHost(t *testing.T, projectRoot, backend string) *fakeLFSHost {
	t.Helper()
	h := &fakeLFSHost{objects: map[string][]byte{}}
	h.storage = httptest.NewServer(http.HandlerFunc(h.serveStorage))
	t.Cleanup(h.storage.Close)
	git := &cgi.Handler{
		Path: backend,
		Env:  []string{"GIT_PROJECT_ROOT=" + projectRoot, "GIT_HTTP_EXPORT_ALL=1", "REMOTE_USER=tester"},
	}
	h.git = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/info/lfs/objects/batch"):
			h.serveBatch(w, r)
		case strings.HasSuffix(r.URL.Path, "/info/lfs/locks/verify"):
			h.mu.Lock()
			h.apiAuth = append(h.apiAuth, r.Header.Get("Authorization"))
			h.mu.Unlock()
			w.Header().Set("Content-Type", "application/vnd.git-lfs+json")
			_, _ = io.WriteString(w, `{"ours":[],"theirs":[]}`)
		default:
			git.ServeHTTP(w, r)
		}
	}))
	t.Cleanup(h.git.Close)
	return h
}

func (h *fakeLFSHost) serveBatch(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Operation string   `json:"operation"`
		Transfers []string `json:"transfers"`
		Objects   []struct {
			OID  string `json:"oid"`
			Size int64  `json:"size"`
		} `json:"objects"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.apiAuth = append(h.apiAuth, r.Header.Get("Authorization"))
	h.transfers = append(h.transfers, req.Transfers)

	type action struct {
		Href      string            `json:"href"`
		Header    map[string]string `json:"header"`
		ExpiresIn int               `json:"expires_in"`
	}
	header := map[string]string{"Authorization": storageCredential}
	var objects []map[string]any
	for _, o := range req.Objects {
		actions := map[string]action{}
		_, have := h.objects[o.OID]
		switch {
		case req.Operation == "download" && have:
			actions["download"] = action{h.storage.URL + "/objects/" + o.OID, header, 600}
		case req.Operation == "upload" && !have:
			actions["upload"] = action{h.storage.URL + "/objects/" + o.OID, header, 600}
			actions["verify"] = action{h.storage.URL + "/verify", header, 600}
		}
		obj := map[string]any{"oid": o.OID, "size": o.Size}
		if len(actions) > 0 {
			obj["actions"] = actions
		}
		objects = append(objects, obj)
	}
	w.Header().Set("Content-Type", "application/vnd.git-lfs+json")
	_ = json.NewEncoder(w).Encode(map[string]any{"transfer": "basic", "objects": objects})
}

func (h *fakeLFSHost) serveStorage(w http.ResponseWriter, r *http.Request) {
	h.mu.Lock()
	h.storageAuth = append(h.storageAuth, r.Header.Get("Authorization"))
	h.mu.Unlock()
	if r.Header.Get("Authorization") != storageCredential {
		http.Error(w, "storage credential missing", http.StatusUnauthorized)
		return
	}
	oid := strings.TrimPrefix(r.URL.Path, "/objects/")
	switch {
	case r.Method == http.MethodGet:
		h.mu.Lock()
		b, ok := h.objects[oid]
		h.mu.Unlock()
		if !ok {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write(b)
	case r.Method == http.MethodPut:
		b, err := io.ReadAll(r.Body)
		if err != nil || sha256Hex(b) != oid {
			http.Error(w, "content does not match its oid", http.StatusBadRequest)
			return
		}
		h.mu.Lock()
		h.objects[oid] = b
		h.mu.Unlock()
	case r.Method == http.MethodPost && r.URL.Path == "/verify":
		var v struct {
			OID string `json:"oid"`
		}
		_ = json.NewDecoder(r.Body).Decode(&v)
		h.mu.Lock()
		_, ok := h.objects[v.OID]
		if ok {
			h.verified = append(h.verified, v.OID)
		}
		h.mu.Unlock()
		if !ok {
			http.NotFound(w, r)
		}
	default:
		http.Error(w, "unexpected storage request", http.StatusBadRequest)
	}
}

func sha256Hex(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

func lfsPointer(content []byte) string {
	return fmt.Sprintf("version https://git-lfs.github.com/spec/v1\noid sha256:%s\nsize %d\n", sha256Hex(content), len(content))
}

// lfsClientEnv is the process-scoped git config a run's git carries: the
// rewrite onto the proxy, the placeholder credential, and the git-lfs filters.
func lfsClientEnv(proxyAddr, upstream, placeholder string) []string {
	base := "http://" + proxyAddr + "/"
	pairs := [][2]string{
		{"url." + base + ".insteadOf", upstream + "/"},
		{"http." + base + ".extraHeader", "Authorization: Basic " + base64.StdEncoding.EncodeToString([]byte("x-run:"+placeholder))},
		{"user.name", "Agent"},
		{"user.email", "agent@example.test"},
	}
	pairs = append(pairs, githooks.LFSConfigPairs()...)
	env := []string{fmt.Sprintf("GIT_CONFIG_COUNT=%d", len(pairs))}
	for i, kv := range pairs {
		env = append(env, fmt.Sprintf("GIT_CONFIG_KEY_%d=%s", i, kv[0]), fmt.Sprintf("GIT_CONFIG_VALUE_%d=%s", i, kv[1]))
	}
	return env
}

// TestLFS_RealGitLFSThroughProxy drives real git and git-lfs through the
// proxy: a checkout that replaces pointers, the pointers-first checkout TF's
// setup makes followed by `git lfs pull`, and a push that uploads a new LFS
// object. The client must reach storage only through the proxy and never hold
// the storage credential; the git host must see the installation token on
// every API call; and a run that may not push must not upload.
func TestLFS_RealGitLFSThroughProxy(t *testing.T) {
	if _, err := exec.LookPath("git-lfs"); err != nil {
		t.Skipf("git-lfs not installed: %v", err)
	}
	backend := filepath.Join(gitExecPath(t), "git-http-backend")
	if _, err := os.Stat(backend); err != nil {
		t.Skipf("git-http-backend not present at %s: %v", backend, err)
	}

	root := t.TempDir()
	bare := filepath.Join(root, "octo", "repo.git")
	host := newFakeLFSHost(t, root, backend)

	asset := []byte("hello lfs\n")
	host.objects[sha256Hex(asset)] = asset
	runGit(t, "", "init", "--bare", "--initial-branch=main", bare)
	runGit(t, bare, "config", "http.receivepack", "true")
	seed := t.TempDir()
	runGit(t, "", "init", "--initial-branch=main", seed)
	writeFile(t, filepath.Join(seed, ".gitattributes"), "*.bin filter=lfs diff=lfs merge=lfs -text\n")
	writeFile(t, filepath.Join(seed, "asset.bin"), lfsPointer(asset))
	runGit(t, seed, "add", ".")
	runGit(t, seed, "-c", "user.name=Seed", "-c", "user.email=seed@example.test", "commit", "-m", "seed")
	runGit(t, seed, "push", bare, "main")

	const placeholder = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	var mayPush atomic.Bool
	mayPush.Store(true)
	denials := make(chan gitproxy.DeniedGitOp, 16)
	srv, err := gitproxy.New(gitproxy.Config{
		Upstream:      host.git.URL,
		IncomingToken: placeholder,
		TokenSource: func(context.Context, string, string) (gitproxy.Token, error) {
			return gitproxy.Token{Value: "ghs_tok"}, nil
		},
		Authorize: func(context.Context, string, string) (gitproxy.Decision, error) {
			var refs []string
			if mayPush.Load() {
				refs = []string{"refs/heads/feature"}
			}
			return gitproxy.Decision{Allowed: true, AllowedRefs: refs}, nil
		},
		RecordDenial: func(_ context.Context, d gitproxy.DeniedGitOp) { denials <- d },
	})
	if err != nil {
		t.Fatalf("gitproxy.New: %v", err)
	}
	addr, err := srv.Start("")
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
	})
	env := lfsClientEnv(addr, host.git.URL, placeholder)
	lazyEnv := append(append([]string(nil), env...), githooks.LFSSkipSmudgeEnvVar+"=1")
	remote := host.git.URL + "/octo/repo.git"

	// A checkout with git-lfs's filter active fetches every LFS file in the
	// tree through the batch API and a transfer per object.
	smudged := filepath.Join(t.TempDir(), "smudged")
	if out, err := gitOutput("", env, "clone", remote, smudged); err != nil {
		t.Fatalf("clone with smudge: %v\n%s", err, out)
	}
	if got := readFile(t, filepath.Join(smudged, "asset.bin")); got != string(asset) {
		t.Fatalf("asset.bin after a smudging clone = %q, want its content", got)
	}

	// TF's own checkout leaves pointers; `git lfs pull` fetches on request,
	// with the transfer at the proxy and never at storage.
	lazy := filepath.Join(t.TempDir(), "lazy")
	if out, err := gitOutput("", lazyEnv, "clone", remote, lazy); err != nil {
		t.Fatalf("clone with smudge skipped: %v\n%s", err, out)
	}
	if got := readFile(t, filepath.Join(lazy, "asset.bin")); got != lfsPointer(asset) {
		t.Fatalf("asset.bin after a pointers-first clone = %q, want the pointer", got)
	}
	out, err := gitOutput(lazy, append(lazyEnv, "GIT_TRACE=1", "GIT_CURL_VERBOSE=1"), "lfs", "pull")
	if err != nil {
		t.Fatalf("git lfs pull: %v\n%s", err, out)
	}
	if got := readFile(t, filepath.Join(lazy, "asset.bin")); got != string(asset) {
		t.Fatalf("asset.bin after git lfs pull = %q, want its content", got)
	}
	storageHost := strings.TrimPrefix(host.storage.URL, "http://")
	if strings.Contains(out, storageHost) || strings.Contains(out, "storage-secret") {
		t.Errorf("git-lfs was handed the storage URL or its credential:\n%s", out)
	}
	if !strings.Contains(out, "/info/lfs/transfers/") {
		t.Errorf("git-lfs's trace shows no transfer through the proxy:\n%s", out)
	}
	if status := runGitEnv(t, lazy, lazyEnv, "status", "--porcelain"); status != "" {
		t.Errorf("worktree not clean after git lfs pull:\n%s", status)
	}

	// A push uploads the LFS objects it references through the proxy. git-lfs's
	// pre-push hook does the upload; in a run, TF's own hook runs it.
	runGitEnv(t, lazy, lazyEnv, "lfs", "install", "--local")
	added := []byte("a new asset\n")
	writeFile(t, filepath.Join(lazy, "added.bin"), string(added))
	runGitEnv(t, lazy, lazyEnv, "add", "added.bin")
	runGitEnv(t, lazy, lazyEnv, "commit", "-m", "add an asset")
	if out, err := gitOutput(lazy, lazyEnv, "push", "origin", "HEAD:refs/heads/feature"); err != nil {
		t.Fatalf("push with a new LFS object: %v\n%s", err, out)
	}
	host.mu.Lock()
	_, uploaded := host.objects[sha256Hex(added)]
	verified := append([]string(nil), host.verified...)
	host.mu.Unlock()
	if !uploaded {
		t.Fatalf("the pushed LFS object never reached storage")
	}
	if len(verified) != 1 || verified[0] != sha256Hex(added) {
		t.Errorf("verified %v, want exactly the pushed object", verified)
	}
	if got := strings.TrimSpace(runGit(t, bare, "rev-parse", "refs/heads/feature")); got != strings.TrimSpace(runGitEnv(t, lazy, lazyEnv, "rev-parse", "HEAD")) {
		t.Errorf("refs/heads/feature on the upstream = %s, want the pushed commit", got)
	}

	// A run that may not push to the repository may not upload to it either,
	// and is told why.
	mayPush.Store(false)
	refused := []byte("refused asset\n")
	writeFile(t, filepath.Join(lazy, "refused.bin"), string(refused))
	runGitEnv(t, lazy, lazyEnv, "add", "refused.bin")
	runGitEnv(t, lazy, lazyEnv, "commit", "-m", "another asset")
	out, err = gitOutput(lazy, lazyEnv, "push", "origin", "HEAD:refs/heads/feature")
	if err == nil {
		t.Fatalf("push succeeded for a run that may not push:\n%s", out)
	}
	if !strings.Contains(out, "may not upload Git LFS objects") {
		t.Errorf("git-lfs's output does not carry the proxy's refusal:\n%s", out)
	}
	host.mu.Lock()
	_, leaked := host.objects[sha256Hex(refused)]
	host.mu.Unlock()
	if leaked {
		t.Errorf("an upload the gate refused reached storage")
	}
	if d := nextDenial(t, denials); d.Op != "lfs-upload" || d.Reason != "push-not-authorized" || d.Owner != "octo" || d.Repo != "repo" {
		t.Errorf("denial recorded as %+v, want an lfs-upload push-not-authorized on octo/repo", d)
	}

	host.mu.Lock()
	defer host.mu.Unlock()
	wantAPI := "Basic " + base64.StdEncoding.EncodeToString([]byte("x-access-token:ghs_tok"))
	if len(host.apiAuth) == 0 {
		t.Fatalf("no LFS API call reached the git host")
	}
	for i, a := range host.apiAuth {
		if a != wantAPI {
			t.Errorf("LFS API call %d carried Authorization %q, want the installation token", i, a)
		}
	}
	for i, tr := range host.transfers {
		if len(tr) != 1 || tr[0] != "basic" {
			t.Errorf("batch %d offered transfers %v, want only basic", i, tr)
		}
	}
	if len(host.storageAuth) == 0 {
		t.Fatalf("no request reached storage")
	}
	for i, a := range host.storageAuth {
		if a != storageCredential {
			t.Errorf("storage request %d carried Authorization %q, want only the storage's own header", i, a)
		}
	}
}

func nextDenial(t *testing.T, ch <-chan gitproxy.DeniedGitOp) gitproxy.DeniedGitOp {
	t.Helper()
	select {
	case d := <-ch:
		return d
	case <-time.After(5 * time.Second):
		t.Fatal("no denial recorded")
		return gitproxy.DeniedGitOp{}
	}
}

func runGitEnv(t *testing.T, dir string, env []string, args ...string) string {
	t.Helper()
	out, err := gitOutput(dir, env, args...)
	if err != nil {
		t.Fatalf("git %s in %q: %v\n%s", strings.Join(args, " "), dir, err, out)
	}
	return out
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(b)
}
