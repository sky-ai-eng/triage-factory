// Package deploy is the CLI entrypoint for `triagefactory deploy`, which
// makes the binary the installer for a self-hosted multi-mode deployment:
//
//	triagefactory deploy init     render a deployment directory
//	triagefactory deploy doctor   check the host, the rendered files and the stack
//	triagefactory deploy up       start it and wait for it to be healthy
//	triagefactory deploy upgrade  move it to this binary's version
//
// The deployment stays a docker compose project — Postgres, GoTrue, SeaweedFS
// and the executor's gVisor jails need containers — and deploy renders it
// from the compose bundle the binary embeds, the same files a release's
// compose tarball carries. init pins the control and executor image by a
// digest whose cosign signature it has verified, and writes a .env holding
// every secret boot needs.
//
// Every subcommand works on one deployment directory (--dir, default the
// current directory) and first checks that the host can run the stack at
// all: Linux, a reachable Docker Engine, and Compose 2 or later. The executor
// needs Linux, so a host that fails the gate is told why and nothing is
// written; there is no rendering a bundle on one machine to run on another.
//
// The binary's version decides the image a release deployment runs, so up
// refuses a binary that is not the deployment's version, and upgrade is the
// one way to move a deployment to a newer one. Upgrades are orderly, not
// zero-downtime: the API is down while control restarts, and runs in flight
// are drained or handed off with workspace snapshots.
//
// deploy runs before any server wiring, opens no database, and writes nothing
// outside the deployment directory.
package deploy

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"

	"golang.org/x/term"
)

// Bundle is what deploy renders from: the files the binary embeds, at their
// repository paths, and the binary's own version.
type Bundle struct {
	Files   fs.FS
	Version string
}

// Handle dispatches `triagefactory deploy ...` and exits with its status.
func Handle(args []string, b Bundle) {
	os.Exit(run(context.Background(), args, b, newHost()))
}

func run(ctx context.Context, args []string, b Bundle, h *host) int {
	if len(args) == 0 {
		fmt.Fprint(h.stderr, usage)
		return 2
	}
	switch args[0] {
	case "init":
		return runInit(ctx, args[1:], b, h)
	case "doctor":
		return runDoctor(ctx, args[1:], b, h)
	case "up":
		return runUp(ctx, args[1:], b, h)
	case "upgrade":
		return runUpgrade(ctx, args[1:], b, h)
	case "-h", "--help", "help":
		fmt.Fprint(h.stdout, usage)
		return 0
	default:
		fmt.Fprintf(h.stderr, "unknown deploy subcommand %q\n\n%s", args[0], usage)
		return 2
	}
}

// host is everything deploy touches outside the deployment directory: the
// platform, the docker CLI, the network and the terminal. Tests replace it.
type host struct {
	goos   string
	stdin  io.Reader
	stdout io.Writer
	stderr io.Writer

	// terminal reports whether stdin is a terminal init can prompt on, and
	// readSecret reads one line from it without echoing it.
	terminal   bool
	readSecret func() (string, error)

	// docker runs the docker CLI with args, in dir when dir is not empty.
	docker func(ctx context.Context, dir string, args ...string) (stdout, stderr []byte, err error)

	// cgroupControllers exists exactly when the host runs the unified (v2)
	// cgroup hierarchy.
	cgroupControllers string
	// portFree reports whether a TCP port on the host can be bound.
	portFree func(port int) bool

	// stream runs the docker CLI with args, in dir when dir is not empty,
	// with its output on the terminal and no time limit: compose pulling,
	// building and starting a stack takes as long as it takes, and the
	// operator watches it.
	stream func(ctx context.Context, dir string, args ...string) error

	// resolveImage pins a release's image by a digest whose signature it
	// has verified, and verifyImage verifies the signature of an image
	// already pinned (repository:version@sha256:…).
	resolveImage func(ctx context.Context, version string) (string, error)
	verifyImage  func(ctx context.Context, version, ref string) error

	// getenv reads the environment deploy runs compose in, which compose
	// interpolates the compose file from alongside .env.
	getenv func(string) string

	// now and sleep are the clock the waits in up and upgrade run on.
	now   func() time.Time
	sleep func(ctx context.Context, d time.Duration) error

	// http is how the health checks reach a running stack, at the host
	// ports its containers publish.
	http *http.Client
}

