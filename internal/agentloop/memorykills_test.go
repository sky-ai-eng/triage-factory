package agentloop

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// fakeJail is a JailMemory whose kill count a test moves by hand, usually from
// inside a tool call to stand for a kill that landed while it ran.
type fakeJail struct {
	kills     int
	unread    bool
	exited    bool
	oomKilled bool
}

func (j *fakeJail) OOMKills() (int, bool) {
	if j.unread || j.exited {
		return 0, false
	}
	return j.kills, true
}
func (j *fakeJail) Exited() bool    { return j.exited }
func (j *fakeJail) OOMKilled() bool { return j.exited && j.oomKilled }

// funcToolHost answers every call through call.
type funcToolHost struct {
	call func(name string, args map[string]any) (ToolOutcome, error)
}

func (h funcToolHost) Call(name string, args map[string]any) (ToolOutcome, error) {
	return h.call(name, args)
}
func (funcToolHost) Close() error { return nil }

func watch(jail *fakeJail, limitMB int, call func(name string, args map[string]any) (ToolOutcome, error)) *memoryKillWatch {
	w := WatchMemoryKills(funcToolHost{call: call}, jail, limitMB, nil).(*memoryKillWatch)
	w.exitWait = 50 * time.Millisecond
	return w
}

const killedOutput = "partial\n\n\nCommand was killed by SIGKILL (signal 9)"

// tally is a count func that adds every kill it is told to a map by kind.
func tally() (map[MemoryKill]int, func(MemoryKill, int)) {
	got := map[MemoryKill]int{}
	return got, func(kind MemoryKill, n int) { got[kind] += n }
}

// A call during which nothing was killed comes back exactly as the host
// answered it, in every form a result takes.
func TestWatchMemoryKills_NothingKilledChangesNothing(t *testing.T) {
	for name, out := range map[string]ToolOutcome{
		"content":    {Content: "hello"},
		"tool error": {ToolError: "(no output)\n\nCommand exited with code 1"},
		"images":     {Content: "Read image file", Images: []ToolImage{{Data: "AAAA", MimeType: "image/png"}}},
		"protocol":   {Protocol: &ProtocolError{Kind: protoUnknownTool, Message: "unknown tool: x"}},
	} {
		t.Run(name, func(t *testing.T) {
			w := watch(&fakeJail{}, 4096, func(string, map[string]any) (ToolOutcome, error) { return out, nil })
			got, err := w.Call("bash", nil)
			if err != nil {
				t.Fatalf("Call: %v", err)
			}
			if got.Content != out.Content || got.ToolError != out.ToolError || len(got.Images) != len(out.Images) {
				t.Errorf("result changed: got %+v, want %+v", got, out)
			}
			if (got.Protocol == nil) != (out.Protocol == nil) || (got.Protocol != nil && *got.Protocol != *out.Protocol) {
				t.Errorf("protocol error changed: got %+v, want %+v", got.Protocol, out.Protocol)
			}
		})
	}
}

// The case the harness alone cannot explain: a command killed at the ceiling
// reads as a SIGKILL, and only the count says the ceiling sent it.
func TestWatchMemoryKills_AKillDuringACommandIsExplainedOnItsResult(t *testing.T) {
	jail := &fakeJail{}
	w := watch(jail, 4096, func(string, map[string]any) (ToolOutcome, error) {
		jail.kills++
		return ToolOutcome{ToolError: killedOutput}, nil
	})
	got, err := w.Call("bash", nil)
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	if !strings.HasPrefix(got.ToolError, killedOutput+"\n\n") {
		t.Fatalf("the notice must follow the command's own output: %q", got.ToolError)
	}
	notice := strings.TrimPrefix(got.ToolError, killedOutput+"\n\n")
	for _, want := range []string{
		"reached its 4096 MB memory limit while this command ran",
		"killed a process in it",
		"most likely this command or one of its children",
		"The sandbox is still running.",
		"split the work into smaller steps.",
	} {
		if !strings.Contains(notice, want) {
			t.Errorf("notice lacks %q: %q", want, notice)
		}
	}
	if strings.Contains(notice, "no command of yours was running") {
		t.Errorf("a kill during the command is not a background kill: %q", notice)
	}
}

