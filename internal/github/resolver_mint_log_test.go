package github

import (
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/logging"
	"github.com/sky-ai-eng/triage-factory/internal/upstream"
)

type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// TestClientFor_UnreachableMintLogsBelowWarn: the installation-token mint
// meeting a proxy's 502 is the host being unreachable, which the poller
// reports once as the connection being lost. The resolver's own line about
// the failed mint is one of the per-request failures in between, so it logs
// at Debug, and the error it returns keeps its class for the caller.
func TestClientFor_UnreachableMintLogsBelowWarn(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		_, _ = w.Write([]byte("<html><body>502 Bad Gateway</body></html>"))
	}))
	t.Cleanup(srv.Close)

	out := &syncBuffer{}
	restore := logging.SetOutput(out)
	prev := logging.Level()
	logging.SetLevel(slog.LevelInfo)
	t.Cleanup(func() {
		logging.SetLevel(prev)
		restore()
	})

	r := newTestResolver(
		&fakeSecrets{vals: map[string]string{"pem": testPEM(t)}},
		&fakeApps{app: activeApp(), insts: []domain.OrgGitHubAppInstallation{installOn("acme")}},
		&fakeOrgs{base: srv.URL},
		&fakeAgents{},
		nil,
	)
	_, err := r.ClientFor(context.Background(), "org-1", "acme")
	if class, ok := upstream.ClassOf(err); !ok || class != upstream.Transient {
		t.Fatalf("ClientFor = %v, class (%q, %v); want a transient failure", err, class, ok)
	}
	if got := out.String(); strings.Contains(got, "mint installation token failed") {
		t.Errorf("an unreachable host's mint failure logged at INFO or above:\n%s", got)
	}
}
