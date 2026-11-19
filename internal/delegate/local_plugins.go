package delegate

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"

	"github.com/sky-ai-eng/triage-factory/internal/agentproc"
	"github.com/sky-ai-eng/triage-factory/internal/agentproc/localsandbox"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/worktree"
)

// A team's local plugins reach a spawn here. resolveSDKTeamOptions reads the
// list; preparePluginsForSpawn validates and resolves it, creates each entry's
// run state directory and builds the callbacks that record what the wrapper
// reports; the two spawn sites (runAgent and ResumeWithMessage) put the result
// on the run's agentproc.RunOptions. A team with no plugins leaves the options
// untouched, so such a spawn is byte-identical to one made before the feature.

// pluginSpawn names the run a plugin list is prepared for.
type pluginSpawn struct {
	orgID          string
	teamID         string
	conversationID string
	claimID        string
	// runRoot is the run's working directory, the tree its state directories
	// live under (<runRoot>/_tfac/plugins/<index>).
	runRoot string
	// addDirs are the run's --add-dir grants, which a plugin may not live in.
	addDirs []string
	// resuming is set when the spawn continues an existing session, so a state
	// directory that had to be recreated is a reset of the plugin's per-run
	// state rather than its first creation.
	resuming bool
}

// spawnPlugins is a prepared plugin list and the callbacks that report on it.
// The zero value is a run with no plugins.
type spawnPlugins struct {
	list            []agentproc.Plugin
	onLoaded        func(agentproc.PluginsReport)
	onEnvKeyDropped func(key, reason string)
}

// apply sets the plugin fields on opts. A zero spawnPlugins sets nothing.
func (sp spawnPlugins) apply(opts *agentproc.RunOptions) {
	if len(sp.list) == 0 {
		return
	}
	opts.Plugins = sp.list
	opts.OnPluginsLoaded = sp.onLoaded
	opts.OnPluginEnvKeyDropped = sp.onEnvKeyDropped
}

// extraAllowedTools returns base, the run's extra allowlist entries, with each
// plugin's grant_tools added once. An entry that grants nothing adds nothing.
func (sp spawnPlugins) extraAllowedTools(base string) string {
	have := map[string]struct{}{}
	for _, tool := range strings.Split(base, ",") {
		if tool = strings.TrimSpace(tool); tool != "" {
			have[tool] = struct{}{}
		}
	}
	var added []string
	for _, p := range sp.list {
		for _, tool := range p.GrantTools {
			if _, dup := have[tool]; dup {
				continue
			}
			have[tool] = struct{}{}
			added = append(added, tool)
		}
	}
	switch {
	case len(added) == 0:
		return base
	case strings.TrimSpace(base) == "":
		return strings.Join(added, ",")
	default:
		return base + "," + strings.Join(added, ",")
	}
}

// specAddDirs is the --add-dir list a local sandbox plan binds.
func specAddDirs(spec *localsandbox.Spec) []string {
	if spec == nil {
		return nil
	}
	return spec.AddDirs
}

// preparePluginsForSpawn prepares list for one spawn. An error is a spawn
// refused: it wraps agentproc.ErrPluginNotLoaded, so its text starts
// plugin_not_loaded, and the caller fails the run with it as it stands.
func (s *Spawner) preparePluginsForSpawn(ctx context.Context, in pluginSpawn, list []domain.LocalPlugin) (spawnPlugins, error) {
	if len(list) == 0 {
		return spawnPlugins{}, nil
	}
	plugins, warnings, err := agentproc.PreparePlugins(list, agentproc.PluginPrepareContext{
		RunRoot:  in.runRoot,
		AddDirs:  in.addDirs,
		RunsBase: worktree.RunsBase(),
	})
	if err != nil {
		// A refused spawn is a plugin that did not load, so it counts with
		// the ones the wrapper reports, or an alert on the counter would not
		// see the runs that never started.
		recordLocalPluginNotLoaded(in.teamID)
		delegateLog.Error("local plugins refused the spawn", "conversation", in.conversationID, "team", in.teamID, "error", err)
		return spawnPlugins{}, err
	}
	for _, w := range warnings {
		delegateLog.Warn("local plugin setting left out of the run",
			"conversation", in.conversationID, "team", in.teamID, "field", w.Field, "reason", w.Message)
		if w.EnvKeyDropped {
			recordLocalPluginEnvKeyDropped(in.teamID)
		}
	}
	if len(plugins) == 0 {
		return spawnPlugins{}, nil
	}

	recordLocalPluginSpawn(in.teamID)
	logPluginSpawn(in, plugins)

	return spawnPlugins{
		list:     plugins,
		onLoaded: s.pluginsLoadedHandler(ctx, in),
		onEnvKeyDropped: func(key, reason string) {
			// The key only: the value is the operator's and may be sensitive.
			delegateLog.Warn("local plugin env key dropped at spawn",
				"conversation", in.conversationID, "team", in.teamID, "key", key, "reason", reason)
			recordLocalPluginEnvKeyDropped(in.teamID)
		},
	}, nil
}

