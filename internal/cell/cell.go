// Package cell is the contract between a multi-mode executor and whatever
// builds the cell a delegated run executes in. A cell is one engagement's
// isolated environment taken as a unit: its network, its credential sidecar,
// its run-tree grants and its jail. The gVisor implementation lives in
// internal/cell/gvisor; internal/cell/celltest is the conformance suite every
// implementation must pass. docs/for-agents/specs/sandbox-backends/ is the
// design of record.
//
// Everything that is the product stays outside this package and identical on
// every backend: the native agent loop, the tool host and its protocol, the
// sidecar and the policy its proxies enforce, credential sealing, and what
// goes into the run tree. A backend owns only how a cell is built, how the
// jail is reached, and how it is torn down.
package cell

import (
	"context"
	"net"
	"time"
)

// Backend builds and reclaims cells. An executor process holds one.
type Backend interface {
	// Name is the backend's operator-facing identifier ("gvisor").
	Name() string

	// Provision builds a cell for one engagement of spec.ConversationID: its
	// network and its credential sidecar process. It stops short of
	// credentials. Provisioning the sidecar — publishing its key, relaying the
	// sealed bundle, starting its proxies on ProxyHost — is backend-neutral
	// protocol over Sidecar().Supervision(), and the caller runs it.
	//
	// On error nothing of the cell is left behind and the caller does not
	// Close a nil return.
	Provision(ctx context.Context, spec ProvisionSpec) (Cell, error)

	// ReapOrphans reclaims every cell a previous process on this host left
	// behind. Called once at boot, before any cell of this process exists, so
	// everything it finds belongs to an engagement that is gone. Per-cell
	// failures are logged and skipped; the error reports only a sweep that
	// could not run at all.
	ReapOrphans(ctx context.Context) error
}

// ProvisionSpec is one engagement's cell request.
type ProvisionSpec struct {
	// ConversationID is the conversation the engagement serves. It names the
	// cell's per-run resources and is never trusted for anything privileged.
	ConversationID string

	// Step, when non-nil, brackets each step of the bring-up: the backend
	// calls it with the step's name before the step and calls the returned
	// function after. It is how the caller's stall watchdog sees a bring-up
	// that is stuck inside the backend. The names are the backend's own.
	Step func(name string) (end func())
}

// Cell is one engagement's provisioned environment. Its methods are safe to
// call from the engagement's goroutines; Close is not concurrent with the
// others.
type Cell interface {
	// ProxyHost is the address the sidecar binds its proxies on. It is
	// reachable from the jail and from this process, so a host-side clone
	// before the jail exists routes through the same proxies the agent will.
	ProxyHost() string

	// Sidecar is the cell's credential sidecar process.
	Sidecar() Sidecar

	// HandOffRunTree gives the run tree rooted at root to this engagement's
	// jail. It is made at every launch, not only the first: a task's tree
	// outlives an engagement, and the engagement before this one may have
	// held it under another identity. An ending that has to capture a tree
	// no jail holds hands it off too. The grant is taken back by Close.
	HandOffRunTree(ctx context.Context, root string) error

	// Launch starts the engagement's jail with the resident tool host as its
	// main process. The jail sees the run tree at /work and TF's inputs at
	// the fixed in-jail paths env assembly assumes, however the backend
	// delivers them.
	//
	// The jail lives no longer than ctx: cancelling it kills the jail. A
	// caller passes the engagement's context, so a stopped engagement takes
	// its jail with it.
	Launch(ctx context.Context, spec LaunchSpec) (Jail, error)

	// Close reclaims the cell: it kills the sidecar process and only then
	// releases every identity the cell held — addresses, uids, file grants —
	// so none of them reaches another engagement while anything of this one
	// still carries it. The caller stops and closes the jail, and shuts the
	// sidecar's protocol down (relay, audit drain), before calling it: a jail
	// that is closed but not yet stopped may still hold the identity Close is
	// about to release. Idempotent.
	Close() error
}

// Sidecar is a cell's live credential-sidecar process.
type Sidecar interface {
	// Supervision is this process's end of the duplex control stream to the
	// sidecar. Valid until Close.
	Supervision() net.Conn

	// Close kills the sidecar and returns once it has been reaped.
	// Idempotent.
	Close() error
}