// Each kill is reported once, and a kill that happened before the engagement
// began is not reported at all.
func TestWatchMemoryKills_EachKillIsReportedOnce(t *testing.T) {
	jail := &fakeJail{kills: 3}
	killNow := false
	w := watch(jail, 4096, func(string, map[string]any) (ToolOutcome, error) {
		if killNow {
			jail.kills += 2
		}
		return ToolOutcome{Content: "ok"}, nil
	})
	if got, _ := w.Call("bash", nil); got.Content != "ok" {
		t.Fatalf("kills before the engagement must not be reported: %q", got.Content)
	}
	killNow = true
	got, _ := w.Call("bash", nil)
	if !strings.Contains(got.Content, "killed 2 processes in it") {
		t.Fatalf("two kills during the call must be counted as two: %q", got.Content)
	}
	killNow = false
	if got, _ := w.Call("bash", nil); got.Content != "ok" {
		t.Fatalf("a kill already reported must not be reported again: %q", got.Content)
	}
}

// A kill between calls, or during a call that starts no process, was a
// process the agent left running in the background.
func TestWatchMemoryKills_ABackgroundKillIsToldAsOne(t *testing.T) {
	t.Run("between calls", func(t *testing.T) {
		jail := &fakeJail{}
		w := watch(jail, 4096, func(string, map[string]any) (ToolOutcome, error) { return ToolOutcome{Content: "ok"}, nil })
		_, _ = w.Call("bash", nil)
		jail.kills++ // while the model was thinking
		got, _ := w.Call("bash", nil)
		if !strings.Contains(got.Content, "while no command of yours was running in the foreground") ||
			!strings.Contains(got.Content, "a process you left running in the background") {
			t.Fatalf("a kill between calls must be told as a background kill: %q", got.Content)
		}
		if strings.Contains(got.Content, "while this command ran") {
			t.Fatalf("a kill between calls is not this command's: %q", got.Content)
		}
	})
	t.Run("during a call that starts no process", func(t *testing.T) {
		jail := &fakeJail{}
		w := watch(jail, 4096, func(string, map[string]any) (ToolOutcome, error) {
			jail.kills++
			return ToolOutcome{Content: "file contents"}, nil
		})
		got, _ := w.Call("read", nil)
		if !strings.HasPrefix(got.Content, "file contents\n\n") ||
			!strings.Contains(got.Content, "a process you left running in the background") {
			t.Fatalf("a kill during a read was not the read's: %q", got.Content)
		}
	})
	t.Run("both, on one command", func(t *testing.T) {
		jail := &fakeJail{}
		w := watch(jail, 4096, func(string, map[string]any) (ToolOutcome, error) {
			jail.kills++
			return ToolOutcome{ToolError: killedOutput}, nil
		})
		jail.kills++ // before the call
		got, _ := w.Call("bash", nil)
		if !strings.Contains(got.ToolError, "while this command ran, and the kernel killed a process") ||
			!strings.Contains(got.ToolError, "in the foreground, and the kernel killed a process") {
			t.Fatalf("each kill must be told by when it happened: %q", got.ToolError)
		}
	})
}

// Every kill the watch sees is counted once, under what memory ended: the
// budget's command, the ceiling's processes, or the jail. A budget kill keeps
// the harness's own error as it is, since that error already tells the model.
func TestWatchMemoryKills_CountsEveryKillOnceByKind(t *testing.T) {
	jail := &fakeJail{}
	var answer func() (ToolOutcome, error)
	w := watch(jail, 4096, func(string, map[string]any) (ToolOutcome, error) { return answer() })
	got, count := tally()
	w.count = count

	const budgetError = "Command killed: exceeded the per-command memory budget (~3100 MB resident against a 3072 MB budget)."
	answer = func() (ToolOutcome, error) {
		return ToolOutcome{ToolError: budgetError, Cause: ToolErrorCauseMemoryBudget}, nil
	}
	if out, err := w.Call("bash", nil); err != nil || out.ToolError != budgetError {
		t.Fatalf("budget kill = (%+v, %v), want the harness's error unchanged", out, err)
	}

	answer = func() (ToolOutcome, error) { jail.kills++; return ToolOutcome{ToolError: killedOutput}, nil }
	_, _ = w.Call("bash", nil)

	answer = func() (ToolOutcome, error) { return ToolOutcome{Content: "ok"}, nil }
	jail.kills += 2 // in the background, between calls
	_, _ = w.Call("read", nil)
	_, _ = w.Call("read", nil) // nothing new

	answer = func() (ToolOutcome, error) { jail.kills++; return ToolOutcome{}, io.EOF }
	if _, err := w.Call("bash", nil); err == nil {
		t.Fatal("a call whose tool host died returned no error")
	}

	want := map[MemoryKill]int{MemoryKillBashBudget: 1, MemoryKillCeiling: 3, MemoryKillJailLost: 1}
	if len(got) != len(want) {
		t.Errorf("counted %v, want %v", got, want)
	}
	for kind, n := range want {
		if got[kind] != n {
			t.Errorf("%s = %d, want %d (all: %v)", kind, got[kind], n, got)
		}
	}
}