// logPluginSpawn writes the spawn's one info line: what the run was given.
// Env entries are listed by key, never by value.
func logPluginSpawn(in pluginSpawn, plugins []agentproc.Plugin) {
	var (
		required   []bool
		grants     [][]string
		envKeys    [][]string
		resetState []string
	)
	for _, p := range plugins {
		required = append(required, p.Required)
		grants = append(grants, p.GrantTools)
		keys := []string{}
		for _, e := range p.Env {
			key, _, _ := strings.Cut(e, "=")
			keys = append(keys, key)
		}
		envKeys = append(envKeys, keys)
		if in.resuming && p.StateDirCreated {
			resetState = append(resetState, p.StateDir)
		}
	}
	delegateLog.Info("local plugins loading into the run",
		"conversation", in.conversationID, "team", in.teamID, "count", len(plugins),
		"paths", agentproc.PluginPaths(plugins), "required", required, "grant_tools", grants, "env_keys", envKeys)
	if len(resetState) > 0 {
		delegateLog.Info("the resumed run's plugin state directories did not exist and were recreated empty; the plugins' per-run state is reset",
			"conversation", in.conversationID, "team", in.teamID, "state_dirs", resetState)
	}
}

// pluginsLoadedHandler returns the callback for the wrapper's plugins_loaded
// report. It runs on the run's reader goroutine, so it is one log line, two
// instrument writes and one bounded store write.
func (s *Spawner) pluginsLoadedHandler(ctx context.Context, in pluginSpawn) func(agentproc.PluginsReport) {
	// The init histogram is the time to the first report: a re-emitted init
	// reports again, later, and is not the plugins' start-up cost.
	var initRecorded bool
	return func(r agentproc.PluginsReport) {
		loaded := make([]string, 0, len(r.Plugins))
		for _, p := range r.Plugins {
			loaded = append(loaded, p.Name+"@"+p.Version)
		}
		failed := make([]string, 0, len(r.Failed))
		for _, f := range r.Failed {
			failed = append(failed, f.Path+": "+f.Reason)
		}
		// Warn for any plugin that did not load, required or not: a run that
		// goes on without an optional plugin is still missing it.
		log := delegateLog.Info
		if !r.OK || len(r.Failed) > 0 {
			log = delegateLog.Warn
		}
		log("local plugins reported",
			"conversation", in.conversationID, "team", in.teamID, "ok", r.OK,
			"plugins", loaded, "failed", failed, "since_spawn", r.SinceSpawn)

		if !initRecorded {
			initRecorded = true
			recordLocalPluginInit(in.teamID, r.SinceSpawn)
		}
		if !r.OK {
			recordLocalPluginNotLoaded(in.teamID)
		}

		if s.conversations == nil || in.claimID == "" {
			return
		}
		// Detached: the report can arrive as the run is being stopped, and the
		// record of what loaded is the more useful for a run that failed.
		wctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), detachedWriteDeadline)
		defer cancel()
		if _, err := s.conversations.RecordClaimPluginsLoadedSystem(wctx, in.orgID, in.claimID, r.PluginsLoaded); err != nil {
			delegateLog.Warn("record the plugins the run loaded failed; the run goes on",
				"conversation", in.conversationID, "claim", in.claimID, "error", err)
		}
	}
}

// failPluginNotLoaded makes a required plugin's failure the run's outcome. The
// wrapper reports it by exiting, which can follow a turn's result, so a result
// the process produced before it does not stand: the run's tool calls were
// refused until the check passed, and the check did not. err is the live
// process's terminal error. A disposition that already left the process (a
// fenced claim, a park) is not changed.
func failPluginNotLoaded(out *liveOutcome, err error) {
	if out.fenced || out.hibernated || !errors.Is(err, agentproc.ErrPluginNotLoaded) {
		return
	}
	out.result = nil
	out.err = err
}

