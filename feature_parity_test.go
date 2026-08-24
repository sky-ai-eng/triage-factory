package main

import (
	"os"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/sky-ai-eng/triage-factory/cmd/exec"
	"github.com/sky-ai-eng/triage-factory/cmd/exec/agenthost"
	"github.com/sky-ai-eng/triage-factory/internal/agentprompt"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/domain/events"
	"github.com/sky-ai-eng/triage-factory/internal/entitlements"
)

// TestAllDomainEventTypesRegistered_CompositionRoot is the out-of-core
// counterpart to internal/domain/events' own TestAllDomainEventTypesRegistered,
// which can only verify core-sourced types (github/jira/system) — it must
// never import ee/. This process (package main, which blank-imports ee/slack
// and friends) is the one place that sees every real init()-time schema
// registration, core and ee alike, so it's the one place that can catch a new
// domain.AllEventTypes() entry (e.g. an ee package's own event source) that
// forgot to register a matching events.Schema.
func TestAllDomainEventTypesRegistered_CompositionRoot(t *testing.T) {
	for _, et := range domain.AllEventTypes() {
		if _, ok := events.Get(et.ID); !ok {
			t.Errorf("event type %q is in domain.AllEventTypes() but not registered in the events package", et.ID)
		}
	}
}

// TestRegisteredFeaturesAreDeclared asserts that every feature referenced by
// a registration seam (event-source gates, agenthost extension namespaces) is
// a member of entitlements.AllFeatures().
//
// Why this must live at the composition root: package main is the sole
// importer of ee/, so this test process — and only this test process — sees
// exactly the real init()-time registrations. Seam tests elsewhere register
// synthetic features (Feature("test-...")) with t.Cleanup resets; those never
// appear here, which is why this is a test and not a registration-time panic
// (a membership panic would outlaw that synthetic-feature test pattern).
//
// Why membership matters: gating works either way (Has is a plain map
// lookup), but /api/entitlements answers by iterating AllFeatures() — a
// feature missing from it never reaches the frontend probe, so every
// render-gated surface stays dark even for licensed orgs.
func TestRegisteredFeaturesAreDeclared(t *testing.T) {
	declared := map[entitlements.Feature]bool{}
	for _, f := range entitlements.AllFeatures() {
		declared[f] = true
	}

	check := func(source string, features []entitlements.Feature) {
		for _, f := range features {
			if !declared[f] {
				t.Errorf("%s references feature %q, which is not in entitlements.AllFeatures() — add it to allFeatures (and mirror it in useEntitlements.ts) or the /api/entitlements probe will never surface it", source, f)
			}
		}
	}
	check("entitlements.GateEventSource", entitlements.RegisteredEventGateFeatures())
	check("agenthost.RegisterExtension", agenthost.RegisteredExtensionFeatures())
}

// TestExecSubcommandsAndToolsReferencesArePaired holds the two per-source
// registration seams an ee feature must both hit to one another: every
// registered exec subcommand naming a SourceKind has agent-facing tools
// reference text registered under that source (else the agent holds CLI verbs
// its prompt never mentions), and every registered tools reference is claimed
// by a subcommand's SourceKind (else the prompt documents verbs the CLI does
// not serve, and the help index has nothing to filter them by). Package main
// is the sole importer of ee/, so this process — and only this process — sees
// the real init()-time registrations on both sides.
func TestExecSubcommandsAndToolsReferencesArePaired(t *testing.T) {
	claimed := map[string]bool{}
	for name, sub := range exec.RegisteredSubcommands() {
		if sub.SourceKind == "" {
			continue
		}
		claimed[sub.SourceKind] = true
		if _, ok := agentprompt.ToolsReferenceFor(sub.SourceKind); !ok {
			t.Errorf("exec subcommand %q names source %q but no tools reference is registered for it", name, sub.SourceKind)
		}
	}
	for _, source := range agentprompt.RegisteredToolsReferenceSources() {
		if !claimed[source] {
			t.Errorf("tools reference %q has no exec subcommand claiming it via Subcommand.SourceKind", source)
		}
	}
}

// TestFrontendMirrorsAllFeatures asserts that every feature in
// entitlements.AllFeatures() has its wire value declared in the frontend
// useEntitlements hook — the second hand-maintained mirror of allFeatures,
// and the likelier one to drift since no Go compiler sees it. The hook's
// convention is one `export const Feature<X> = '<id>' as const` per feature
// (frontend/src/hooks/useEntitlements.ts); this matches on the literal.
func TestFrontendMirrorsAllFeatures(t *testing.T) {
	src, err := os.ReadFile("frontend/src/hooks/useEntitlements.ts")
	if err != nil {
		t.Fatalf("read useEntitlements.ts: %v", err)
	}
	content := string(src)
	for _, f := range entitlements.AllFeatures() {
		needle := "'" + string(f) + "' as const"
		if !strings.Contains(content, needle) {
			t.Errorf("feature %q is not mirrored in useEntitlements.ts — expected a `export const Feature<X> = %s` declaration", f, needle)
		}
	}
}

