package delegate

import (
	"context"
	"errors"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/agentloop"
	"github.com/sky-ai-eng/triage-factory/internal/cell"
)

// fakeJail is one launched jail of a fakeCell: it records what was done to
// it, and stamps its actuals on Close the way a real jail does.
type fakeJail struct {
	n       int
	spec    cell.LaunchSpec
	actuals cell.Actuals
	stopErr error

	mu      sync.Mutex
	stopped bool
	closed  bool
	frozen  bool
}

func (j *fakeJail) Connect(context.Context, time.Duration) (net.Conn, error) {
	a, b := net.Pipe()
	_ = b.Close()
	return a, nil
}
func (j *fakeJail) Stop() error {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.stopErr != nil {
		return j.stopErr
	}
	j.stopped = true
	return nil
}
func (j *fakeJail) Exited() bool {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.stopped
}
func (j *fakeJail) Freeze(context.Context) error {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.frozen = true
	return nil
}
func (j *fakeJail) Thaw(context.Context) error { return nil }
func (j *fakeJail) OOMKilled() bool            { return false }
func (j *fakeJail) Sample() cell.Sample {
	n := j.n
	return cell.Sample{MemCurrentMB: &n}
}
func (j *fakeJail) Close() error {
	j.mu.Lock()
	first := !j.closed
	j.closed = true
	j.mu.Unlock()
	if first && j.spec.RecordActuals != nil {
		return j.spec.RecordActuals(context.Background(), j.spec.OrgID, j.spec.ClaimID, j.actuals)
	}
	return nil
}

// fakeCell launches fakeJails, numbered from 1, each with the actuals the
// test queued for it. launchErr fails every launch after the first.
type fakeCell struct {
	actuals   []cell.Actuals
	launchErr error
	jails     []*fakeJail
}

func (*fakeCell) ProxyHost() string                            { return "" }
func (*fakeCell) Sidecar() cell.Sidecar                        { return nil }
func (*fakeCell) HandOffRunTree(context.Context, string) error { return nil }
func (*fakeCell) Close() error                                 { return nil }
func (c *fakeCell) Launch(_ context.Context, spec cell.LaunchSpec) (cell.Jail, error) {
	if len(c.jails) > 0 && c.launchErr != nil {
		return nil, c.launchErr
	}
	j := &fakeJail{n: len(c.jails) + 1, spec: spec}
	if len(c.actuals) > len(c.jails) {
		j.actuals = c.actuals[len(c.jails)]
	}
	c.jails = append(c.jails, j)
	return j, nil
}

// jailNamedToolHost answers every call with the number of the jail it is in.
type jailNamedToolHost struct {
	n      int
	closed *bool
}

func (h jailNamedToolHost) Call(string, map[string]any) (agentloop.ToolOutcome, error) {
	if *h.closed {
		return agentloop.ToolOutcome{}, errors.New("closed")
	}
	return agentloop.ToolOutcome{Content: string(rune('0' + h.n))}, nil
}
func (h jailNamedToolHost) Close() error { *h.closed = true; return nil }

func launchFake(t *testing.T, c *fakeCell, record func(context.Context, string, string, cell.Actuals) error) *restartableJail {
	t.Helper()
	j, err := launchRestartableJail(context.Background(), c, cell.LaunchSpec{RunTree: "/tree", OrgID: "org", ClaimID: "claim", RecordActuals: record},
		func(jail cell.Jail, _ net.Conn) agentloop.ToolHost {
			return jailNamedToolHost{n: jail.(*fakeJail).n, closed: new(bool)}
		})
	if err != nil {
		t.Fatalf("launchRestartableJail: %v", err)
	}
	return j
}

func intp(v int) *int     { return &v }
func i64p(v int64) *int64 { return &v }