// isSpawnRefusal reports whether err is a spawn refused for its team's
// settings: a required plugin that did not load, or settings that could not be
// read. The error's text starts with the machine-readable reason, so a failure
// carries it as it stands.
func isSpawnRefusal(err error) bool {
	return errors.Is(err, agentproc.ErrPluginNotLoaded) || errors.Is(err, errTeamSettingsUnreadable)
}

// sdkFailureMessage is the text an SDK engagement's failure carries. A refusal
// is its own text: the run's stderr says nothing more about it.
func sdkFailureMessage(err error, stderr string) string {
	if isSpawnRefusal(err) {
		return err.Error()
	}
	return fmt.Sprintf("%v\nstderr: %s", err, stderr)
}

// resumeFailureMessage is the text a failed resume carries.
func resumeFailureMessage(err error) string {
	if isSpawnRefusal(err) {
		return err.Error()
	}
	return "resume failed: " + err.Error()
}

// localPlugins owns this package's plugin instruments. Package-level because a
// metric instrument is process-global by nature, and swapped atomically so a
// test can back it with a manual reader while a run may still be counting.
var localPlugins atomic.Pointer[localPluginStats]

func init() {
	localPlugins.Store(newLocalPluginStats(otel.GetMeterProvider()))
}

type localPluginStats struct {
	spawns        metric.Int64Counter
	notLoaded     metric.Int64Counter
	envKeyDropped metric.Int64Counter
	initTime      metric.Float64Histogram
}

// pluginInitBuckets are the seconds the init histogram splits at. A guard
// plugin's SessionStart was measured at about 19 seconds, and the wait for it
// is bounded at 60 by default.
var pluginInitBuckets = []float64{1, 2, 5, 10, 15, 20, 30, 45, 60, 90}

// newLocalPluginStats builds the instruments against mp — production passes
// the global provider, which telemetry.Init installs. An instrument-creation
// error can only be a programmer error, and the API hands back a usable no-op
// alongside it, so it is logged rather than propagated.
func newLocalPluginStats(mp metric.MeterProvider) *localPluginStats {
	m := mp.Meter("internal/delegate")
	spawns, err := m.Int64Counter("local_plugin.spawns",
		metric.WithDescription("Local-mode spawns that carried at least one local plugin, by team."))
	if err != nil {
		delegateLog.Warn("local plugin spawn counter setup failed", "error", err)
	}
	notLoaded, err := m.Int64Counter("local_plugin.not_loaded",
		metric.WithDescription("Runs a local plugin did not load for: refused at the spawn, or reported not loaded by the run, by team."))
	if err != nil {
		delegateLog.Warn("local plugin not-loaded counter setup failed", "error", err)
	}
	envKeyDropped, err := m.Int64Counter("local_plugin.env_key_dropped",
		metric.WithDescription("Plugin env keys left out of a run because they are no longer allowed or Triage Factory sets the key itself, by team."))
	if err != nil {
		delegateLog.Warn("local plugin env-key counter setup failed", "error", err)
	}
	initSeconds, err := m.Float64Histogram("local_plugin.init",
		metric.WithUnit("s"),
		metric.WithDescription("Seconds from a spawn with local plugins to the wrapper's report on them: the plugins' SessionStart cost plus the wait for the first input."),
		metric.WithExplicitBucketBoundaries(pluginInitBuckets...))
	if err != nil {
		delegateLog.Warn("local plugin init histogram setup failed", "error", err)
	}
	return &localPluginStats{spawns: spawns, notLoaded: notLoaded, envKeyDropped: envKeyDropped, initTime: initSeconds}
}

func pluginTeamAttr(team string) metric.MeasurementOption {
	return metric.WithAttributes(attribute.String("team", team))
}

func recordLocalPluginSpawn(team string) {
	if stats := localPlugins.Load(); stats != nil && stats.spawns != nil {
		stats.spawns.Add(context.Background(), 1, pluginTeamAttr(team))
	}
}

func recordLocalPluginNotLoaded(team string) {
	if stats := localPlugins.Load(); stats != nil && stats.notLoaded != nil {
		stats.notLoaded.Add(context.Background(), 1, pluginTeamAttr(team))
	}
}

func recordLocalPluginEnvKeyDropped(team string) {
	if stats := localPlugins.Load(); stats != nil && stats.envKeyDropped != nil {
		stats.envKeyDropped.Add(context.Background(), 1, pluginTeamAttr(team))
	}
}

func recordLocalPluginInit(team string, since time.Duration) {
	if stats := localPlugins.Load(); stats != nil && stats.initTime != nil {
		stats.initTime.Record(context.Background(), since.Seconds(), pluginTeamAttr(team))
	}
}
