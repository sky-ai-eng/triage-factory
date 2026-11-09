package gitproxy

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// lfsFixture is a proxy in front of a git host whose batch answer is whatever
// the test sets, with storage on a host of its own.
type lfsFixture struct {
	srv      *Server
	proxy    *httptest.Server
	answer   atomic.Value // func(storageURL string) (status int, body string)
	storage  *httptest.Server
	stored   atomic.Int64 // storage requests seen
	upstream atomic.Int64 // batch requests seen
	allow    atomic.Bool
	push     atomic.Bool
}

func newLFSFixture(t *testing.T) *lfsFixture {
	t.Helper()
	f := &lfsFixture{}
	f.allow.Store(true)
	f.push.Store(true)
	f.storage = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.stored.Add(1)
		if r.Header.Get("Authorization") != "RemoteAuth s" {
			http.Error(w, "no storage credential", http.StatusUnauthorized)
			return
		}
		_, _ = io.WriteString(w, "object bytes")
	}))
	t.Cleanup(f.storage.Close)
	git := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.upstream.Add(1)
		status, body := f.answer.Load().(func(string) (int, string))(f.storage.URL)
		w.Header().Set("Content-Type", lfsMediaType)
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(git.Close)
	f.setAnswer(http.StatusOK, `{"transfer":"basic","objects":[{"oid":"aa","size":12,"actions":{"download":{"href":"STORAGE/objects/aa?sig=secret","header":{"Authorization":"RemoteAuth s"},"expires_in":600}}}]}`)

	srv, err := New(Config{
		Upstream: git.URL,
		TokenSource: func(context.Context, string, string) (Token, error) {
			return Token{Value: "ghs_tok"}, nil
		},
		Authorize: func(context.Context, string, string) (Decision, error) {
			d := Decision{Allowed: f.allow.Load()}
			if f.push.Load() {
				d.AllowedRefs = []string{"refs/heads/feature"}
			}
			return d, nil
		},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	f.srv = srv
	f.proxy = httptest.NewServer(srv.Handler())
	t.Cleanup(f.proxy.Close)
	return f
}

// setAnswer sets the git host's batch answer; STORAGE in body is replaced by
// the storage host's URL.
func (f *lfsFixture) setAnswer(status int, body string) {
	f.answer.Store(func(storage string) (int, string) {
		return status, strings.ReplaceAll(body, "STORAGE", storage)
	})
}

func (f *lfsFixture) batch(t *testing.T, operation string) (int, string) {
	t.Helper()
	resp, err := http.Post(f.proxy.URL+"/octo/repo.git/info/lfs/objects/batch", lfsMediaType,
		strings.NewReader(`{"operation":"`+operation+`","transfers":["basic","ssh"],"objects":[{"oid":"aa","size":12}]}`))
	if err != nil {
		t.Fatalf("batch: %v", err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

func (f *lfsFixture) do(t *testing.T, method, url string) (int, string) {
	t.Helper()
	req, _ := http.NewRequest(method, url, nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, url, err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

func (f *lfsFixture) outstanding() int {
	f.srv.lfsMu.Lock()
	defer f.srv.lfsMu.Unlock()
	return len(f.srv.lfsTransfers)
}

// downloadHref is the single download href in a batch answer.
func downloadHref(t *testing.T, body string) string {
	t.Helper()
	var doc struct {
		Objects []struct {
			Actions map[string]map[string]any `json:"actions"`
		} `json:"objects"`
	}
	if err := json.Unmarshal([]byte(body), &doc); err != nil || len(doc.Objects) != 1 {
		t.Fatalf("batch answer %q: %v", body, err)
	}
	href, _ := doc.Objects[0].Actions["download"]["href"].(string)
	return href
}

func TestLFSBatch_AnswerNamesOnlyProxyURLs(t *testing.T) {
	f := newLFSFixture(t)
	f.setAnswer(http.StatusOK, `{"transfer":"basic","hash_algo":"sha256","objects":[{"oid":"aa","size":12,"x-extra":7,"actions":{"download":{"href":"STORAGE/objects/aa?sig=secret","header":{"Authorization":"RemoteAuth s"},"expires_in":600}}}]}`)
	before := time.Now()
	status, body := f.batch(t, "download")
	if status != http.StatusOK {
		t.Fatalf("batch = %d %s", status, body)
	}
	if strings.Contains(body, f.storage.URL) || strings.Contains(body, "secret") || strings.Contains(body, "RemoteAuth") {
		t.Fatalf("batch answer carries the storage URL or its credential: %s", body)
	}

	var doc struct {
		HashAlgo string `json:"hash_algo"`
		Objects  []struct {
			Extra         int  `json:"x-extra"`
			Authenticated bool `json:"authenticated"`
			Actions       map[string]struct {
				Href      string            `json:"href"`
				Header    map[string]string `json:"header"`
				ExpiresIn int64             `json:"expires_in"`
				ExpiresAt string            `json:"expires_at"`
			} `json:"actions"`
		} `json:"objects"`
	}
	if err := json.Unmarshal([]byte(body), &doc); err != nil {
		t.Fatalf("decode %s: %v", body, err)
	}
	obj := doc.Objects[0]
	if doc.HashAlgo != "sha256" || obj.Extra != 7 {
		t.Errorf("members the rewrite has no reason to touch were dropped: %s", body)
	}
	a := obj.Actions["download"]
	if want := f.proxy.URL + "/octo/repo.git/info/lfs/transfers/"; !strings.HasPrefix(a.Href, want) {
		t.Errorf("download href = %q, want one under %q", a.Href, want)
	}
	if !obj.Authenticated || a.Header != nil || a.ExpiresIn != 0 {
		t.Errorf("rewritten action = %+v authenticated=%v; want no header, no expires_in, authenticated", a, obj.Authenticated)
	}
	exp, err := time.Parse(time.RFC3339, a.ExpiresAt)
	if err != nil || exp.After(before.Add(601*time.Second)) || exp.Before(before.Add(598*time.Second)) {
		t.Errorf("expires_at = %q, want the storage URL's own expiry, ~600s out", a.ExpiresAt)
	}

	// The transfer reaches storage with the storage's header, once.
	if status, got := f.do(t, http.MethodGet, a.Href); status != http.StatusOK || got != "object bytes" {
		t.Fatalf("download = %d %q", status, got)
	}
	if f.outstanding() != 0 {
		t.Errorf("a transfer that succeeded is still held")
	}
	if status, _ := f.do(t, http.MethodGet, a.Href); status != http.StatusNotFound {
		t.Errorf("a used transfer URL answered %d, want 404", status)
	}
}

func TestLFSBatch_UploadNeedsPushRights(t *testing.T) {
	f := newLFSFixture(t)
	f.push.Store(false)
	status, body := f.batch(t, "upload")
	if status != http.StatusForbidden || !strings.Contains(body, "may not upload Git LFS objects") {
		t.Fatalf("upload by a run that may not push = %d %s", status, body)
	}
	if f.upstream.Load() != 0 {
		t.Errorf("a refused upload reached the git host")
	}
	if status, body := f.batch(t, "download"); status != http.StatusOK {
		t.Errorf("download by a run that may fetch = %d %s", status, body)
	}
}

func TestLFSBatch_RefusesWhatItCannotProxy(t *testing.T) {
	for name, answer := range map[string]string{
		"plain http storage": `{"objects":[{"oid":"aa","size":1,"actions":{"download":{"href":"http://storage.example/objects/aa?sig=secret"}}}]}`,
		"unknown action":     `{"objects":[{"oid":"aa","size":1,"actions":{"download":{"href":"STORAGE/a"},"teleport":{"href":"STORAGE/b"}}}]}`,
		"other adapter":      `{"transfer":"tus","objects":[{"oid":"aa","size":1,"actions":{"upload":{"href":"STORAGE/a"}}}]}`,
	} {
		t.Run(name, func(t *testing.T) {
			f := newLFSFixture(t)
			f.setAnswer(http.StatusOK, answer)
			status, body := f.batch(t, "download")
			if status != http.StatusBadGateway {
				t.Errorf("batch = %d %s, want 502", status, body)
			}
			if strings.Contains(body, "secret") || strings.Contains(body, "STORAGE") || strings.Contains(body, f.storage.URL) {
				t.Errorf("refusal leaks a storage URL: %s", body)
			}
			if f.outstanding() != 0 {
				t.Errorf("a refused answer left %d transfers held", f.outstanding())
			}
		})
	}
}

func TestLFSBatch_ErrorAnswerPassesThrough(t *testing.T) {
	f := newLFSFixture(t)
	f.setAnswer(http.StatusNotFound, `{"message":"Repository or object not found"}`)
	if status, body := f.batch(t, "download"); status != http.StatusNotFound || !strings.Contains(body, "Repository or object not found") {
		t.Errorf("batch = %d %s, want the git host's own 404", status, body)
	}
}

func TestLFSTransfer_RechecksTheGate(t *testing.T) {
	f := newLFSFixture(t)
	_, body := f.batch(t, "download")
	href := downloadHref(t, body)

	f.allow.Store(false)
	if status, _ := f.do(t, http.MethodGet, href); status != http.StatusForbidden {
		t.Errorf("transfer after the repo was untracked = %d, want 403", status)
	}
	if f.stored.Load() != 0 {
		t.Errorf("a refused transfer reached storage")
	}
}

func TestLFSTransfer_IsBoundToItsRepoAndMethod(t *testing.T) {
	f := newLFSFixture(t)
	_, body := f.batch(t, "download")
	href := downloadHref(t, body)
	id := href[strings.LastIndexByte(href, '/')+1:]

	if status, _ := f.do(t, http.MethodGet, f.proxy.URL+"/octo/other.git/info/lfs/transfers/"+id); status != http.StatusNotFound {
		t.Errorf("transfer under another repository = %d, want 404", status)
	}
	if status, _ := f.do(t, http.MethodPut, href); status != http.StatusMethodNotAllowed {
		t.Errorf("download performed as an upload = %d, want 405", status)
	}
	if f.stored.Load() != 0 {
		t.Errorf("a misaddressed transfer reached storage")
	}
}

func TestLFSTransfer_Expires(t *testing.T) {
	f := newLFSFixture(t)
	_, body := f.batch(t, "download")
	href := downloadHref(t, body)

	f.srv.SetNowForTest(func() time.Time { return time.Now().Add(601 * time.Second) })
	if status, _ := f.do(t, http.MethodGet, href); status != http.StatusNotFound {
		t.Errorf("expired transfer = %d, want 404", status)
	}
}

func TestLFSExpiry(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 500, time.UTC)
	cases := map[string]struct {
		a    lfsAction
		want time.Duration
	}{
		"none given":      {lfsAction{}, lfsTransferTTL},
		"expires_in":      {lfsAction{ExpiresIn: 60}, time.Minute},
		"past the cap":    {lfsAction{ExpiresIn: 7200}, lfsTransferTTL},
		"expires_at":      {lfsAction{ExpiresAt: now.Add(2 * time.Minute).Format(time.RFC3339)}, 2 * time.Minute},
		"earlier of both": {lfsAction{ExpiresIn: 600, ExpiresAt: now.Add(time.Minute).Format(time.RFC3339)}, time.Minute},
		"unparseable at":  {lfsAction{ExpiresAt: "soon"}, lfsTransferTTL},
	}
	for name, c := range cases {
		if got := lfsExpiry(c.a, now); !got.Equal(now.Add(c.want).Truncate(time.Second)) {
			t.Errorf("%s: lfsExpiry = %s, want %s", name, got, now.Add(c.want).Truncate(time.Second))
		}
	}
}

func TestLFSTransfers_AreBounded(t *testing.T) {
	s := &Server{}
	now := time.Now()
	for i := 0; i < lfsMaxTransfers; i++ {
		if _, err := s.storeLFSTransfer(&lfsTransfer{expires: now.Add(time.Minute)}, now); err != nil {
			t.Fatalf("store %d: %v", i, err)
		}
	}
	if _, err := s.storeLFSTransfer(&lfsTransfer{expires: now.Add(time.Minute)}, now); err != errLFSTransfersFull {
		t.Fatalf("store past the cap = %v, want errLFSTransfersFull", err)
	}
	// Once they have expired, the cap makes room by forgetting them.
	later := now.Add(2 * time.Minute)
	if _, err := s.storeLFSTransfer(&lfsTransfer{expires: later.Add(time.Minute)}, later); err != nil {
		t.Fatalf("store after the held transfers expired: %v", err)
	}
	if len(s.lfsTransfers) != 1 {
		t.Errorf("held %d transfers after expiry, want 1", len(s.lfsTransfers))
	}
}

// TestLFSTransferBase pins that a transfer URL resolves back to the listener
// and repository path the batch arrived on: on the shared origin that is the
// TLS host GH_HOST names, where the run's git config scopes its credential.
func TestLFSTransferBase(t *testing.T) {
	r := httptest.NewRequest(http.MethodPost, "https://gh.run.internal/octo/repo.git/info/lfs/objects/batch", nil)
	if got, want := lfsTransferBase(r), "https://gh.run.internal/octo/repo.git/info/lfs/transfers/"; got != want {
		t.Errorf("over TLS: %q, want %q", got, want)
	}
	r = httptest.NewRequest(http.MethodPost, "http://192.168.99.1:4000/octo/repo/info/lfs/objects/batch", nil)
	if got, want := lfsTransferBase(r), "http://192.168.99.1:4000/octo/repo/info/lfs/transfers/"; got != want {
		t.Errorf("plain listener: %q, want %q", got, want)
	}
	r = httptest.NewRequest(http.MethodPost, "http://127.0.0.1:4000/info/lfs.git/info/lfs/objects/batch", nil)
	if got, want := lfsTransferBase(r), "http://127.0.0.1:4000/info/lfs.git/info/lfs/transfers/"; got != want {
		t.Errorf("repository named info/lfs: %q, want %q", got, want)
	}
}
