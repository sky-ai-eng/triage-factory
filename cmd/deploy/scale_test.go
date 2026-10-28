package deploy

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

// scaleFixture is a deployment whose stack runs three executors, with
// stored executors as its count and each running executor's instance id
// readable from its state root. arrange, when not nil, changes the stack
// before it is applied.
func scaleFixture(t *testing.T, stored int, arrange func(s *stackFixture)) (*fakeHost, Bundle, string) {
	t.Helper()
	f, b, dir := initedFixture(t)
	st, _ := readState(dir)
	st.Executors = stored
	if err := writeState(dir, st); err != nil {
		t.Fatal(err)
	}
	s := healthyStack()
	s.containers = append(s.containers,
		fixtureContainer("triage-factory-executor-2", "running", 0, "healthy", "unless-stopped"),
		fixtureContainer("triage-factory-executor-3", "running", 0, "healthy", "unless-stopped"),
	)
	if arrange != nil {
		arrange(s)
	}
	s.apply(t, f, dir)
	delete(f.dockerReplies, "image inspect")
	for _, n := range []string{"1", "2", "3"} {
		f.dockerReplies["exec triage-factory-executor-"+n+" sh"] = dockerReply{stdout: "inst-" + n + "\n"}
	}
	f.dockerSeqs[fleetList] = []dockerReply{
		{stdout: fleetJSON("inst-1:none:1:1", "inst-2:until_restart:0:10", "inst-3:until_restart:1:10")},
		{stdout: fleetJSON("inst-1:none:1:1", "inst-2:until_restart:0:1", "inst-3:until_restart:0:1")},
	}
	return f, b, dir
}

// callIndex is the index in f.dockerCalls of the first call starting with
// prefix, or -1.
func callIndex(f *fakeHost, prefix string) int {
	for i, call := range f.dockerCalls {
		if strings.HasPrefix(strings.Join(call, " "), prefix) {
			return i
		}
	}
	return -1
}

// TestUp_ScaleDownDrainsFirst: scaling 3 executors to 1 drains the two
// highest-numbered ones, waits for their runs, and stops and removes them
// before compose runs, so compose has none left to remove undrained.
func TestUp_ScaleDownDrainsFirst(t *testing.T) {
	f, b, dir := scaleFixture(t, 3, nil)
	if code := upRun(t, f, b, dir, "--executors", "1"); code != 0 {
		t.Fatalf("exit = %d; want 0\n%s%s", code, f.out, f.errOut)
	}
	if got := drainCalls(f); strings.Join(got, ",") != "inst-3,inst-2" {
		t.Errorf("drained %q; want inst-3 and inst-2, the executors being removed", got)
	}
	const stop, rm = "stop triage-factory-executor-3 triage-factory-executor-2", "rm triage-factory-executor-3 triage-factory-executor-2"
	lastPoll, stopped, removed, start := -1, callIndex(f, stop), callIndex(f, rm), callIndex(f, composeUpArgs("triage-factory", "1"))
	for i, call := range f.dockerCalls {
		if strings.Join(call, " ") == fleetList {
			lastPoll = i
		}
	}
	if lastPoll < 0 || lastPoll >= stopped || stopped >= removed || removed >= start {
		t.Errorf("want the drain wait, then %q, then %q, then compose up; calls: %q", stop, rm, f.dockerCalls)
	}
	if f.called("exec triage-factory-executor-1") || strings.Contains(strings.Join(f.dockerCalls[stopped], " "), "executor-1") {
		t.Error("the executor that stays was touched")
	}
	if st, _ := readState(dir); st.Executors != 1 {
		t.Errorf("state executors = %d; want 1", st.Executors)
	}
	out := f.out.String()
	wantLine(t, out, "Executors", pass, "2 of 3 to remove: triage-factory-executor-3, triage-factory-executor-2; drained 2 until they stop")
	wantLine(t, out, "Runs", pass, "none in flight")
	wantLine(t, out, "Removed", pass, "triage-factory-executor-3, triage-factory-executor-2")
}

