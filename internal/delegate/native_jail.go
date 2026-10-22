package delegate

import (
	"context"
	"fmt"
	"net"
	"sync"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/agentloop"
	"github.com/sky-ai-eng/triage-factory/internal/cell"
)

// restartableJail is a native engagement's tool-host jail, which the agent
// loop replaces in place when the one it talks to dies. Every jail is
// launched in the engagement's own cell from the same spec, so a new one has
// the same network, credentials, inputs and run tree as the first. Only what
// lived inside the jail that died, its processes and its /tmp, is gone.
//
// It is also the engagement's cell.Jail, standing for whichever jail is
// current, so the checkpointer's freeze, the live-jail samples and the
// ending's stop reach the jail that is running and not one that died.
type restartableJail struct {
	cell cell.Cell
	spec cell.LaunchSpec
	// newToolHost is the loop's tool host for a jail, made from the
	// connection to it.
	newToolHost func(jail cell.Jail, conn net.Conn) agentloop.ToolHost
	// record stamps the claim's actuals. Each jail stamps once when it is
	// closed, and the stamp replaces the last one, so every stamp carries
	// the engagement's jails so far: the highest peak, and their CPU added.
	record func(ctx context.Context, orgID, claimID string, actuals cell.Actuals) error

	mu    sync.Mutex
	jail  cell.Jail
	tools agentloop.ToolHost
	spent cell.Actuals
}

// launchRestartableJail launches the engagement's first jail and connects to
// its tool host. On error nothing of the jail is left running.
func launchRestartableJail(ctx context.Context, c cell.Cell, spec cell.LaunchSpec, newToolHost func(cell.Jail, net.Conn) agentloop.ToolHost) (*restartableJail, error) {
	j := &restartableJail{cell: c, newToolHost: newToolHost, record: spec.RecordActuals}
	spec.RecordActuals = j.recordActuals
	j.spec = spec
	jail, tools, err := j.launch(ctx)
	if err != nil {
		return nil, err
	}
	j.jail, j.tools = jail, tools
	return j, nil
}

// launch starts one jail from the engagement's spec and connects to its tool
// host.
func (j *restartableJail) launch(ctx context.Context) (cell.Jail, agentloop.ToolHost, error) {
	jail, err := j.cell.Launch(ctx, j.spec)
	if err != nil {
		return nil, nil, fmt.Errorf("launch tool host: %w", err)
	}
	conn, err := jail.Connect(ctx, toolHostDialTimeout)
	if err != nil {
		_ = jail.Stop()
		_ = jail.Close()
		return nil, nil, fmt.Errorf("connect to tool host: %w", err)
	}
	return jail, j.newToolHost(jail, conn), nil
}

// restart replaces the current jail with a new one. The old jail is stopped
// and closed first: the new one reuses its container id and socket, and the
// old one's runtime has to be gone before the tree has a new writer. On error
// the old jail, now closed, stays current, and the engagement ends on it.
func (j *restartableJail) restart(ctx context.Context) error {
	j.mu.Lock()
	old, oldTools := j.jail, j.tools
	j.mu.Unlock()
	_ = oldTools.Close()
	if err := old.Stop(); err != nil {
		return fmt.Errorf("stop the jail that died: %w", err)
	}
	_ = old.Close()
	jail, tools, err := j.launch(ctx)
	if err != nil {
		return err
	}
	j.mu.Lock()
	j.jail, j.tools = jail, tools
	j.mu.Unlock()
	return nil
}

func (j *restartableJail) recordActuals(ctx context.Context, orgID, claimID string, a cell.Actuals) error {
	j.mu.Lock()
	if a.PeakMemMB != nil && (j.spent.PeakMemMB == nil || *a.PeakMemMB > *j.spent.PeakMemMB) {
		peak := *a.PeakMemMB
		j.spent.PeakMemMB = &peak
	}
	if a.CPUUsec != nil {
		cpu := *a.CPUUsec
		if j.spent.CPUUsec != nil {
			cpu += *j.spent.CPUUsec
		}
		j.spent.CPUUsec = &cpu
	}
	total := j.spent
	j.mu.Unlock()
	if j.record == nil {
		return nil
	}
	return j.record(ctx, orgID, claimID, total)
}

func (j *restartableJail) current() cell.Jail {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.jail
}

// toolHost is the loop's view of the jail: calls go to the current jail's
// tool host, and a restart replaces it.
func (j *restartableJail) toolHost() agentloop.RestartableToolHost { return jailToolHost{j} }

func (j *restartableJail) Connect(ctx context.Context, timeout time.Duration) (net.Conn, error) {
	return j.current().Connect(ctx, timeout)
}
func (j *restartableJail) Stop() error                      { return j.current().Stop() }
func (j *restartableJail) Exited() bool                     { return j.current().Exited() }
func (j *restartableJail) Freeze(ctx context.Context) error { return j.current().Freeze(ctx) }
func (j *restartableJail) Thaw(ctx context.Context) error   { return j.current().Thaw(ctx) }
func (j *restartableJail) OOMKilled() bool                  { return j.current().OOMKilled() }
func (j *restartableJail) OOMKills() (int, bool)            { return j.current().OOMKills() }
func (j *restartableJail) Sample() cell.Sample              { return j.current().Sample() }
func (j *restartableJail) Close() error                     { return j.current().Close() }

// jailToolHost is a restartableJail as the agent loop's tool host.
type jailToolHost struct{ j *restartableJail }

func (h jailToolHost) Call(name string, args map[string]any) (agentloop.ToolOutcome, error) {
	h.j.mu.Lock()
	tools := h.j.tools
	h.j.mu.Unlock()
	return tools.Call(name, args)
}

// Close closes the connection to the current tool host. The jail is the
// restartableJail's to close.
func (h jailToolHost) Close() error {
	h.j.mu.Lock()
	tools := h.j.tools
	h.j.mu.Unlock()
	return tools.Close()
}

func (h jailToolHost) Restart(ctx context.Context) error { return h.j.restart(ctx) }
