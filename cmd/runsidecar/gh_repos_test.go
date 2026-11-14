package runsidecar

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/agentproc"
	"github.com/sky-ai-eng/triage-factory/internal/cred/credbundle"
	"github.com/sky-ai-eng/triage-factory/internal/ghcli/ghinjector"
	"github.com/sky-ai-eng/triage-factory/internal/sidecarproto"
)

// The gh channel's repository gate is on for a token org alone, over the held
// bundle's authorized set: an App org's token is minted for that set already.
func TestRuntime_GHChannelRepositoriesFollowTheBundle(t *testing.T) {
	rt, err := newCredRuntime()
	if err != nil {
		t.Fatalf("newCredRuntime: %v", err)
	}
	rt.githubBase = "https://github.com"
	if _, _, err := rt.ghChannelRepositories(context.Background()); !errors.Is(err, credbundle.ErrNoBundle) {
		t.Errorf("no bundle: err = %v, want ErrNoBundle, which refuses", err)
	}

	held := func(gh *credbundle.GitHubCreds) {
		rt.mu.Lock()
		defer rt.mu.Unlock()
		rt.bundle = &credbundle.Bundle{GitHub: gh}
	}
	for name, gh := range map[string]*credbundle.GitHubCreds{
		"no GitHub half": nil,
		"an App org": {
			Mode: credbundle.GitHubModeApp, BaseURL: "https://github.com",
			RepoIDs: map[string]string{"acme/widgets": "4242"},
		},
		"another host": {
			Mode: credbundle.GitHubModePAT, BaseURL: "https://ghe.example.com",
			RepoIDs: map[string]string{"acme/widgets": "4242"},
		},
	} {
		held(gh)
		if repos, gated, err := rt.ghChannelRepositories(context.Background()); err != nil || gated {
			t.Errorf("%s: gated = %v, %v (%v), want no gate", name, gated, err, repos)
		}
	}

	held(&credbundle.GitHubCreds{
		Mode:       credbundle.GitHubModePAT,
		BaseURL:    "https://github.com",
		RepoIDs:    map[string]string{"acme/widgets": "4242", "acme/new": ""},
		RepoTokens: map[string]credbundle.RepoToken{"acme/widgets": {Token: "ghp"}, "acme/gears": {Token: "ghp"}},
	})
	repos, gated, err := rt.ghChannelRepositories(context.Background())
	if err != nil || !gated {
		t.Fatalf("a token org: gated = %v, %v, want the gate", gated, err)
	}
	slices.SortFunc(repos, func(a, b ghinjector.AuthorizedRepository) int {
		switch {
		case a.Name < b.Name:
			return -1
		case a.Name > b.Name:
			return 1
		}
		return 0
	})
	want := []ghinjector.AuthorizedRepository{
		{Name: "acme/gears"},
		{Name: "acme/new"},
		{Name: "acme/widgets", ID: "4242"},
	}
	if !slices.Equal(repos, want) {
		t.Errorf("repositories = %+v, want %+v", repos, want)
	}
}

// A token org's request for a repository outside the run is refused by the
// injector built from the production config, and the refusal is relayed for
// the orchestrator to record, with what GitHub answered for the name.
func TestRuntime_GHInjectorRelaysARepositoryDenial(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/repos/acme/payments" {
			_, _ = io.WriteString(w, `{"id": 9, "full_name": "acme/payments", "private": true}`)
			return
		}
		_, _ = io.WriteString(w, `{}`)
	}))
	defer upstream.Close()

	records := make(chan sidecarproto.RelayRecordBody, 4)
	handler := sidecarproto.HandlerFunc(func(_ context.Context, kind sidecarproto.Kind, body json.RawMessage) (any, error) {
		if kind != sidecarproto.KindRelayRecord {
			return nil, nil
		}
		var rc sidecarproto.RelayRecordBody
		if err := json.Unmarshal(body, &rc); err != nil {
			return nil, err
		}
		records <- rc
		return nil, nil
	})
	orch, rt := dialRuntimeWithHandler(t, handler)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	bundle := &credbundle.Bundle{
		GitHub: &credbundle.GitHubCreds{
			Mode:     credbundle.GitHubModePAT,
			BaseURL:  "https://github.com",
			RepoIDs:  map[string]string{"acme/widgets": "4242"},
			CLIToken: &credbundle.RepoToken{Token: "ghp_REALPAT"},
		},
	}
	if err := orch.Call(ctx, sidecarproto.KindSealedBundle,
		sidecarproto.SealedBundleBody{Sealed: sealBundleTo(t, rt.keypair.Public, bundle)}, nil); err != nil {
		t.Fatalf("relay bundle: %v", err)
	}
	rt.mu.Lock()
	rt.githubBase = "https://github.com"
	rt.mu.Unlock()

	cert, certPEM, err := ghinjector.GenerateCert("127.0.0.1")
	if err != nil {
		t.Fatalf("GenerateCert: %v", err)
	}
	const placeholder = "per-run-placeholder"
	srv, err := ghinjector.New(rt.ghInjectorConfig(upstream.URL, cert, placeholder, "run-under-test", nil))
	if err != nil {
		t.Fatalf("construct injector: %v", err)
	}
	host, err := srv.Start("127.0.0.1:0")
	if err != nil {
		t.Fatalf("start injector: %v", err)
	}
	t.Cleanup(func() {
		shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), time.Second)
		defer shutdownCancel()
		_ = srv.Shutdown(shutdownCtx)
	})
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(certPEM) {
		t.Fatal("append injector cert to pool failed")
	}
	client := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool}}}
	get := func(path string) int {
		t.Helper()
		req, _ := http.NewRequest(http.MethodGet, "https://"+host+path, nil)
		req.Header.Set("Authorization", "token "+placeholder)
		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("request through injector: %v", err)
		}
		_ = resp.Body.Close()
		return resp.StatusCode
	}

	if code := get("/api/v3/repos/acme/widgets/pulls/1"); code != http.StatusOK {
		t.Fatalf("the run's own repository: status %d, want it forwarded", code)
	}
	if code := get("/api/v3/repos/acme/payments/pulls/1"); code != http.StatusForbidden {
		t.Fatalf("a repository outside the run: status %d, want the gate's 403", code)
	}
	deadline := time.After(4 * time.Second)
	for {
		select {
		case got := <-records:
			if got.Op != agentproc.OpRecordGHRepositoryDenial {
				continue
			}
			var a agentproc.RecordGHRepositoryDenialArgs
			if err := json.Unmarshal(got.Args, &a); err != nil {
				t.Fatalf("unmarshal relayed denial: %v", err)
			}
			if want := (agentproc.RecordGHRepositoryDenialArgs{
				Repository: "acme/payments", RepositoryID: "9", Path: "/repos/acme/payments/pulls/1",
			}); a != want {
				t.Fatalf("relayed denial = %+v, want %+v", a, want)
			}
			return
		case <-deadline:
			t.Fatal("the gh channel's repository denial never reached the orchestrator")
		}
	}
}