func newHost() *host {
	stdinFD := int(os.Stdin.Fd())
	return &host{
		goos:       runtime.GOOS,
		stdin:      os.Stdin,
		stdout:     os.Stdout,
		stderr:     os.Stderr,
		terminal:   term.IsTerminal(stdinFD),
		readSecret: func() (string, error) { return readSecretRestoringOnSignal(stdinFD) },
		docker: func(ctx context.Context, dir string, args ...string) ([]byte, []byte, error) {
			// Bounded, so a daemon that accepts the connection and never
			// answers fails the check instead of hanging the command.
			ctx, cancel := context.WithTimeout(ctx, dockerCallTimeout)
			defer cancel()
			cmd := exec.CommandContext(ctx, "docker", args...)
			cmd.Dir = dir
			var stdout, stderr strings.Builder
			cmd.Stdout, cmd.Stderr = &stdout, &stderr
			err := cmd.Run()
			if err != nil && errors.Is(ctx.Err(), context.DeadlineExceeded) {
				err = fmt.Errorf("docker %s did not finish within %s", strings.Join(args, " "), dockerCallTimeout)
				return []byte(stdout.String()), nil, err
			}
			return []byte(stdout.String()), []byte(stderr.String()), err
		},
		cgroupControllers: "/sys/fs/cgroup/cgroup.controllers",
		portFree: func(port int) bool {
			l, err := net.Listen("tcp", ":"+strconv.Itoa(port))
			if err != nil {
				return false
			}
			_ = l.Close()
			return true
		},
		stream: func(ctx context.Context, dir string, args ...string) error {
			cmd := exec.CommandContext(ctx, "docker", args...)
			cmd.Dir = dir
			cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
			return cmd.Run()
		},
		resolveImage: func(ctx context.Context, version string) (string, error) {
			return newImageVerifier(version).resolve(ctx, version)
		},
		verifyImage: func(ctx context.Context, version, ref string) error {
			return newImageVerifier(version).verifyPinned(ctx, version, ref)
		},
		getenv: os.Getenv,
		now:    time.Now,
		sleep: func(ctx context.Context, d time.Duration) error {
			t := time.NewTimer(d)
			defer t.Stop()
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-t.C:
				return nil
			}
		},
		http: &http.Client{Timeout: 5 * time.Second},
	}
}

// readSecretRestoringOnSignal reads one line from the terminal at fd without
// echoing it. term.ReadPassword restores echo only when it returns, and an
// interrupt kills the process before it does, which would leave the
// operator's shell not echoing what they type; so an interrupt during the
// read restores the terminal first and then exits as the signal would have.
func readSecretRestoringOnSignal(fd int) (string, error) {
	state, err := term.GetState(fd)
	if err != nil {
		return "", err
	}
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(sigs)
	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case sig := <-sigs:
			_ = term.Restore(fd, state)
			fmt.Fprintln(os.Stderr)
			code := 130
			if s, ok := sig.(syscall.Signal); ok {
				code = 128 + int(s)
			}
			os.Exit(code)
		case <-done:
		}
	}()
	b, err := term.ReadPassword(fd)
	return string(b), err
}

// status is one check's outcome. A fail makes the command exit non-zero; a
// warn does not; a skip is a check that does not apply yet.
type status string

const (
	pass status = "pass"
	warn status = "warn"
	fail status = "fail"
	skip status = "skip"
)

type check struct {
	status status
	name   string
	detail string
}

func (h *host) heading(title string) {
	fmt.Fprintf(h.stdout, "%s\n", title)
}

func (h *host) report(c check) {
	line := fmt.Sprintf("  %-4s  %-18s %s", c.status, c.name, c.detail)
	fmt.Fprintln(h.stdout, strings.TrimRight(line, " "))
}

// reportAll prints every check and reports whether any failed.
func (h *host) reportAll(cs []check) (failed bool) {
	for _, c := range cs {
		h.report(c)
		failed = failed || c.status == fail
	}
	return failed
}

// gate is what every subcommand checks before anything else: a host that
// cannot run the stack. It reports whether the gate held alongside the
// checks, so a caller stops before reading or writing anything more.
func gate(ctx context.Context, h *host) ([]check, bool) {
	var cs []check
	if h.goos == "linux" {
		cs = append(cs, check{pass, "Linux", ""})
	} else {
		cs = append(cs, check{fail, "Linux", fmt.Sprintf("this host is %s; the executor's sandbox (gVisor, its capabilities, cgroup v2) needs a Linux Docker host", h.goos)})
	}

	out, errOut, err := h.docker(ctx, "", "version", "--format", "{{.Server.Version}}")
	noCLI := errors.Is(err, exec.ErrNotFound)
	switch {
	case noCLI:
		cs = append(cs, check{fail, "Docker Engine", "the docker CLI is not on PATH"})
	case err != nil:
		cs = append(cs, check{fail, "Docker Engine", "not reachable: " + firstLine(errOut, err)})
	default:
		cs = append(cs, check{pass, "Docker Engine", strings.TrimSpace(string(out))})
	}

	if noCLI {
		cs = append(cs, check{fail, "Docker Compose", "the docker CLI is not on PATH"})
	} else {
		cs = append(cs, composeCheck(ctx, h))
	}

	held := true
	for _, c := range cs {
		held = held && c.status != fail
	}
	return cs, held
}