// A jail with no ceiling of its own can still lose a process to the host
// running out of memory, and the notice must not invent a limit.
func TestWatchMemoryKills_AnUncappedJailNamesNoLimit(t *testing.T) {
	jail := &fakeJail{}
	w := watch(jail, 0, func(string, map[string]any) (ToolOutcome, error) {
		jail.kills++
		return ToolOutcome{ToolError: killedOutput}, nil
	})
	got, _ := w.Call("bash", nil)
	if !strings.Contains(got.ToolError, "reached the memory available to it") || strings.Contains(got.ToolError, " MB ") {
		t.Fatalf("an uncapped jail's notice names no limit: %q", got.ToolError)
	}
}

// A count that cannot be read is no reading: nothing is claimed, and nothing
// is lost either, because the next readable count still carries the kill.
func TestWatchMemoryKills_AnUnreadableCountClaimsNothing(t *testing.T) {
	jail := &fakeJail{}
	w := watch(jail, 4096, func(string, map[string]any) (ToolOutcome, error) {
		jail.kills++
		jail.unread = true
		return ToolOutcome{ToolError: killedOutput}, nil
	})
	if got, _ := w.Call("bash", nil); got.ToolError != killedOutput {
		t.Fatalf("an unreadable count must leave the result alone: %q", got.ToolError)
	}
}

// A dead tool host whose jail the ceiling ended is explained on the call that
// was running. Every way of learning it is covered, and so is every way of
// not being able to. Which kills explain it depends on whether the request
// left: one that never did found the jail already gone.
func TestWatchMemoryKills_ExplainsAJailTheCeilingEnded(t *testing.T) {
	transport := io.ErrUnexpectedEOF
	notSent := fmt.Errorf("agentloop: write tool request: %w: %w", errRequestNotSent, io.ErrClosedPipe)
	cases := []struct {
		name string
		// tool is the call that was running, bash unless named.
		tool string
		// err is what the call returned, transport unless set.
		err error
		// before sets the jail up ahead of the call; during is what the
		// call does to it.
		before func(j *fakeJail)
		during func(j *fakeJail)
		// notice is how the explanation opens; empty means unexplained.
		notice string
	}{
		{
			name:   "the live count rose",
			during: func(j *fakeJail) { j.kills++ },
			notice: "This command ran, and while it ran the sandbox reached its 4096 MB memory limit.",
		},
		{
			name:   "the jail has exited and recorded its first kill",
			during: func(j *fakeJail) { j.kills++; j.exited = true; j.oomKilled = true },
			notice: "This command ran",
		},
		{
			name:   "a call that starts no process",
			tool:   "read",
			during: func(j *fakeJail) { j.kills++ },
			notice: "While this tool call ran, the sandbox reached its 4096 MB memory limit, most likely because of a process you left running in the background",
		},
		{
			name:   "the jail has exited, but a kill was already counted before the call",
			before: func(j *fakeJail) { j.kills = 1 },
			during: func(j *fakeJail) { j.exited = true; j.oomKilled = true },
		},
		{
			name:   "the jail is still running and nothing was killed",
			during: func(j *fakeJail) {},
		},
		{
			name:   "the jail exited for another reason",
			during: func(j *fakeJail) { j.exited = true },
		},
		{
			name:   "a request that never left, after a background kill",
			err:    notSent,
			before: func(j *fakeJail) { j.kills = 1 },
			during: func(j *fakeJail) {},
			notice: "This tool call was not executed: before it was sent, the sandbox reached its 4096 MB memory limit",
		},
		{
			name:   "a request that never left, to a jail that exited and recorded its first kill",
			err:    notSent,
			during: func(j *fakeJail) { j.exited = true; j.oomKilled = true },
			notice: "This tool call was not executed",
		},
		{
			name:   "a request that never left, with nothing killed",
			err:    notSent,
			during: func(j *fakeJail) {},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tool, callErr := tc.tool, tc.err
			if tool == "" {
				tool = "bash"
			}
			if callErr == nil {
				callErr = transport
			}
			jail := &fakeJail{}
			w := watch(jail, 4096, func(string, map[string]any) (ToolOutcome, error) {
				tc.during(jail)
				return ToolOutcome{}, callErr
			})
			counted, count := tally()
			w.count = count
			if tc.before != nil {
				tc.before(jail)
			}
			_, err := w.Call(tool, nil)
			if !errors.Is(err, callErr) {
				t.Fatalf("the call's failure must stay in the chain: %v", err)
			}
			lost, ok := lostToolHost(err)
			if explained := tc.notice != ""; ok != explained {
				t.Fatalf("explained = %v, want %v (err: %v)", ok, explained, err)
			}
			wantLost := 0
			if ok {
				wantLost = 1
			}
			if counted[MemoryKillJailLost] != wantLost || counted[MemoryKillCeiling] != 0 {
				t.Errorf("counted %v, want jail_lost %d and no ceiling kills", counted, wantLost)
			}
			if !ok {
				return
			}
			if !strings.HasPrefix(lost.Notice, tc.notice) {
				t.Errorf("notice = %q, want it to open %q", lost.Notice, tc.notice)
			}
			for _, want := range []string{"ended the sandbox", memoryRemedies} {
				if !strings.Contains(lost.Notice, want) {
					t.Errorf("notice lacks %q: %q", want, lost.Notice)
				}
			}
			if want := "the sandbox reached its 4096 MB memory limit and the kernel ended it"; lost.Reason != want {
				t.Errorf("Reason = %q, want %q", lost.Reason, want)
			}
			if !lost.MemoryLimit {
				t.Error("MemoryLimit = false for a jail the ceiling ended")
			}
		})
	}
}

