// Package gvisor is the gVisor implementation of the cell contract
// (internal/cell): a per-run netns and veth with a fail-closed egress policy,
// a capless credential sidecar at the run's own uid, and the tool host as the
// main process of a runsc jail. Every privileged step goes through the
// cap-broker by way of internal/sandbox; this package is the seam the
// executor reaches those steps through, not a second implementation of them.
package gvisor

import (
	"context"
	"fmt"
	"sync"

	"github.com/sky-ai-eng/triage-factory/internal/cell"
	"github.com/sky-ai-eng/triage-factory/internal/sandbox"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/codes"
)

var tracer = otel.Tracer("internal/cell/gvisor")

// Backend builds gVisor cells. Stateless: the subnet allocator, the uid bands
// and the broker connection it relies on are process-wide in internal/sandbox.
type Backend struct{}

// New returns the gVisor backend.
func New() *Backend { return &Backend{} }

var _ cell.Backend = (*Backend)(nil)

// Name implements cell.Backend.
func (*Backend) Name() string { return "gvisor" }

// Provision implements cell.Backend. Its three steps — the run network, the
// run's socket directories, the sidecar process — are each one broker round
// trip, reported to spec.Step as sidecar_network, sidecar_sockets and
// sidecar_launch.
func (*Backend) Provision(ctx context.Context, spec cell.ProvisionSpec) (cell.Cell, error) {
	step := spec.Step
	if step == nil {
		step = func(string) func() { return func() {} }
	}

	var net *sandbox.RunNetwork
	err := traced(ctx, "sandbox.network.setup", step("sidecar_network"), func(ctx context.Context) error {
		var err error
		net, err = sandbox.SetupRunNetwork(ctx, spec.ConversationID)
		return err
	})
	if err != nil {
		return nil, fmt.Errorf("set up run network: %w", err)
	}

	// The run's socket directories, made for this engagement's index before
	// its sidecar exists: the sidecar can create its socket and cert nowhere
	// else. Making them replaces whatever an earlier engagement of this
	// conversation left at the same paths, which is what lets a re-claim that
	// races the previous cell's teardown come up instead of failing.
	err = traced(ctx, "sandbox.sockets.prepare", step("sidecar_sockets"), func(ctx context.Context) error {
		return net.PrepareSockets(ctx, spec.ConversationID)
	})
	if err != nil {
		_ = net.Close()
		return nil, fmt.Errorf("prepare run socket directories: %w", err)
	}

	var sc sandbox.LaunchedSidecar
	err = traced(ctx, "sandbox.sidecar.launch", step("sidecar_launch"), func(ctx context.Context) error {
		var err error
		sc, err = sandbox.LaunchSidecar(ctx, sandbox.SidecarConfig{ConversationID: spec.ConversationID, SubnetIdx: net.Idx})
		return err
	})
	if err != nil {
		_ = net.Close()
		return nil, fmt.Errorf("launch credential sidecar: %w", err)
	}

	return &gvisorCell{conversationID: spec.ConversationID, net: net, sidecar: sc}, nil
}

// ReapOrphans implements cell.Backend.
func (*Backend) ReapOrphans(ctx context.Context) error {
	return sandbox.ReapOrphans(ctx)
}

// traced runs fn under a span named name and the caller's step bracket, the
// pair every bring-up step gets.
func traced(ctx context.Context, name string, end func(), fn func(context.Context) error) error {
	ctx, span := tracer.Start(ctx, name)
	err := fn(ctx)
	end()
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
	}
	span.End()
	return err
}

// gvisorCell is one engagement's network plus its sidecar process. The
// network's subnet index is the cell's identity: it names the netns and veth,
// and derives both the sidecar's uid and the jail's.
type gvisorCell struct {
	conversationID string
	net            *sandbox.RunNetwork
	sidecar        sandbox.LaunchedSidecar
	closeOnce      sync.Once
}

var _ cell.Cell = (*gvisorCell)(nil)

// ProxyHost implements cell.Cell: the veth's host-side IP, which the host
// reaches directly and the jail reaches as its only route.
func (c *gvisorCell) ProxyHost() string { return c.net.HostIP }

// Sidecar implements cell.Cell.
func (c *gvisorCell) Sidecar() cell.Sidecar { return c.sidecar }

// HandOffRunTree implements cell.Cell. The grant is made through the network,
// which takes it back before the index can go to anyone else.
func (c *gvisorCell) HandOffRunTree(ctx context.Context, root string) error {
	return c.net.HandOffRunTree(ctx, root)
}

// Close implements cell.Cell: the sidecar is killed and reaped first, then the
// network takes back every grant made under the jail identity, tears down the
// veth the proxies were bound on, and only then releases the subnet index —
// the index is what lets another engagement have this cell's uids.
func (c *gvisorCell) Close() error {
	c.closeOnce.Do(func() {
		_ = c.sidecar.Close()
		_ = c.net.Close()
	})
	return nil
}