// TestFrontendMirrorsConversationStatusVocabulary asserts that the frontend's copy of
// the conversation status vocabulary matches internal/domain/conversation_status.go
// EXACTLY, in both directions.
//
// The mirror is hand-maintained by choice — codegen buys a build step and a
// generated file to keep eleven rarely-changing names honest. Both directions
// are load-bearing: a phase added in Go and not here silently misses every UI
// arm (that is how `awaiting_credentials` came to render as inert grey
// mid-setup), and a name here that Go never emits is a branch that can never be
// taken (that is how `initializing` and `worktree_created` outlived the backend
// states they described).
//
// What this test canNOT see is component code, which never reads the arrays it
// pins — it compares a status against a bare literal, and two retired statuses
// walked back in that way within days of this test landing. That half is
// enforced on the frontend side, by the conversation-status/no-ghost-conversation-status ESLint
// rule (frontend/eslint-rules/), which reads its vocabulary out of the very
// declarations parsed below.
func TestFrontendMirrorsConversationStatusVocabulary(t *testing.T) {
	src, err := os.ReadFile("frontend/src/types.ts")
	if err != nil {
		t.Fatalf("read types.ts: %v", err)
	}
	content := string(src)

	compare := func(decl string, want []string) {
		got, _ := tsArrayDecl(t, content, decl)
		if diff := vocabularyDiff(want, got); diff != "" {
			t.Errorf("frontend %s has drifted from internal/domain/conversation_status.go:\n%s", decl, diff)
		}
	}
	compare("CLAIM_PHASES", domain.AllClaimPhases())
	compare("TERMINAL_CONVERSATION_STATUSES", domain.AllTerminalConversationStatuses())

	// CONVERSATION_STATUSES is the full union, so it spells only the names that are
	// neither a phase nor a terminal and spreads the other two arrays — which
	// is what keeps it from being a third place to forget a phase.
	var base []string
	for _, s := range domain.AllConversationStatuses() {
		if !domain.IsClaimPhase(s) && !domain.IsTerminalConversationStatus(s) {
			base = append(base, s)
		}
	}
	members, spreads := tsArrayDecl(t, content, "CONVERSATION_STATUSES")
	if diff := vocabularyDiff(base, members); diff != "" {
		t.Errorf("frontend CONVERSATION_STATUSES spells the wrong non-phase non-terminal statuses:\n%s", diff)
	}
	for _, want := range []string{"CLAIM_PHASES", "TERMINAL_CONVERSATION_STATUSES"} {
		if !slices.Contains(spreads, want) {
			t.Errorf("frontend CONVERSATION_STATUSES must spread ...%s rather than re-listing its members (spreads: %v)", want, spreads)
		}
	}
}

// TestFrontendMirrorsParkReasonVocabulary asserts that every park reason the
// backend can write onto conversations.park_reason has a gloss in the
// frontend's PARK_REASON_LABELS, and that the map glosses nothing the backend
// never writes.
//
// Both directions decide what a person reads on the run station's "stop"
// readout. A reason missing from the map falls through parkReasonLabel to the
// raw identifier — the run station printing `blueprint_terminal` at a viewer is
// the exact defect the typed vocabulary replaced, since the old column printed
// whatever the last writer happened to leave, model stop reasons included. A
// key the backend never writes is the opposite: a phrase that can never appear,
// which is how a gloss outlives the reason it described.
func TestFrontendMirrorsParkReasonVocabulary(t *testing.T) {
	src, err := os.ReadFile("frontend/src/lib/conversationStatus.ts")
	if err != nil {
		t.Fatalf("read conversationStatus.ts: %v", err)
	}
	got := tsObjectKeys(t, string(src), "PARK_REASON_LABELS: Record<string, string>")

	want := make([]string, 0, len(domain.AllParkReasons()))
	for _, r := range domain.AllParkReasons() {
		want = append(want, string(r))
	}
	if diff := vocabularyDiff(want, got); diff != "" {
		t.Errorf("frontend PARK_REASON_LABELS has drifted from internal/domain/conversation_status.go:\n%s", diff)
	}
}