// LaunchSpec is one jail launch. Inputs are named by their host source; the
// backend decides how each reaches its fixed in-jail path.
type LaunchSpec struct {
	// WorkspaceKey is the key the run tree is built under (the task id). A
	// cold rehydrate rebuilds under the same one.
	WorkspaceKey string

	// RunTree is the host path of the run tree, presented in the jail at
	// /work and handed off to the jail before it starts.
	RunTree string

	// Env is the engagement's host-path-bearing env. The backend translates
	// its host paths to in-jail paths, the same translation every launch
	// gets.
	Env []string

	// ProxyEnv is the non-secret env the sidecar's bring-up produced: proxy
	// coordinates and per-run placeholders, appended verbatim.
	ProxyEnv []string

	// SidecarEndpoints are the "host:port" sidecar listeners ProxyEnv and the
	// gh channel point the jail at, each on ProxyHost. They are the only host
	// addresses the jail can reach: every other listener on the host, the
	// sidecar's own included, stays closed to it.
	SidecarEndpoints []string

	// AgentHostSocket is the host path of the exec-verb socket the sidecar
	// serves. Required: without it every exec verb inside the jail is
	// stranded.
	AgentHostSocket string

	// CABundle is the host path of the CA bundle the sidecar wrote: the
	// trust file every TLS client in the jail reads, carrying the egress
	// interceptor's CA. Required: without it no registry the egress proxy
	// admits verifies in the jail.
	CABundle string

	// GHChannel wires the real-gh credential injector when the sidecar bound
	// one; nil otherwise.
	GHChannel *GHChannel

	// SkillsSource and MemorySource are this launch's staging directories for
	// a blueprint step's skill and the materialized entity memory, presented
	// read-only. Empty when the launch has none. A source that has vanished
	// is skipped with a warning rather than failing the launch.
	SkillsSource string
	MemorySource string

	// MemoryLimitMB caps the jail. Zero uses the executor's default.
	MemoryLimitMB int

	// OrgID and ClaimID name the engagement the jail belongs to, for
	// resource sampling and the actuals stamp. Empty leaves both
	// unattributed, which is the test-harness shape.
	OrgID   string
	ClaimID string

	// RecordActuals persists what the jail cost once it has exited. Called
	// from Jail.Close, best-effort.
	RecordActuals func(ctx context.Context, orgID, claimID string, actuals Actuals) error
}

// GHChannel is the real-gh injector the sidecar bound for this engagement.
// The jail's gh verifies it through the CA bundle, which carries its leaf.
type GHChannel struct {
	// Host is the injector's "host:port" (no scheme), the agent's GH_HOST.
	Host string
	// Token is the per-run placeholder the agent's gh presents. The injector
	// strips it and attaches the real token.
	Token string
}

// Jail is a launched tool-host jail.
type Jail interface {
	// Connect returns the connection to the in-jail tool host, failing early
	// when the jail exits before the tool host is reachable rather than
	// waiting out timeout.
	Connect(ctx context.Context, timeout time.Duration) (net.Conn, error)

	// Stop kills the jail and returns once no process of it is running. An
	// ending that is about to read the run tree stops the jail first,
	// because a sent kill is not a quiet tree. Safe to call more than once
	// and before Close.
	Stop() error

	// Exited reports whether the jail's processes are gone.
	Exited() bool

	// Freeze holds every process in the jail where it stands until Thaw. A
	// jail that has already exited is left alone. A jail that cannot be
	// frozen is an error, never a quiet success: a capture that went ahead on
	// a jail it believed frozen would read a moving tree.
	Freeze(ctx context.Context) error

	// Thaw lets a frozen jail run again.
	Thaw(ctx context.Context) error

	// OOMKilled reports whether the jail's memory ceiling killed it.
	// Meaningful only once the jail has exited.
	OOMKilled() bool

	// Close kills the jail if it is still running, stamps its actuals and
	// reclaims what the launch created. It does not promise the jail's
	// processes are gone when it returns; Stop does. The cell outlives it.
	// Idempotent.
	Close() error
}

// Actuals is what one jail consumed, as its backend measured it. Both fields
// are pointers so "not measured" stays distinguishable from any measured
// value, a measured zero included; a recorded actual stays NULL rather than
// carrying an understated figure that would read as real.
type Actuals struct {
	// PeakMemMB is the jail's high-water memory use in MiB, rounded up. nil
	// when unmeasured.
	PeakMemMB *int
	// CPUUsec is cumulative CPU time in microseconds across the whole jail.
	// nil when unmeasured.
	CPUUsec *int64
}