func TestUp_ScaleDown(t *testing.T) {
	tests := []struct {
		name     string
		stored   int
		stack    func(s *stackFixture)
		arrange  func(f *fakeHost)
		args     []string
		wantCode int
		check    func(t *testing.T, f *fakeHost)
	}{
		{
			name: "an executor whose id cannot be read is still removed",
			arrange: func(f *fakeHost) {
				f.dockerReplies["exec triage-factory-executor-3 sh"] = dockerReply{stderr: "no such file\n", err: errors.New("exit status 1")}
			},
			args: []string{"--executors", "2"},
			check: func(t *testing.T, f *fakeHost) {
				wantLine(t, f.out.String(), "Executors", warn, "could not drain triage-factory-executor-3 (read its instance id: no such file)")
				if callIndex(f, "rm triage-factory-executor-3") < 0 {
					t.Error("the executor was not removed")
				}
				if len(drainCalls(f)) > 0 {
					t.Errorf("drained %q; want none", drainCalls(f))
				}
			},
		},
		{
			name: "a stopped executor is removed without a drain",
			stack: func(s *stackFixture) {
				s.containers[len(s.containers)-1]["State"] = map[string]any{"Status": "exited", "ExitCode": 0}
			},
			// The fake's inspect does not see the rm; drop the container by
			// the time compose runs, as docker would have.
			arrange: func(f *fakeHost) { f.onStream = func([]string) { dropContainer(f, "triage-factory-executor-3") } },
			args:    []string{"--executors", "2"},
			check: func(t *testing.T, f *fakeHost) {
				if f.called("exec triage-factory-executor-3") || len(drainCalls(f)) > 0 {
					t.Errorf("a stopped executor was drained: %q", f.dockerCalls)
				}
				if callIndex(f, "rm triage-factory-executor-3") < 0 {
					t.Error("the stopped executor was not removed")
				}
			},
		},
		{
			name: "runs outlast the timeout",
			arrange: func(f *fakeHost) {
				f.dockerSeqs[fleetList] = []dockerReply{{stdout: fleetJSON("inst-3:until_restart:2:1")}}
			},
			args: []string{"--executors", "2", "--drain-timeout", "1m"},
			check: func(t *testing.T, f *fakeHost) {
				wantLine(t, f.out.String(), "Runs", warn, "2 still in flight after 1m0s")
				if callIndex(f, "rm triage-factory-executor-3") < 0 {
					t.Error("the executor was not removed after the timeout")
				}
			},
		},
		{
			name:     "an interrupt during the drain undoes it and removes nothing",
			arrange:  interruptOnSleep,
			args:     []string{"--executors", "2"},
			wantCode: 1,
			check: func(t *testing.T, f *fakeHost) {
				if !strings.Contains(f.errOut.String(), "interrupted; the drain was undone (1 executor(s)); no executor was removed, and nothing was started") {
					t.Errorf("stderr = %q; want it to say the drain was undone", f.errOut)
				}
				if got := undrainCalls(f); strings.Join(got, ",") != "inst-3" {
					t.Errorf("undrained %q; want inst-3", got)
				}
				if f.called("stop") || f.called("rm") || len(f.streamed) > 0 {
					t.Errorf("an interrupted scale-down removed or started something: %q %q", f.dockerCalls, f.streamed)
				}
			},
		},
		{
			name: "a stop that fails stops up before compose",
			arrange: func(f *fakeHost) {
				f.dockerReplies["stop"] = dockerReply{stderr: "boom\n", err: errors.New("exit status 1")}
			},
			args:     []string{"--executors", "1"},
			wantCode: 1,
			check: func(t *testing.T, f *fakeHost) {
				if len(f.streamed) > 0 {
					t.Errorf("compose ran after a failed stop: %q", f.streamed)
				}
				if !strings.Contains(f.errOut.String(), "they are drained until they restart") {
					t.Errorf("stderr = %q; want it to say the executors are drained", f.errOut)
				}
			},
		},
		{
			name:   "the stored count, with no flag",
			stored: 1,
			check: func(t *testing.T, f *fakeHost) {
				if callIndex(f, "rm triage-factory-executor-3 triage-factory-executor-2") < 0 {
					t.Errorf("up did not scale down to the stored count; calls: %q", f.dockerCalls)
				}
			},
		},
		{
			name: "scaling up removes nothing",
			args: []string{"--executors", "5"},
			check: func(t *testing.T, f *fakeHost) {
				if f.called("stop") || f.called("rm") || len(drainCalls(f)) > 0 {
					t.Errorf("scaling up drained or removed executors: %q", f.dockerCalls)
				}
				wantStreamed(t, f, composeUpArgs("triage-factory", "5"))
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			stored := tt.stored
			if stored == 0 {
				stored = 3
			}
			f, b, dir := scaleFixture(t, stored, tt.stack)
			if tt.arrange != nil {
				tt.arrange(f)
			}
			if code := upRun(t, f, b, dir, tt.args...); code != tt.wantCode {
				t.Fatalf("exit = %d; want %d\n%s%s", code, tt.wantCode, f.out, f.errOut)
			}
			tt.check(t, f)
		})
	}
}

// dropContainer removes the container with id from the fake's inspect reply.
func dropContainer(f *fakeHost, id string) {
	var containers []map[string]any
	_ = json.Unmarshal([]byte(f.dockerReplies["inspect"].stdout), &containers)
	kept := containers[:0]
	for _, c := range containers {
		if c["Id"] != id {
			kept = append(kept, c)
		}
	}
	out, _ := json.Marshal(kept)
	f.dockerReplies["inspect"] = dockerReply{stdout: string(out)}
}
