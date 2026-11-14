package delegate

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"github.com/sky-ai-eng/triage-factory/internal/agentproc"
	"github.com/sky-ai-eng/triage-factory/internal/agentproc/localsandbox"
	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// pluginMetrics backs the plugin instruments with a manual reader for one test.
type pluginMetrics struct {
	t      *testing.T
	reader *sdkmetric.ManualReader
}

func swapPluginMetrics(t *testing.T) *pluginMetrics {
	t.Helper()
	reader := sdkmetric.NewManualReader()
	prev := localPlugins.Swap(newLocalPluginStats(sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))))
	t.Cleanup(func() { localPlugins.Store(prev) })
	return &pluginMetrics{t: t, reader: reader}
}

func (m *pluginMetrics) metric(name string) (metricdata.Aggregation, bool) {
	m.t.Helper()
	var rm metricdata.ResourceMetrics
	if err := m.reader.Collect(context.Background(), &rm); err != nil {
		m.t.Fatalf("collect metrics: %v", err)
	}
	for _, sm := range rm.ScopeMetrics {
		for _, mt := range sm.Metrics {
			if mt.Name == name {
				return mt.Data, true
			}
		}
	}
	return nil, false
}

// counter is the counter's value for team, 0 when it has no series.
func (m *pluginMetrics) counter(name, team string) int64 {
	m.t.Helper()
	data, ok := m.metric(name)
	if !ok {
		return 0
	}
	sum, ok := data.(metricdata.Sum[int64])
	if !ok {
		m.t.Fatalf("%s is %T, want an int64 sum", name, data)
	}
	for _, dp := range sum.DataPoints {
		if v, ok := dp.Attributes.Value(attribute.Key("team")); ok && v.AsString() == team {
			return dp.Value
		}
	}
	return 0
}

// histogram is the observation count and sum for team.
func (m *pluginMetrics) histogram(name, team string) (uint64, float64) {
	m.t.Helper()
	data, ok := m.metric(name)
	if !ok {
		return 0, 0
	}
	h, ok := data.(metricdata.Histogram[float64])
	if !ok {
		m.t.Fatalf("%s is %T, want a float64 histogram", name, data)
	}
	for _, dp := range h.DataPoints {
		if v, ok := dp.Attributes.Value(attribute.Key("team")); ok && v.AsString() == team {
			return dp.Count, dp.Sum
		}
	}
	return 0, 0
}

