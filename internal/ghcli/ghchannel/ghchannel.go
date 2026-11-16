// Package ghchannel stands up the local-mode half of the real-`gh` credential
// channel: a per-run loopback injector plus the private on-disk surface one gh
// invocation needs to reach it.
//
// Multi mode gets the same channel from the per-run credential sidecar
// (cmd/runsidecar), which binds the injector on the run's veth IP and publishes
// its cert through a bind mount. Local mode has no sidecar and no jail, so the
// equivalent wiring lives here — the injector itself is already mode-agnostic
// (internal/ghcli/ghinjector imports nothing from the sandbox), and this package only
// supplies what the sidecar would otherwise supply: a listener, a trust file, a
// per-run placeholder, and somewhere for gh to keep its config.
//
// # Why a per-run gh config dir
//
// A local run executes under the user's own HOME, so gh would otherwise read
// ~/.config/gh — including the hosts entry holding the user's personal GitHub
// credential. Every channel here points gh at a per-run directory instead,
// holding no setting but the socket's, which is the local-mode analogue of the
// jail's HOME isolation: the only credential in play is the per-run
// placeholder, and the only real one lives in this process and is injected on
// the upstream hop.
//
// # Why gh is on a unix socket
//
// gh resolves a command's repository by matching the checkout's git remotes
// against GH_HOST, and drops a remote's port before it compares, so a GH_HOST
// with a port matches no remote. Git's rewritten remotes name the injector's
// TLS listener, "127.0.0.1:<port>", and a local process cannot bind the port
// 443 that would let GH_HOST leave it out. So gh is given a GH_HOST of
// "127.0.0.1" and a config whose http_unix_socket sends every request it makes
// to the injector over a socket in the channel directory, while git keeps the
// ported listener. When the socket cannot be set up, GH_HOST keeps the port
// and a repository-contextual gh command needs -R.
//
// # Lifecycle
//
// One channel per run: Start before the agent subprocess, Close when the run
// ends. Close stops the listeners and removes the channel directory, so a run
// that outlives its channel gets connection-refused — which surfaces to the
// agent as an ordinary gh error, the same contract multi mode has.
package ghchannel

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"syscall"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/cabundle"
	"github.com/sky-ai-eng/triage-factory/internal/ghcli/ghinjector"
	"github.com/sky-ai-eng/triage-factory/internal/logging"
	"github.com/sky-ai-eng/triage-factory/internal/paths"
	"github.com/sky-ai-eng/triage-factory/internal/proxy/proxycert"
)

// shutdownTimeout bounds the listener drain on Close. The injector holds no
// state worth waiting on; an in-flight request either completes promptly or the
// run is already over.
const shutdownTimeout = 3 * time.Second

// socketHost is GH_HOST while gh reaches the injector over the channel's unix
// socket: the TLS listener's address without its port, which is what gh
// compares a rewritten remote against.
const socketHost = "127.0.0.1"

// socketPathLimit is the size of a unix socket address's path field: 108 bytes
// on Linux, 104 on macOS. A path that fills it leaves no room for the NUL a
// client written in C expects after it, so the channel's socket path must be
// shorter.
const socketPathLimit = len(syscall.RawSockaddrUnix{}.Path)

var channelLog = logging.Component("ghchannel")

// Config is one run's channel inputs.
type Config struct {
	// ConversationID scopes the on-disk directory. Required.
	ConversationID string

	// Upstream is the org's real REST API base ("https://api.github.com" or a
	// GHES "{host}/api/v3"). Required: the channel attaches the org's
	// credential, so it is told exactly where that credential goes.
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

	// ObserveWrite, when non-nil, receives every mutating REST request the
	// injector forwarded, with its outcome — the write audit. Local mode wires
	// this straight into the recorder in-process; there is no relay hop to make.
	ObserveWrite func(context.Context, ghinjector.ObservedWrite)

	// ObserveRead, when non-nil, receives each pull request or issue a read
	// addressed, once per run — the run's touched entities. Wired in-process
	// like ObserveWrite.
	ObserveRead func(context.Context, ghinjector.ObservedRead)

	// AuthorizeWrite decides the shapes the injector's refusal policy gates and
	// records their denials. Multi mode relays the same decision to the
	// orchestrator; here it is an ordinary call, since the process holding the
	// policy and the one running the proxy are the same process. Leaving it nil
	// refuses every gated shape without recording one — see the injector's own
	// doc for why that is the fail-closed reading rather than an off switch.
	AuthorizeWrite ghinjector.AuthorizeWrite

	// Repositories and DenyRepository are the injector's repository gate: the
	// run's authorized set, read per request, and the record of each refusal.
	// Local mode wires both, since no local credential is confined to the run.
	Repositories   ghinjector.RepositorySource
	DenyRepository func(context.Context, ghinjector.DeniedRepository)

	// GitHandler, when non-nil, serves git smart HTTP on the injector's TLS
	// listener, the origin git's rewritten remotes name (Channel.GitHost).
	GitHandler http.Handler
}

// Channel is a live per-run injector and the coordinates an agent subprocess
// needs to use it. Every field is safe to hand to the agent: Token is the
// per-run placeholder, never the org's credential.
type Channel struct {
	// Host is GH_HOST: "127.0.0.1" when gh reaches the injector over the
	// channel's unix socket, which ConfigDir's config.yml names, or GitHost
	// when the socket could not be set up.
	Host string
	// GitHost is the injector's TLS listener, "127.0.0.1:<port>", which git's
	// rewritten remotes name.
	GitHost string
	// Token is the per-run placeholder — GH_ENTERPRISE_TOKEN. The injector
	// strips it and injects the real credential upstream.
	Token string
	// CertPath is the per-run trust file — SSL_CERT_FILE, and git's CA for
	// GitHost.
	CertPath string
	// ConfigDir is the per-run gh config directory — GH_CONFIG_DIR.
	ConfigDir string
	// BinDir is the directory the pinned gh lives in, for the PATH prepend.
	BinDir string

	srv        *ghinjector.Server
	channelDir string
}