// composeCheck wants the compose plugin (`docker compose`), version 2 or
// later — the line docker-compose.yml's syntax (a top-level name:, merge
// keys across files, cgroup: private) is written for.
func composeCheck(ctx context.Context, h *host) check {
	out, errOut, err := h.docker(ctx, "", "compose", "version", "--short")
	if err != nil {
		return check{fail, "Docker Compose", "`docker compose` is not available: " + firstLine(errOut, err)}
	}
	version := strings.TrimPrefix(strings.TrimSpace(string(out)), "v")
	majorStr, _, _ := strings.Cut(version, ".")
	major, convErr := strconv.Atoi(majorStr)
	if convErr != nil {
		return check{fail, "Docker Compose", fmt.Sprintf("cannot read the version from %q", version)}
	}
	if major < 2 {
		return check{fail, "Docker Compose", fmt.Sprintf("version %s; deploy needs 2 or later", version)}
	}
	return check{pass, "Docker Compose", version}
}

// preflight is the gate plus what the stack needs from the host beyond it.
// firstInit adds the port check, which only means something before this
// deployment's own control service holds the port.
func preflight(ctx context.Context, h *host, firstInit bool) ([]check, bool) {
	cs, held := gate(ctx, h)
	if !held {
		return cs, false
	}
	if _, err := os.Stat(h.cgroupControllers); err == nil {
		cs = append(cs, check{pass, "cgroup v2", ""})
	} else {
		cs = append(cs, check{fail, "cgroup v2", fmt.Sprintf("%s is missing: this host runs cgroup v1, and the executor's per-run memory ceilings need v2", h.cgroupControllers)})
	}
	if firstInit {
		for _, p := range publishedPorts {
			name := "Port " + strconv.Itoa(p.port)
			if h.portFree(p.port) {
				cs = append(cs, check{pass, name, "free"})
			} else {
				cs = append(cs, check{warn, name, fmt.Sprintf("in use; the %s service publishes it, so the stack cannot start until it is free (or compose.override.yml moves it)", p.service)})
			}
		}
	}
	return cs, true
}

// dockerCallTimeout bounds one docker CLI call. The slowest deploy makes is
// doctor's container start under the seccomp profile, which takes seconds.
const dockerCallTimeout = 2 * time.Minute

// publishedPorts are the host ports docker-compose.yml publishes, each with
// the service that publishes it. Any one of them held by another process
// stops `docker compose up`.
var publishedPorts = []struct {
	port    int
	service string
}{
	{3000, "control"},
	{9999, "gotrue"},
	{3030, "grafana"},
}

// firstLine is the first non-empty line of a command's stderr, or err's text
// when stderr is empty: enough to say why a docker call failed on one line.
func firstLine(stderr []byte, err error) string {
	for _, line := range strings.Split(string(stderr), "\n") {
		if line = strings.TrimSpace(line); line != "" {
			return line
		}
	}
	if err != nil {
		return err.Error()
	}
	return "no output"
}

// deploymentDir resolves --dir to an absolute path.
func deploymentDir(dir string) (string, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return "", fmt.Errorf("resolve --dir %q: %w", dir, err)
	}
	return abs, nil
}

// newFlagSet is a subcommand's flag set: errors are returned rather than
// exiting, and usage goes to stderr.
func newFlagSet(name, usageText string, h *host) *flag.FlagSet {
	fset := flag.NewFlagSet(name, flag.ContinueOnError)
	fset.SetOutput(h.stderr)
	fset.Usage = func() { fmt.Fprint(h.stderr, usageText) }
	return fset
}

// parseFlags parses args into fset and refuses positional arguments, which
// no deploy subcommand takes. It returns the exit status to stop with, or -1
// to continue.
func parseFlags(fset *flag.FlagSet, args []string, h *host) int {
	if err := fset.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if fset.NArg() > 0 {
		fmt.Fprintf(h.stderr, "%s takes no arguments, got %q\n", fset.Name(), fset.Args())
		return 2
	}
	return -1
}

const usage = `triagefactory deploy — render, start, upgrade and check a self-hosted
multi-mode deployment.

USAGE
  triagefactory deploy init [flags]     render a deployment directory: the
                                        compose bundle this binary carries, the
                                        release image pinned by a verified
                                        digest, and a .env with every secret
                                        boot needs
  triagefactory deploy up [flags]       start the deployment, with the stored
                                        executor count, and wait for it to be
                                        healthy
  triagefactory deploy upgrade [flags]  move the deployment to this binary's
                                        version: verify the new image, drain the
                                        executors, re-render, restart
  triagefactory deploy doctor [flags]   check the host, the rendered files and
                                        the running stack

A release deployment runs the image of the binary's own version. After
installing a newer binary, run deploy upgrade before deploy up.

Every subcommand takes --dir (default: the current directory), the deployment
directory it works on, and first checks the host: Linux, a reachable Docker
Engine, and Docker Compose 2 or later. A host that fails is told why, and
nothing is written.

Run "triagefactory deploy <subcommand> --help" for a subcommand's flags.
`
