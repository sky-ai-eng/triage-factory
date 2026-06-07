package app

import (
	"flag"
	"fmt"
	"io"
)

const (
	// DefaultPort is the server's listen port when --port is omitted.
	DefaultPort = 3000
	// DefaultHost binds to loopback only. Triage Factory is a local-first
	// tool that holds keychain-backed credentials and an unauthenticated
	// HTTP API; exposing it on all interfaces by default would let anyone
	// on the same network drive delegated runs. Override with --host if
	// you genuinely want LAN access.
	DefaultHost = "127.0.0.1"
)

// Config holds the server-mode inputs derived from the command line. It's
// produced by LoadConfig and consumed by New; everything in it is a pure
// function of the flags (no I/O, no DB), which keeps boot easy to reason
// about and to test.
type Config struct {
	Host      string
	Port      int
	NoBrowser bool

	// Addr is the host:port the HTTP server binds. BrowserURL is the
	// pretty, user-facing address (loopback shown as "localhost"). Both
	// are derived from Host/Port in LoadConfig.
	Addr       string
	BrowserURL string
}

// LoadConfig parses the server-mode flags (--port, --host, --no-browser)
// and derives the bind address + browser URL. args is the argument slice
// with the program name already stripped (os.Args[1:]); CLI subcommands
// must be dispatched before this is called.
func LoadConfig(args []string) (Config, error) {
	cfg := Config{Host: DefaultHost, Port: DefaultPort}

	fs := flag.NewFlagSet("triagefactory", flag.ContinueOnError)
	// Silence the flag package's own usage dump on a parse error; main
	// reports the returned error as a single clean line. `--help` is served
	// separately via the CLI dispatch (see cli.go).
	fs.SetOutput(io.Discard)
	fs.IntVar(&cfg.Port, "port", DefaultPort, "port to listen on")
	fs.StringVar(&cfg.Host, "host", DefaultHost, "bind address (use 0.0.0.0 for LAN access)")
	fs.BoolVar(&cfg.NoBrowser, "no-browser", false, "do not open a browser on start")
	if err := fs.Parse(args); err != nil {
		return Config{}, err
	}

	cfg.Addr = fmt.Sprintf("%s:%d", cfg.Host, cfg.Port)

	// Display host: keep "localhost" in the browser-facing URL when bound
	// to loopback (prettier, and what users expect), but show the actual
	// bind host when overridden so it's obvious where the server is
	// reachable.
	displayHost := cfg.Host
	if cfg.Host == "127.0.0.1" || cfg.Host == "" {
		displayHost = "localhost"
	}
	cfg.BrowserURL = fmt.Sprintf("http://%s:%d", displayHost, cfg.Port)
	return cfg, nil
}
