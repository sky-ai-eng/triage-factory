package agentloop

import (
	"errors"
	"fmt"
	"sync"
	"time"
)

// JailMemory is what the loop reads about the memory ceiling of the jail its
// tools run in. cell.Jail satisfies it.
type JailMemory interface {
	// OOMKills is the count of the jail's processes killed for memory so
	// far, readable while the jail runs.
	OOMKills() (n int, ok bool)
	// Exited reports whether the jail's processes are gone.
	Exited() bool
	// OOMKilled reports, once the jail has exited, whether any process of
	// it was ever killed for memory.
	OOMKilled() bool
}

// jailExitWait bounds how long a dead tool host waits for its jail to finish
// exiting before reading the jail's exit attribution. The tool host is the
// jail's main process, so a jail whose tool host died follows it within a
// moment; a jail still running past this was not ended by its ceiling.
const jailExitWait = 5 * time.Second

// WatchMemoryKills wraps host so a call's result says when the jail's memory
// ceiling killed something while the call ran or since the previous call.
// limitMB is the ceiling the jail was launched under, zero for none.
//
// The kernel holds a jail at its ceiling by killing the host process with the
// most memory in it, usually the one running the largest guest process. The
// agent sees only the effect: a command that died of SIGKILL or exited 137, or
// a background server that stopped answering. The kill count read on either
// side of a call is what attributes the kill to the ceiling and to the call.
//
// When the kill took the tool host's jail with it, the call returns a
// ToolHostLostError whose notice says the ceiling ended the jail.
//
// A call during which nothing was killed passes through unchanged.
func WatchMemoryKills(host ToolHost, jail JailMemory, limitMB int) ToolHost {
	w := &memoryKillWatch{host: host, jail: jail, limitMB: limitMB, exitWait: jailExitWait}
	if n, ok := jail.OOMKills(); ok {
		w.told = n
	}
	return w
}

type memoryKillWatch struct {
	host     ToolHost
	jail     JailMemory
	limitMB  int
	exitWait time.Duration

	// mu spans a whole call, both count reads included, so the kills read on
	// either side of a call happened during that call and no other.
	mu sync.Mutex
	// told is the count every notice so far accounts for: kills at or
	// below it have been reported, or happened before the engagement began.
	told int
}

func (w *memoryKillWatch) Call(name string, args map[string]any) (ToolOutcome, error) {
	w.mu.Lock()
	defer w.mu.Unlock()

	before, beforeOK := w.jail.OOMKills()
	out, err := w.host.Call(name, args)
	if err != nil {
		return out, w.explainLostHost(name, before, beforeOK, err)
	}
	after, afterOK := w.jail.OOMKills()
	if !afterOK {
		return out, nil
	}

	// Kills counted before the call began happened while nothing the agent
	// ran in the foreground was running. Kills during a call that starts no
	// process (every tool but bash) cannot have been the call's own.
	start := w.told
	if beforeOK && before > start {
		start = before
	}
	background := start - w.told
	during := after - start
	if during < 0 {
		during = 0
	}
	if name != bashToolName {
		background += during
		during = 0
	}
	if after > w.told {
		w.told = after
	}

	var notices []string
	if during > 0 {
		notices = append(notices, w.duringNotice(during))
	}
	if background > 0 {
		notices = append(notices, w.backgroundNotice(background))
	}
	for _, n := range notices {
		out = appendNotice(out, n)
	}
	return out, nil
}

func (w *memoryKillWatch) Close() error { return w.host.Close() }