// A restart puts a new jail behind everything that holds the engagement's
// jail: the old one is stopped and closed before the new one launches from
// the same spec, calls go to the new tool host, and the freeze, samples and
// stop reach the new jail.
func TestRestartableJail_ARestartReplacesTheJailBehindEverythingThatHoldsIt(t *testing.T) {
	c := &fakeCell{}
	j := launchFake(t, c, nil)
	tools := j.toolHost()
	if out, err := tools.Call("bash", nil); err != nil || out.Content != "1" {
		t.Fatalf("call before the restart = (%+v, %v), want jail 1", out, err)
	}

	if err := tools.Restart(context.Background()); err != nil {
		t.Fatalf("Restart: %v", err)
	}
	if len(c.jails) != 2 {
		t.Fatalf("jails launched = %d, want 2", len(c.jails))
	}
	old, next := c.jails[0], c.jails[1]
	if !old.stopped || !old.closed {
		t.Errorf("old jail = (stopped %v, closed %v), want both before the new one launched", old.stopped, old.closed)
	}
	if next.spec.RunTree != old.spec.RunTree || next.spec.ClaimID != old.spec.ClaimID {
		t.Errorf("new jail's spec = %+v, want the engagement's", next.spec)
	}
	if out, err := tools.Call("bash", nil); err != nil || out.Content != "2" {
		t.Errorf("call after the restart = (%+v, %v), want jail 2", out, err)
	}
	if err := j.Freeze(context.Background()); err != nil || !next.frozen || old.frozen {
		t.Errorf("freeze reached (old %v, new %v), want only the new jail", old.frozen, next.frozen)
	}
	if s := j.Sample(); s.MemCurrentMB == nil || *s.MemCurrentMB != 2 {
		t.Errorf("sample = %+v, want the new jail's", s)
	}
	if err := j.Stop(); err != nil || !next.stopped {
		t.Errorf("stop = %v, new jail stopped %v", err, next.stopped)
	}
}

// Each jail stamps the claim when it closes, and a stamp replaces the last,
// so every stamp carries the engagement's jails so far: the highest peak and
// the CPU of all of them.
func TestRestartableJail_TheClaimsActualsCoverEveryJail(t *testing.T) {
	c := &fakeCell{actuals: []cell.Actuals{
		{PeakMemMB: intp(300), CPUUsec: i64p(10)},
		{PeakMemMB: intp(200), CPUUsec: i64p(5)},
	}}
	var stamps []cell.Actuals
	j := launchFake(t, c, func(_ context.Context, _, _ string, a cell.Actuals) error {
		stamps = append(stamps, a)
		return nil
	})
	if err := j.toolHost().Restart(context.Background()); err != nil {
		t.Fatalf("Restart: %v", err)
	}
	_ = j.Close()
	if len(stamps) != 2 {
		t.Fatalf("stamps = %d, want one per jail", len(stamps))
	}
	last := stamps[1]
	if last.PeakMemMB == nil || *last.PeakMemMB != 300 || last.CPUUsec == nil || *last.CPUUsec != 15 {
		t.Errorf("last stamp = (peak %v, cpu %v), want (300, 15)", last.PeakMemMB, last.CPUUsec)
	}
}

// A restart that cannot launch, or cannot stop what is left of the old jail,
// fails, and the engagement ends on the old jail rather than on nothing.
func TestRestartableJail_AFailedRestartLeavesTheOldJailCurrent(t *testing.T) {
	t.Run("the launch fails", func(t *testing.T) {
		c := &fakeCell{launchErr: errors.New("no capacity")}
		j := launchFake(t, c, nil)
		if err := j.toolHost().Restart(context.Background()); err == nil {
			t.Fatal("Restart succeeded with a launch that failed")
		}
		if j.current() != c.jails[0] {
			t.Error("the current jail moved off the old one")
		}
	})
	t.Run("the old jail will not stop", func(t *testing.T) {
		c := &fakeCell{}
		j := launchFake(t, c, nil)
		c.jails[0].stopErr = errors.New("still running")
		if err := j.toolHost().Restart(context.Background()); err == nil {
			t.Fatal("Restart succeeded over a jail still running")
		}
		if len(c.jails) != 1 {
			t.Errorf("jails launched = %d, want no second one beside a jail still running", len(c.jails))
		}
	})
}