// TestFrontendMirrorsExternalActionVocabulary asserts that every external-action
// discriminator the backend can write has a presentation in the frontend's
// ACTION_META, and that ACTION_META spells no action the backend never writes.
//
// Both directions decide what a person sees on the audit log of record. An
// action missing from the map renders through FALLBACK_ACTION_META — an
// unlabelled "action" pill — and, because the lens's filter dropdown is derived
// from the map's keys, cannot be filtered for at all; that is how the four
// security-signal rows came to be the least legible rows on a governance
// surface. A key the backend never writes is the opposite defect: a filter
// option that always returns nothing, which reads as "this never happened"
// rather than "this cannot happen".
//
// The Go side is read out of the const block rather than a hand-kept
// domain.AllActions(), which would just be a third list to forget.
func TestFrontendMirrorsExternalActionVocabulary(t *testing.T) {
	src, err := os.ReadFile("internal/domain/external_action.go")
	if err != nil {
		t.Fatalf("read external_action.go: %v", err)
	}
	var want []string
	for _, m := range goActionConst.FindAllStringSubmatch(string(src), -1) {
		want = append(want, m[1])
	}
	if len(want) == 0 {
		t.Fatal("no Action* consts found in external_action.go — the const block's shape changed and this guard has stopped reading it")
	}

	meta, err := os.ReadFile("frontend/src/components/actionMeta.ts")
	if err != nil {
		t.Fatalf("read actionMeta.ts: %v", err)
	}
	got := tsObjectKeys(t, string(meta), "ACTION_META: Record<string, ActionMeta>")
	if diff := vocabularyDiff(want, got); diff != "" {
		t.Errorf("frontend ACTION_META has drifted from internal/domain/external_action.go:\n%s", diff)
	}
}

var (
	tsQuotedMember = regexp.MustCompile(`'([a-z_]+)'`)
	tsSpreadMember = regexp.MustCompile(`\.\.\.([A-Z_]+)`)
	// The const block's one-per-line `ActionX = "x"` form. Anchored to a leading
	// tab so a doc comment quoting a value can't be mistaken for a declaration.
	goActionConst = regexp.MustCompile(`(?m)^\tAction\w+\s+=\s+"([a-z_]+)"$`)
	// A top-level key of a TS object literal, at the one indent level the
	// formatter gives it.
	tsObjectKey = regexp.MustCompile(`(?m)^  ([a-z_]+): `)
)

// tsObjectKeys pulls the top-level keys out of an `export const <decl> = { … }`
// object literal, reading to the closing brace at column 0. A textual parser for
// the same reason tsArrayDecl is one: the declarations are plain literals by
// convention, and shelling out to a TS toolchain would make this test skip
// wherever the frontend isn't installed.
func tsObjectKeys(t *testing.T, src, decl string) []string {
	t.Helper()
	open := "export const " + decl + " = {"
	start := strings.Index(src, open)
	if start < 0 {
		t.Fatalf("no `%s…}` declaration found — the Go vocabulary needs a frontend mirror to check against", open)
	}
	body := src[start+len(open):]
	end := strings.Index(body, "\n}")
	if end < 0 {
		t.Fatalf("declaration %s is unterminated", decl)
	}
	var keys []string
	for _, m := range tsObjectKey.FindAllStringSubmatch(body[:end], -1) {
		keys = append(keys, m[1])
	}
	return keys
}

// tsArrayDecl pulls the quoted members and the `...SPREAD` references out of
// an `export const <name> = [ … ]` declaration. Deliberately a small textual
// parser rather than a TS toolchain dependency: the declarations it reads are
// flat literal arrays by convention, and a Go test that shells out to node
// would stop running wherever the frontend isn't installed.
func tsArrayDecl(t *testing.T, src, name string) (members, spreads []string) {
	t.Helper()
	open := "export const " + name + " = ["
	start := strings.Index(src, open)
	if start < 0 {
		t.Fatalf("types.ts has no `%s…]` declaration — the Go vocabulary needs a frontend mirror to check against", open)
	}
	body := src[start+len(open):]
	end := strings.Index(body, "]")
	if end < 0 {
		t.Fatalf("types.ts declaration %s is unterminated", name)
	}
	body = body[:end]
	for _, m := range tsQuotedMember.FindAllStringSubmatch(body, -1) {
		members = append(members, m[1])
	}
	for _, m := range tsSpreadMember.FindAllStringSubmatch(body, -1) {
		spreads = append(spreads, m[1])
	}
	return members, spreads
}

// vocabularyDiff reports set-difference both ways, or "" when the two agree.
// Order is not compared: these are sets, and the arrays read better grouped by
// meaning than sorted.
func vocabularyDiff(want, got []string) string {
	inGot := map[string]bool{}
	for _, s := range got {
		inGot[s] = true
	}
	inWant := map[string]bool{}
	for _, s := range want {
		inWant[s] = true
	}
	var lines []string
	for _, s := range want {
		if !inGot[s] {
			lines = append(lines, "  missing from the frontend: "+s)
		}
	}
	for _, s := range got {
		if !inWant[s] {
			lines = append(lines, "  frontend-only (Go never emits it): "+s)
		}
	}
	slices.Sort(lines)
	return strings.Join(lines, "\n")
}