// explainLostHost returns err as a ToolHostLostError when the memory ceiling
// ended the jail, and err unchanged otherwise.
//
// A live count that rose past the kills already accounted for is the answer,
// when the jail is still there to read. Which kills those are depends on
// whether the request was sent. If it was, the tool host was alive when the
// call began, so a kill counted before it is one the jail survived. If it was
// not, the jail was already gone, and a kill since the last notice is the one
// that ended it. Once the jail has exited its count is gone, and OOMKilled
// stands in, but only while no kill had been accounted for: it reports any
// kill in the jail's life, so after one the jail survived it cannot say what
// ended the jail.
func (w *memoryKillWatch) explainLostHost(name string, before int, beforeOK bool, err error) error {
	sent := !errors.Is(err, errRequestNotSent)
	start := w.told
	if sent && beforeOK && before > start {
		start = before
	}
	ended := false
	if n, ok := w.jail.OOMKills(); ok {
		ended = n > start
	} else if start == 0 {
		deadline := time.Now().Add(w.exitWait)
		for !w.jail.Exited() && time.Now().Before(deadline) {
			time.Sleep(20 * time.Millisecond)
		}
		ended = w.jail.Exited() && w.jail.OOMKilled()
	}
	if !ended {
		return err
	}
	return &ToolHostLostError{
		Notice:      w.lostNotice(name, sent),
		Reason:      "the sandbox reached " + w.limitPhrase() + " and the kernel ended it",
		MemoryLimit: true,
		Err:         err,
	}
}

// bashToolName is the one tool that starts processes, so the only one whose
// call a kill can be the call's own.
const bashToolName = "bash"

// memoryRemedies is the advice every memory notice ends with: the harness's
// per-command budget error gives the same.
const memoryRemedies = "use less memory: lower build parallelism, stream instead of loading whole files, " +
	"or split the work into smaller steps."

func (w *memoryKillWatch) limitPhrase() string {
	if w.limitMB <= 0 {
		return "the memory available to it"
	}
	return fmt.Sprintf("its %d MB memory limit", w.limitMB)
}

func processes(n int) string {
	if n == 1 {
		return "a process"
	}
	return fmt.Sprintf("%d processes", n)
}

func (w *memoryKillWatch) duringNotice(n int) string {
	return fmt.Sprintf("The sandbox reached %s while this command ran, and the kernel killed %s in it to stay under the limit. "+
		"The killed process was most likely this command or one of its children: a command killed this way reports SIGKILL (signal 9) "+
		"or exit code 137. Otherwise it was a process running in the background. The sandbox is still running. To retry, %s",
		w.limitPhrase(), processes(n), memoryRemedies)
}

func (w *memoryKillWatch) backgroundNotice(n int) string {
	return fmt.Sprintf("The sandbox reached %s while no command of yours was running in the foreground, and the kernel killed %s in it "+
		"to stay under the limit: a process you left running in the background. Check that the processes you started in the background "+
		"are still running before you rely on them. To keep them running, %s",
		w.limitPhrase(), processes(n), memoryRemedies)
}

// lostNotice answers the call that was running when the ceiling ended the
// jail. A command can be what grew past the ceiling; any other call starts no
// process, so the one that did was running in the background, and so was
// whatever ended a jail before a call was even sent.
func (w *memoryKillWatch) lostNotice(name string, sent bool) string {
	switch {
	case !sent:
		return fmt.Sprintf("This tool call was not executed: before it was sent, the sandbox reached %s while no command of yours "+
			"was running in the foreground, most likely because of a process you left running in the background, and the kernel "+
			"ended the sandbox to stay under the limit. To keep processes running in the background, %s",
			w.limitPhrase(), memoryRemedies)
	case name == bashToolName:
		return fmt.Sprintf("This command ran, and while it ran the sandbox reached %s. The kernel ended the sandbox to stay under "+
			"the limit, so the command's result is lost. Before you run it again, %s",
			w.limitPhrase(), memoryRemedies)
	default:
		return fmt.Sprintf("While this tool call ran, the sandbox reached %s, most likely because of a process you left running "+
			"in the background, and the kernel ended the sandbox to stay under the limit, so the call's result is lost. "+
			"To keep processes running in the background, %s",
			w.limitPhrase(), memoryRemedies)
	}
}

// appendNotice adds a notice after a result's own text, whichever form the
// result takes.
func appendNotice(out ToolOutcome, notice string) ToolOutcome {
	join := func(text string) string {
		if text == "" {
			return notice
		}
		return text + "\n\n" + notice
	}
	switch {
	case out.Protocol != nil:
		p := *out.Protocol
		p.Message = join(p.Message)
		out.Protocol = &p
	case out.ToolError != "":
		out.ToolError = join(out.ToolError)
	default:
		out.Content = join(out.Content)
	}
	return out
}