// The engine side of a jail the ceiling ended: the call it died in gets the
// explanation, then where the conversation goes on. Restarted in place, the
// loop carries on; with no restart, the calls after it keep the
// never-executed notice and the engagement ends with its tool host lost.
func TestRun_AJailTheCeilingEndedIsExplainedOnTheCallItEnded(t *testing.T) {
	for _, restartable := range []bool{true, false} {
		name := "not restartable"
		if restartable {
			name = "restarted"
		}
		t.Run(name, func(t *testing.T) {
			tr := newMemTranscript(pendingUser("go"))
			jail := &fakeJail{}
			killed := false
			var host = WatchMemoryKills(funcToolHost{call: func(name string, _ map[string]any) (ToolOutcome, error) {
				if name != "bash" || killed {
					return ToolOutcome{Content: "ok:" + name}, nil
				}
				killed = true
				jail.kills++
				jail.exited = true
				jail.oomKilled = true
				return ToolOutcome{}, io.EOF
			}}, jail, 4096, nil)
			turns := []scriptedTurn{{calls: []domain.ToolCall{{ID: "c1", Name: "bash"}, {ID: "c2", Name: "read"}}}}
			if restartable {
				host = restartingWatch{ToolHost: host}
				turns = append(turns, scriptedTurn{text: "done"})
			}
			got := newTestEngine(tr, &scriptedProvider{turns: turns}, host).Run(context.Background(), testParams())
			results := tr.toolResults()
			if len(results) != 2 {
				t.Fatalf("both calls need results, got %+v", results)
			}
			if !strings.HasPrefix(results[0].Content, "This command ran") {
				t.Errorf("the call that ran must be told it ran: %q", results[0].Content)
			}
			if restartable {
				if got.Kind != ResultConcluded {
					t.Fatalf("disposition = %v, want concluded (err: %v)", got.Kind, got.Err)
				}
				if !strings.HasSuffix(results[0].Content, restartedSentence) || results[1].Content != restartedRemainingNotice {
					t.Errorf("results = %q / %q, want the restart named on both", results[0].Content, results[1].Content)
				}
				return
			}
			if got.Kind != ResultToolHostLost {
				t.Fatalf("disposition = %v, want tool host lost (err: %v)", got.Kind, got.Err)
			}
			if lost, ok := lostToolHost(got.Err); !ok || !lost.MemoryLimit || !strings.Contains(lost.Reason, "4096 MB memory limit") {
				t.Errorf("Err = %+v, want the lost tool host naming the limit", got.Err)
			}
			if !strings.HasSuffix(results[0].Content, newSandboxSentence) || results[1].Content != fatalToolHostNotice {
				t.Errorf("results = %q / %q, want the next claim named on the first and the never-executed notice on the second", results[0].Content, results[1].Content)
			}
		})
	}
}

// restartingWatch makes a memory watch restartable, the way the delegate's
// jail is: in production a restart puts a new watch over a new jail, and
// here the watch's host simply stops dying.
type restartingWatch struct{ ToolHost }

func (restartingWatch) Restart(context.Context) error { return nil }