// writePluginDir makes a directory that passes the plugin checks.
func writePluginDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, ".claude-plugin"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".claude-plugin", "plugin.json"), []byte(`{"name":"guard","version":"1.0.0"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	resolved, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatal(err)
	}
	return resolved
}

func TestSpawnPlugins_ExtraAllowedTools(t *testing.T) {
	plugins := func(grants ...[]string) spawnPlugins {
		var sp spawnPlugins
		for _, g := range grants {
			sp.list = append(sp.list, agentproc.Plugin{GrantTools: g})
		}
		return sp
	}
	for _, tc := range []struct {
		name string
		sp   spawnPlugins
		base string
		want string
	}{
		{name: "no plugins leaves the base alone", sp: spawnPlugins{}, base: "mcp__a,mcp__b", want: "mcp__a,mcp__b"},
		{name: "no plugins leaves an empty base empty", sp: spawnPlugins{}, base: "", want: ""},
		{name: "entries that grant nothing add nothing", sp: plugins(nil, nil), base: "mcp__a", want: "mcp__a"},
		{name: "only the entry that sets grants adds them", sp: plugins(nil, []string{"Skill"}, nil), base: "", want: "Skill"},
		{name: "grants follow the base", sp: plugins([]string{"Agent", "Skill"}), base: "mcp__a", want: "mcp__a,Agent,Skill"},
		{name: "a grant two entries share is added once", sp: plugins([]string{"Skill"}, []string{"Agent", "Skill"}), base: "", want: "Skill,Agent"},
		{name: "a grant the base has is not repeated", sp: plugins([]string{"Skill"}), base: "mcp__a, Skill", want: "mcp__a, Skill"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.sp.extraAllowedTools(tc.base); got != tc.want {
				t.Errorf("extraAllowedTools(%q) = %q, want %q", tc.base, got, tc.want)
			}
		})
	}
}

func TestSpawnPlugins_ApplyLeavesAnEmptyListUntouched(t *testing.T) {
	var opts agentproc.RunOptions
	spawnPlugins{onLoaded: func(agentproc.PluginsReport) {}}.apply(&opts)
	if opts.Plugins != nil || opts.OnPluginsLoaded != nil || opts.OnPluginEnvKeyDropped != nil {
		t.Errorf("a spawn with no plugins set plugin options: %+v", opts)
	}
}

func TestSpecAddDirs(t *testing.T) {
	if got := specAddDirs(nil); got != nil {
		t.Errorf("specAddDirs(nil) = %v, want nil", got)
	}
	if got := specAddDirs(&localsandbox.Spec{AddDirs: []string{"/a", "/b"}}); len(got) != 2 || got[0] != "/a" {
		t.Errorf("specAddDirs = %v, want the spec's list", got)
	}
}

func TestPreparePluginsForSpawn_EmptyListChangesNothing(t *testing.T) {
	m := swapPluginMetrics(t)
	s := NewSpawner(nil, db.Stores{}, nil, nil, "")

	sp, err := s.preparePluginsForSpawn(t.Context(), pluginSpawn{teamID: "team-1", runRoot: t.TempDir()}, nil)
	if err != nil {
		t.Fatalf("preparePluginsForSpawn: %v", err)
	}
	if sp.list != nil || sp.onLoaded != nil || sp.onEnvKeyDropped != nil {
		t.Errorf("an empty list produced %+v", sp)
	}
	if got := m.counter("local_plugin.spawns", "team-1"); got != 0 {
		t.Errorf("spawns = %d, want 0", got)
	}
}

// TestPreparePluginsForSpawn_RequiredFailureFailsTheSpawn: a required entry
// that cannot load refuses the spawn with the plugin_not_loaded text, and the
// refusal counts as a plugin that did not load.
func TestPreparePluginsForSpawn_RequiredFailureFailsTheSpawn(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeLocal)
	m := swapPluginMetrics(t)
	s := NewSpawner(nil, db.Stores{}, nil, nil, "")
	missing := filepath.Join(t.TempDir(), "gone")

	_, err := s.preparePluginsForSpawn(t.Context(), pluginSpawn{teamID: "team-1", runRoot: t.TempDir()},
		[]domain.LocalPlugin{{Path: missing, Required: true}})
	if err == nil {
		t.Fatal("a required plugin that does not exist must fail the spawn")
	}
	if !errors.Is(err, agentproc.ErrPluginNotLoaded) {
		t.Errorf("err = %v, want it to wrap ErrPluginNotLoaded", err)
	}
	if !strings.HasPrefix(err.Error(), "plugin_not_loaded:") {
		t.Errorf("err text = %q, want the plugin_not_loaded: prefix", err.Error())
	}
	if got := sdkFailureMessage(err, "noisy stderr"); got != err.Error() {
		t.Errorf("failure message = %q, want the error's own text without stderr", got)
	}
	if kind := classifyFailureKind(err); kind != domain.ConversationFailureUnclassified {
		t.Errorf("failure kind = %q, want unclassified", kind)
	}
	if got := m.counter("local_plugin.not_loaded", "team-1"); got != 1 {
		t.Errorf("not_loaded = %d, want 1", got)
	}
	if got := m.counter("local_plugin.spawns", "team-1"); got != 0 {
		t.Errorf("spawns = %d, want 0: a refused spawn carried no plugin", got)
	}
}

// TestPreparePluginsForSpawn_OptionalFailureIsAWarning: a non-required entry
// that cannot load is left out and the spawn goes on.
func TestPreparePluginsForSpawn_OptionalFailureIsAWarning(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeLocal)
	m := swapPluginMetrics(t)
	log := captureDelegateLog(t)
	s := NewSpawner(nil, db.Stores{}, nil, nil, "")

	sp, err := s.preparePluginsForSpawn(t.Context(), pluginSpawn{teamID: "team-1", runRoot: t.TempDir()},
		[]domain.LocalPlugin{{Path: filepath.Join(t.TempDir(), "gone")}})
	if err != nil {
		t.Fatalf("a non-required plugin must not fail the spawn: %v", err)
	}
	if len(sp.list) != 0 {
		t.Errorf("plugins = %+v, want the unusable entry left out", sp.list)
	}
	log.mu.Lock()
	warned := len(log.records)
	log.mu.Unlock()
	if warned == 0 {
		t.Error("the entry left out was not logged")
	}
	if got := m.counter("local_plugin.spawns", "team-1"); got != 0 {
		t.Errorf("spawns = %d, want 0: no plugin reached the run", got)
	}
}

func TestPreparePluginsForSpawn_PreparesTheRun(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeLocal)
	m := swapPluginMetrics(t)
	s := NewSpawner(nil, db.Stores{}, nil, nil, "")
	plugin := writePluginDir(t)
	runRoot := t.TempDir()

	sp, err := s.preparePluginsForSpawn(t.Context(), pluginSpawn{
		teamID: "team-1", conversationID: "conv-1", runRoot: runRoot,
	}, []domain.LocalPlugin{
		{
			Path:       plugin,
			Required:   true,
			GrantTools: []string{"Skill"},
			Env:        map[string]string{"GUARD_STATE": "{run_state_dir}state", "TF_SNEAKY": "x"},
		},
	})
	if err != nil {
		t.Fatalf("preparePluginsForSpawn: %v", err)
	}
	if len(sp.list) != 1 || sp.list[0].Path != plugin || !sp.list[0].Required {
		t.Fatalf("plugins = %+v, want the one resolved entry", sp.list)
	}
	wantState := filepath.Join(runRoot, "_tfac", "plugins", "0")
	if sp.list[0].StateDir != wantState {
		t.Errorf("state dir = %q, want %q", sp.list[0].StateDir, wantState)
	}
	if info, err := os.Stat(wantState); err != nil || !info.IsDir() {
		t.Errorf("state dir was not created: %v", err)
	}
	wantEnv := "GUARD_STATE=" + wantState + string(filepath.Separator) + "state"
	if len(sp.list[0].Env) != 1 || sp.list[0].Env[0] != wantEnv {
		t.Errorf("env = %v, want [%s]: the denied TF_ key must be dropped", sp.list[0].Env, wantEnv)
	}
	if got := sp.extraAllowedTools(""); got != "Skill" {
		t.Errorf("extra tools = %q, want Skill", got)
	}

	var opts agentproc.RunOptions
	sp.apply(&opts)
	if len(opts.Plugins) != 1 || opts.OnPluginsLoaded == nil || opts.OnPluginEnvKeyDropped == nil {
		t.Errorf("apply left the options without the plugin fields: %+v", opts)
	}

	if got := m.counter("local_plugin.spawns", "team-1"); got != 1 {
		t.Errorf("spawns = %d, want 1", got)
	}
	if got := m.counter("local_plugin.env_key_dropped", "team-1"); got != 1 {
		t.Errorf("env_key_dropped = %d, want 1 for the TF_ key", got)
	}
	if got := m.counter("local_plugin.not_loaded", "team-1"); got != 0 {
		t.Errorf("not_loaded = %d, want 0", got)
	}
}

// TestPreparePluginsForSpawn_PluginInsideAnAddDirIsRefused pins that the
// sandbox plan's grants reach the prepare check.
func TestPreparePluginsForSpawn_PluginInsideAnAddDirIsRefused(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeLocal)
	swapPluginMetrics(t)
	s := NewSpawner(nil, db.Stores{}, nil, nil, "")
	plugin := writePluginDir(t)

	_, err := s.preparePluginsForSpawn(t.Context(), pluginSpawn{
		teamID: "team-1", runRoot: t.TempDir(), addDirs: []string{filepath.Dir(plugin)},
	}, []domain.LocalPlugin{{Path: plugin, Required: true}})
	if !errors.Is(err, agentproc.ErrPluginNotLoaded) {
		t.Errorf("err = %v, want a plugin under an add-dir refused as plugin_not_loaded", err)
	}
}

func TestPreparePluginsForSpawn_SpawnLogNamesEnvKeysNotValues(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeLocal)
	swapPluginMetrics(t)
	log := captureDelegateLog(t)
	s := NewSpawner(nil, db.Stores{}, nil, nil, "")
	plugin := writePluginDir(t)

	_, err := s.preparePluginsForSpawn(t.Context(), pluginSpawn{teamID: "team-1", conversationID: "conv-1", runRoot: t.TempDir()},
		[]domain.LocalPlugin{{Path: plugin, Env: map[string]string{"GUARD_MODE": "hunter2-literal"}}})
	if err != nil {
		t.Fatalf("preparePluginsForSpawn: %v", err)
	}
	log.mu.Lock()
	defer log.mu.Unlock()
	var spawnLine string
	for _, r := range log.records {
		if r.Message == "local plugins loading into the run" {
			var b strings.Builder
			r.Attrs(func(a slog.Attr) bool { fmt.Fprintf(&b, "%s=%v ", a.Key, a.Value); return true })
			spawnLine = b.String()
		}
	}
	if spawnLine == "" {
		t.Fatal("no spawn log line")
	}
	if !strings.Contains(spawnLine, "GUARD_MODE") {
		t.Errorf("spawn line %q does not name the env key", spawnLine)
	}
	if strings.Contains(spawnLine, "hunter2-literal") {
		t.Errorf("spawn line %q carries an env value", spawnLine)
	}
}

// pluginReportStore records what the report handler persists.
type pluginReportStore struct {
	db.ConversationStore
	mu      sync.Mutex
	calls   int
	orgID   string
	claimID string
	report  domain.PluginsLoaded
	err     error
}

func (f *pluginReportStore) RecordClaimPluginsLoadedSystem(_ context.Context, orgID, claimID string, report domain.PluginsLoaded) (*domain.Claim, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	f.orgID, f.claimID, f.report = orgID, claimID, report
	return nil, f.err
}

func TestPluginsLoadedHandler_RecordsAndCounts(t *testing.T) {
	for _, tc := range []struct {
		name         string
		report       domain.PluginsLoaded
		wantNotLoad  int64
		storeErr     error
		wantPersists int
	}{
		{
			name: "ok",
			report: domain.PluginsLoaded{
				OK:      true,
				Plugins: []domain.LoadedPlugin{{Name: "guard", Version: "1.2.3", Path: "/p", Required: true}},
			},
			wantPersists: 1,
		},
		{
			name: "not ok",
			report: domain.PluginsLoaded{
				Failed: []domain.PluginFailure{{Path: "/p", Reason: "plugin not in init", Required: true}},
			},
			wantNotLoad:  1,
			wantPersists: 1,
		},
		{
			name:         "a store failure does not stop the report",
			report:       domain.PluginsLoaded{OK: true},
			storeErr:     errors.New("db down"),
			wantPersists: 1,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := swapPluginMetrics(t)
			store := &pluginReportStore{err: tc.storeErr}
			s := NewSpawner(nil, db.Stores{}, nil, nil, "")
			s.conversations = store
			// A cancelled run ctx: the record is detached from it.
			ctx, cancel := context.WithCancel(t.Context())
			cancel()
			h := s.pluginsLoadedHandler(ctx, pluginSpawn{orgID: "org-1", teamID: "team-1", conversationID: "conv-1", claimID: "claim-1"})

			h(agentproc.PluginsReport{PluginsLoaded: tc.report, SinceSpawn: 19 * time.Second})

			if got := m.counter("local_plugin.not_loaded", "team-1"); got != tc.wantNotLoad {
				t.Errorf("not_loaded = %d, want %d", got, tc.wantNotLoad)
			}
			count, sum := m.histogram("local_plugin.init", "team-1")
			if count != 1 || sum != 19 {
				t.Errorf("init histogram = (%d, %v), want one observation of 19s", count, sum)
			}
			store.mu.Lock()
			defer store.mu.Unlock()
			if store.calls != tc.wantPersists {
				t.Fatalf("persisted %d times, want %d", store.calls, tc.wantPersists)
			}
			if store.orgID != "org-1" || store.claimID != "claim-1" {
				t.Errorf("persisted on (%q, %q), want (org-1, claim-1)", store.orgID, store.claimID)
			}
			if store.report.OK != tc.report.OK || len(store.report.Plugins) != len(tc.report.Plugins) || len(store.report.Failed) != len(tc.report.Failed) {
				t.Errorf("persisted %+v, want %+v", store.report, tc.report)
			}
		})
	}
}

func TestPluginsLoadedHandler_NoClaimRecordsNothing(t *testing.T) {
	swapPluginMetrics(t)
	store := &pluginReportStore{}
	s := NewSpawner(nil, db.Stores{}, nil, nil, "")
	s.conversations = store
	s.pluginsLoadedHandler(t.Context(), pluginSpawn{teamID: "team-1"})(agentproc.PluginsReport{PluginsLoaded: domain.PluginsLoaded{OK: true}})
	if store.calls != 0 {
		t.Errorf("persisted %d times with no claim id", store.calls)
	}
}

func TestPreparePluginsForSpawn_EnvKeyDroppedCallback(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeLocal)
	m := swapPluginMetrics(t)
	log := captureDelegateLog(t)
	s := NewSpawner(nil, db.Stores{}, nil, nil, "")
	sp, err := s.preparePluginsForSpawn(t.Context(), pluginSpawn{teamID: "team-1", runRoot: t.TempDir()},
		[]domain.LocalPlugin{{Path: writePluginDir(t)}})
	if err != nil {
		t.Fatal(err)
	}

	sp.onEnvKeyDropped("GIT_DIR", "Triage Factory sets this key for the run")

	if got := m.counter("local_plugin.env_key_dropped", "team-1"); got != 1 {
		t.Errorf("env_key_dropped = %d, want 1", got)
	}
	log.mu.Lock()
	defer log.mu.Unlock()
	if len(log.records) == 0 {
		t.Error("the dropped key was not logged")
	}
}

func TestFailPluginNotLoaded(t *testing.T) {
	pluginErr := fmt.Errorf("%w: /opt/plugins/guard: not loaded", agentproc.ErrPluginNotLoaded)
	result := &agentproc.Result{StopReason: "end_turn", Result: "Done."}

	t.Run("a plugin failure wins over a result", func(t *testing.T) {
		out := liveOutcome{result: result}
		failPluginNotLoaded(&out, pluginErr)
		if out.result != nil {
			t.Errorf("result = %+v, want it cleared", out.result)
		}
		if !errors.Is(out.err, agentproc.ErrPluginNotLoaded) {
			t.Errorf("err = %v, want plugin_not_loaded", out.err)
		}
		if got := sdkFailureMessage(out.err, "stderr"); got != pluginErr.Error() {
			t.Errorf("failure message = %q, want %q", got, pluginErr.Error())
		}
	})
	t.Run("other process errors leave the result", func(t *testing.T) {
		out := liveOutcome{result: result}
		failPluginNotLoaded(&out, errors.New("exit status 1"))
		if out.result != result || out.err != nil {
			t.Errorf("outcome = %+v, want it untouched", out)
		}
	})
	t.Run("no error leaves the result", func(t *testing.T) {
		out := liveOutcome{result: result}
		failPluginNotLoaded(&out, nil)
		if out.result != result {
			t.Errorf("outcome = %+v, want it untouched", out)
		}
	})
	t.Run("a fenced or parked outcome is not rewritten", func(t *testing.T) {
		for _, out := range []liveOutcome{{fenced: true}, {hibernated: true}} {
			failPluginNotLoaded(&out, pluginErr)
			if out.err != nil {
				t.Errorf("outcome %+v was rewritten", out)
			}
		}
	})
}

// TestDriveThenFailPluginNotLoaded_ResultAlreadySeen follows the order the
// spawn sites run in: the driver hands back the result the process folded
// before the wrapper exited with plugin_not_loaded, and the override then
// makes the failure the outcome.
func TestDriveThenFailPluginNotLoaded_ResultAlreadySeen(t *testing.T) {
	s := NewSpawner(nil, db.Stores{}, nil, nil, "")
	proc := newFakeLiveProc("sess")
	pluginErr := fmt.Errorf("%w: /opt/plugins/guard", agentproc.ErrPluginNotLoaded)
	proc.exit(&agentproc.Result{StopReason: "end_turn", Result: "Done."}, pluginErr)

	out := s.driveLiveConversation(context.Background(), liveParkContext{}, proc, make(chan *agentproc.Result), nil)
	if out.result == nil {
		t.Fatalf("the driver dropped the result before the override ran: %+v", out)
	}
	failPluginNotLoaded(&out, proc.Err())

	if out.result != nil || !errors.Is(out.err, agentproc.ErrPluginNotLoaded) {
		t.Errorf("outcome = %+v, want the plugin failure and no result", out)
	}
}