// Start writes the per-run trust surface, binds the injector on loopback, and
// serves it on the channel's unix socket. On any error nothing is left running
// and the channel directory is removed — callers degrade to no gh channel
// rather than to a half-built one. A socket that cannot be set up is not an
// error: the channel starts with gh on the TLS listener (see the package doc).
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
		return nil, fmt.Errorf("ghchannel: Upstream is required")
	}

	// Pre-flight the state root through the error-returning form: the path
	// resolvers below panic on an unresolvable $HOME, and this constructor
	// returns an error, so a missing home has to surface as one.
	if _, err := paths.StateRootErr(); err != nil {
		return nil, fmt.Errorf("ghchannel: resolve state root: %w", err)
	}
	channelDir := paths.GHChannelDir(cfg.ConversationID)
	configDir := filepath.Join(channelDir, "config")
	// A directory already here was left by a process that died before its
	// Close: its socket file would refuse the bind, and its gh config would
	// still send gh to that dead socket. One channel holds a conversation's
	// directory at a time, so what is here belongs to none.
	if err := os.RemoveAll(channelDir); err != nil {
		return nil, fmt.Errorf("ghchannel: clear channel dir: %w", err)
	}
	// 0700 throughout: the agent runs as this same user in local mode, so the
	// permissions are not a boundary against it — they keep the run's surface
	// off other accounts on a shared machine.
	if err := os.MkdirAll(configDir, 0o700); err != nil {
		return nil, fmt.Errorf("ghchannel: create channel dir: %w", err)
	}
	fail := func(err error) (*Channel, error) {
		_ = os.RemoveAll(channelDir)
		return nil, err
	}

	cert, certPEM, err := proxycert.Generate("gh injector", "127.0.0.1")
	if err != nil {
		return fail(fmt.Errorf("ghchannel: generate cert: %w", err))
	}
	certPath := filepath.Join(channelDir, "injector.crt")
	// The system roots alongside this run's leaf: SSL_CERT_FILE is
	// process-global, so a leaf-only file would narrow every other TLS client
	// in the agent subprocess. See cabundle.WithSystemRoots.
	if err := os.WriteFile(certPath, cabundle.WithSystemRoots(certPEM), 0o600); err != nil {
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
		ObserveWrite:   cfg.ObserveWrite,
		ObserveRead:    cfg.ObserveRead,
		AuthorizeWrite: cfg.AuthorizeWrite,
		Repositories:   cfg.Repositories,
		DenyRepository: cfg.DenyRepository,
		GitHandler:     cfg.GitHandler,
		// Loopback only — no veth, no other host may reach this listener.
	})
	if err != nil {
		return fail(fmt.Errorf("ghchannel: construct injector: %w", err))
	}
	gitHost, err := srv.Start(net.JoinHostPort("127.0.0.1", "0"))
	if err != nil {
		return fail(fmt.Errorf("ghchannel: start injector: %w", err))
	}
	host := gitHost
	if serveSocket(srv, cfg.ConversationID, channelDir, configDir) {
		host = socketHost
	}

	return &Channel{
		Host:       host,
		GitHost:    gitHost,
		Token:      token,
		CertPath:   certPath,
		ConfigDir:  configDir,
		BinDir:     cfg.BinDir,
		srv:        srv,
		channelDir: channelDir,
	}, nil
}

// serveSocket binds the channel's unix socket, serves srv on it, and writes the
// gh config that sends gh's requests there, reporting whether gh is on the
// socket. A failure is logged and answered false, and leaves neither the
// socket nor the config line behind, since a channel on the TLS listener alone
// still works.
func serveSocket(srv *ghinjector.Server, conversationID, channelDir, configDir string) bool {
	path := filepath.Join(channelDir, "gh.sock")
	if len(path) >= socketPathLimit {
		channelLog.Warn("gh channel socket path is too long; gh stays on the ported address and needs -R to name a repository",
			"conversation", conversationID, "path_length", len(path), "limit", socketPathLimit)
		return false
	}
	warn := func(err error) bool {
		channelLog.Warn("gh channel socket not served; gh stays on the ported address and needs -R to name a repository",
			"conversation", conversationID, "path_length", len(path), "error", err)
		return false
	}
	ln, err := net.Listen("unix", path)
	if err != nil {
		return warn(err)
	}
	// The channel directory already keeps other accounts out. The socket gets
	// the mode of the channel's other files rather than the umask's.
	if err := os.Chmod(path, 0o600); err != nil {
		_ = ln.Close()
		return warn(err)
	}
	// Quoted, since a home directory can hold characters a plain YAML scalar
	// cannot. Go's escapes are YAML's for a path that is valid UTF-8, and a
	// YAML file can hold no other.
	configPath := filepath.Join(configDir, "config.yml")
	if err := os.WriteFile(configPath, []byte("http_unix_socket: "+strconv.Quote(path)+"\n"), 0o600); err != nil {
		_ = ln.Close()
		return warn(err)
	}
	if err := srv.ServeUnix(ln); err != nil {
		_ = os.Remove(configPath)
		_ = ln.Close()
		return warn(err)
	}
	return true
}

// Close stops the injector and removes the channel directory. Idempotent and
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
	if c.channelDir != "" {
		_ = os.RemoveAll(c.channelDir)
		c.channelDir = ""
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
