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
	// calls numbers the calls made to the tool host, and inFlight is the one
	// running, zero between calls, begun at inFlightSince. overran is the
	// call endOverrunCall ended, and overrunLimit the deadline it ran past.
	calls, inFlight, overran uint64
	inFlightSince            time.Time
	overrunLimit             time.Duration
	// restarting is set while restart runs.
	restarting bool
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
	j.restarting = true
	j.mu.Unlock()
	defer func() {
		j.mu.Lock()
		j.restarting = false
		j.mu.Unlock()
	}()
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

// endOverrunCall ends the tool call that the watchdog found at seen to have
// run past limit, by stopping the jail it runs in. The call then fails on its
// socket like any call whose jail died, and the loop restarts the jail; the
// call is told why it ended.
//
// The call may have returned since the watchdog looked. Then nothing is in
// flight, or the call in flight began after seen, and the jail is left alone.
// handled is false only when a restart is what overran: a restart that is
// itself stuck has no call to end, and stopping the engagement is all that is
// left. stopped reports whether the jail was stopped.
func (j *restartableJail) endOverrunCall(limit time.Duration, seen time.Time) (handled, stopped bool) {
	j.mu.Lock()
	if j.restarting {
		j.mu.Unlock()
		return false, false
	}
	if j.inFlight == 0 || j.inFlightSince.After(seen) {
		j.mu.Unlock()
		return true, false
	}
	j.overran, j.overrunLimit = j.inFlight, limit
	jail := j.jail
	j.mu.Unlock()
	if err := jail.Stop(); err != nil {
		delegateLog.Warn("stopping the jail to end a tool call past its deadline failed", "error", err)
	}
	return true, true
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
	h.j.calls++
	call := h.j.calls
	h.j.inFlight, h.j.inFlightSince = call, time.Now()
	tools := h.j.tools
	h.j.mu.Unlock()
	out, err := tools.Call(name, args)
	h.j.mu.Lock()
	h.j.inFlight = 0
	overran, limit := h.j.overran == call, h.j.overrunLimit
	h.j.mu.Unlock()
	// A call that answered before the stop reached it keeps its answer, and
	// the next call finds the jail gone, as it would any jail that died.
	if err != nil && overran {
		return out, agentloop.ToolCallOverranError(limit, err)
	}
	return out, err
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
