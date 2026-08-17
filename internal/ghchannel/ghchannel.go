// Package ghchannel stands up the local-mode half of the real-`gh` credential
// channel: a per-run loopback injector plus the private on-disk surface one gh
// invocation needs to reach it.
//
// Multi mode gets the same channel from the per-run credential sidecar
// (cmd/runsidecar), which binds the injector on the run's veth IP and publishes
// its cert through a bind mount. Local mode has no sidecar and no jail, so the
// equivalent wiring lives here — the injector itself is already mode-agnostic
// (internal/ghinjector imports nothing from the sandbox), and this package only
// supplies what the sidecar would otherwise supply: a listener, a trust file, a
// per-run placeholder, and somewhere for gh to keep its config.
//
// # Why a per-run gh config dir
//
// A local run executes under the user's own HOME, so gh would otherwise read
// ~/.config/gh — including the hosts entry holding the user's personal GitHub
// credential. Every channel here points gh at an empty per-run directory
// instead, which is the local-mode analogue of the jail's HOME isolation: the
// only credential in play is the per-run placeholder, and the only real one
// lives in this process and is injected on the upstream hop.
//
// # Lifecycle
//
// One channel per run: Start before the agent subprocess, Close when the run
// ends. Close stops the listener and removes the run's directory, so a run that
// outlives its channel gets connection-refused — which surfaces to the agent as
// an ordinary gh error, the same contract multi mode has.
package ghchannel

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/ghinjector"
	"github.com/sky-ai-eng/triage-factory/internal/paths"
)

// shutdownTimeout bounds the listener drain on Close. The injector holds no
// state worth waiting on; an in-flight request either completes promptly or the
// run is already over.
const shutdownTimeout = 3 * time.Second

// Config is one run's channel inputs.
type Config struct {
	// ConversationID scopes the on-disk directory. Required.
	ConversationID string

	// Upstream is the org's real REST API base ("https://api.github.com" or a
	// GHES "{host}/api/v3"). Empty defaults to api.github.com.
	Upstream string

	// BinDir is the directory holding the TF-owned gh binary. It is prepended
	// to the agent subprocess's PATH so `gh` resolves there and never to the
	// user's own installation. Required — a channel with no binary to reach it
	// is not a channel.
	BinDir string

	// TokenSource resolves the org's real GitHub credential. Called per
	// request with no caching in front of it, so a rotation mid-run is picked
	// up on the next call. Required.
	//
	// This is the one place the executor rule "never read stores.Secrets"
	// does not apply: local mode has no executor split and no sealed bundle,
	// so the live secret store IS the credential source.
	TokenSource func(context.Context) (string, error)

	// Observe, when non-nil, receives each artifact-bearing mutation the
	// injector sees complete. Local mode wires this straight into the artifact
	// recorder in-process — there is no relay hop to make.
	Observe func(context.Context, ghinjector.ObservedMutation)

	// ObserveWrite, when non-nil, receives every mutating REST request the
	// injector forwarded, with its outcome — the audit-log half of the same
	// channel. Wired in-process too.
	ObserveWrite func(context.Context, ghinjector.ObservedWrite)

	// AuthorizeWrite decides the shapes the injector's refusal policy gates and
	// records their denials. Multi mode relays the same decision to the
	// orchestrator; here it is an ordinary call, since the process holding the
	// policy and the one running the proxy are the same process. Leaving it nil
	// refuses every gated shape without recording one — see the injector's own
	// doc for why that is the fail-closed reading rather than an off switch.
	AuthorizeWrite ghinjector.AuthorizeWrite
}

// Channel is a live per-run injector and the coordinates an agent subprocess
// needs to use it. Every field is safe to hand to the agent: Token is the
// per-run placeholder, never the org's credential.
type Channel struct {
	// Host is the injector's bound "127.0.0.1:<port>" — GH_HOST.
	Host string
	// Token is the per-run placeholder — GH_ENTERPRISE_TOKEN. The injector
	// strips it and injects the real credential upstream.
	Token string
	// CertPath is the per-run trust file — SSL_CERT_FILE.
	CertPath string
	// ConfigDir is the per-run gh config directory — GH_CONFIG_DIR.
	ConfigDir string
	// BinDir is the directory the pinned gh lives in, for the PATH prepend.
	BinDir string

	srv    *ghinjector.Server
	runDir string
}

// Start writes the per-run trust surface and binds the injector on loopback.
// On any error nothing is left running and the run directory is removed —
// callers degrade to no gh channel rather than to a half-built one.
func Start(cfg Config) (*Channel, error) {
	if cfg.ConversationID == "" {
		return nil, fmt.Errorf("ghchannel: ConversationID is required")
	}
	if cfg.BinDir == "" {
		return nil, fmt.Errorf("ghchannel: BinDir is required")
	}
	if cfg.TokenSource == nil {
		return nil, fmt.Errorf("ghchannel: TokenSource is required")
	}
	upstream := cfg.Upstream
	if upstream == "" {
		upstream = "https://api.github.com"
	}

	// Pre-flight the state root through the error-returning form: the path
	// resolvers below panic on an unresolvable $HOME, and this constructor
	// returns an error, so a missing home has to surface as one.
	if _, err := paths.StateRootErr(); err != nil {
		return nil, fmt.Errorf("ghchannel: resolve state root: %w", err)
	}
	runDir := paths.GHChannelRunDir(cfg.ConversationID)
	configDir := filepath.Join(runDir, "config")
	// 0700 throughout: the agent runs as this same user in local mode, so the
	// permissions are not a boundary against it — they keep the run's surface
	// off other accounts on a shared machine.
	if err := os.MkdirAll(configDir, 0o700); err != nil {
		return nil, fmt.Errorf("ghchannel: create run dir: %w", err)
	}
	fail := func(err error) (*Channel, error) {
		_ = os.RemoveAll(runDir)
		return nil, err
	}

	cert, certPEM, err := ghinjector.GenerateCert("127.0.0.1")
	if err != nil {
		return fail(fmt.Errorf("ghchannel: generate cert: %w", err))
	}
	certPath := filepath.Join(runDir, "injector.crt")
	// The system roots alongside this run's leaf: SSL_CERT_FILE is
	// process-global, so a leaf-only file would narrow every other TLS client
	// in the agent subprocess. See ghinjector.TrustBundlePEM.
	if err := os.WriteFile(certPath, ghinjector.TrustBundlePEM(certPEM), 0o600); err != nil {
		return fail(fmt.Errorf("ghchannel: write trust file: %w", err))
	}

	token, err := placeholderToken()
	if err != nil {
		return fail(err)
	}
	srv, err := ghinjector.New(ghinjector.Config{
		Upstream:       upstream,
		IncomingToken:  token,
		Cert:           cert,
		ConversationID: cfg.ConversationID,
		TokenSource:    cfg.TokenSource,
		Observe:        cfg.Observe,
		ObserveWrite:   cfg.ObserveWrite,
		AuthorizeWrite: cfg.AuthorizeWrite,
		// Loopback only — no veth, no other host may reach this listener.
	})
	if err != nil {
		return fail(fmt.Errorf("ghchannel: construct injector: %w", err))
	}
	host, err := srv.Start(net.JoinHostPort("127.0.0.1", "0"))
	if err != nil {
		return fail(fmt.Errorf("ghchannel: start injector: %w", err))
	}

	return &Channel{
		Host:      host,
		Token:     token,
		CertPath:  certPath,
		ConfigDir: configDir,
		BinDir:    cfg.BinDir,
		srv:       srv,
		runDir:    runDir,
	}, nil
}

// Close stops the injector and removes the run's directory. Idempotent and
// nil-safe, so callers can defer it unconditionally.
func (c *Channel) Close() error {
	if c == nil {
		return nil
	}
	if c.srv != nil {
		ctx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
		_ = c.srv.Shutdown(ctx)
		cancel()
		c.srv = nil
	}
	if c.runDir != "" {
		_ = os.RemoveAll(c.runDir)
		c.runDir = ""
	}
	return nil
}

// placeholderToken mints the per-run value the agent's gh presents. A sibling
// run holds a different one and so cannot spend this run's credential.
func placeholderToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("ghchannel: generate placeholder token: %w", err)
	}
	return hex.EncodeToString(b), nil
}
