package delegate

import (
	"context"
	"encoding/json"
	"slices"
	"strings"

	"github.com/sky-ai-eng/triage-factory/internal/agentprompt"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// A conversation's system text is two blocks: the shared framework prompt and
// the conversation's own block. The provider binds each thinking block to the
// system prompt it was produced under and refuses a request that replays one
// after the prompt changed, so a conversation keeps the text its first claim
// sent (domain.SystemText), and every later claim composes the text again and
// tells the conversation only what changed, as a system update.
//
// The comparison is part by part. Each framework block is a part, each
// <run_context> line, each source's verbs in <tools>, the mission and the
// step-ending note. A later claim composes again only the parts whose inputs
// move under a parked conversation: the framework blocks and the verbs a
// release ships, the step-ending note, the team's branch convention, the
// sources the run can use, and whether the task has closed or other work
// overlaps it. It carries every other part over from what the conversation
// was frozen with: the mission and the run's fixed facts are the step's, not
// today's, and a mission recomposed after a pull request rename would tell a
// conversation to do something nobody asked.

// Part keys. Framework blocks are keyed by agentprompt ("framework/<path>")
// and so are the sources' verbs ("tools/<source>").
const (
	partScope        = "run_context/scope"
	partTaskState    = "run_context/task_state"
	partSiblingWork  = "run_context/sibling_work"
	partRunRoot      = "run_context/run_root"
	partPRCheckout   = "run_context/pr_checkout"
	partBaseCheckout = "run_context/base_checkout"
	partBranch       = "run_context/branch"
	partRunURL       = "run_context/run_url"
	partSources      = "run_context/sources"
	partMission      = "mission"
	partStepEnd      = "step_end"
)

// runContextOrder is the order <run_context> renders its lines in.
var runContextOrder = []string{
	partScope, partTaskState, partSiblingWork, partRunRoot, partPRCheckout,
	partBaseCheckout, partBranch, partRunURL, partSources,
}

// carriedParts are the parts a later claim takes from what the conversation was
// frozen with rather than composing again.
var carriedParts = map[string]bool{
	partScope: true, partRunRoot: true, partPRCheckout: true, partBaseCheckout: true,
	partRunURL: true, partMission: true,
}

// systemInputs is what a claim composes a conversation's system text from.
type systemInputs struct {
	spec    agentprompt.Spec
	mission string

	scope, taskState, siblingWork, runRoot, prCheckout, baseCheckout, branchTemplate, runURL string

	sources sourceDocs
	stepEnd string
}

// composeSystemParts is one claim's composition of a conversation's system
// text, in render order: the framework blocks, the <run_context> lines, the
// sources' verbs, the mission, and the step-ending note. A part with nothing
// to say is left out.
func composeSystemParts(in systemInputs) []domain.SystemPart {
	var parts []domain.SystemPart
	for _, p := range agentprompt.BuildParts(in.spec) {
		parts = append(parts, domain.SystemPart(p))
	}
	parts = append(parts, runContextParts(in.scope, in.taskState, in.siblingWork, in.runRoot,
		in.prCheckout, in.baseCheckout, in.branchTemplate, in.runURL, in.sources.notes)...)
	for _, p := range in.sources.toolsParts(in.spec.Runtime) {
		parts = append(parts, domain.SystemPart(p))
	}
	if m := strings.TrimSpace(in.mission); m != "" {
		parts = append(parts, domain.SystemPart{Key: partMission, Name: "the mission", Text: m})
	}
	if e := strings.TrimSpace(in.stepEnd); e != "" {
		parts = append(parts, domain.SystemPart{Key: partStepEnd, Name: "the step-ending instructions", Text: e})
	}
	return parts
}

// renderSystem joins parts into the two blocks a request carries.
func renderSystem(parts []domain.SystemPart) domain.SystemText {
	var framework, lines []string
	var tools []agentprompt.Part
	var mission, stepEnd string
	for _, p := range parts {
		switch {
		case strings.HasPrefix(p.Key, "framework/"):
			framework = append(framework, p.Text)
		case strings.HasPrefix(p.Key, "run_context/"):
			lines = append(lines, p.Text)
		case strings.HasPrefix(p.Key, "tools/"):
			tools = append(tools, agentprompt.Part(p))
		case p.Key == partMission:
			mission = p.Text
		case p.Key == partStepEnd:
			stepEnd = p.Text
		}
	}
	toolTexts := make([]string, len(tools))
	for i, p := range tools {
		toolTexts[i] = p.Text
	}
	prompt := ""
	if len(framework) > 0 {
		prompt = strings.Join(framework, "\n\n") + "\n"
	}
	return domain.SystemText{
		Prompt: prompt,
		Block:  composeConversationSystemBlock(mission, renderRunContext(lines), strings.Join(toolTexts, "\n\n"), stepEnd),
		Parts:  parts,
	}
}

// carryOver replaces the carried parts of a claim's composition with the ones
// the conversation was frozen with, in render order.
func carryOver(composed, frozen []domain.SystemPart) []domain.SystemPart {
	out := make([]domain.SystemPart, 0, len(composed))
	for _, p := range composed {
		if !carriedParts[p.Key] {
			out = append(out, p)
		}
	}
	for _, p := range frozen {
		if carriedParts[p.Key] {
			out = append(out, p)
		}
	}
	slices.SortStableFunc(out, func(a, b domain.SystemPart) int { return partRank(a.Key) - partRank(b.Key) })
	return out
}

// partRank is a part's place in render order, by group: framework blocks, then
// <run_context> lines, the sources' verbs, the mission, the step-ending note.
// The sort is stable, so parts of one group keep the order composition gave
// them.
func partRank(key string) int {
	switch {
	case strings.HasPrefix(key, "framework/"):
		return 0
	case strings.HasPrefix(key, "run_context/"):
		return 100 + slices.Index(runContextOrder, key)
	case strings.HasPrefix(key, "tools/"):
		return 200
	case key == partMission:
		return 300
	default:
		return 400
	}
}

// systemUpdate is the message telling a conversation that last heard was what
// changed in now: each part that is new or different, in full, and one line
// for each part that no longer applies. "" when nothing changed.
func systemUpdate(was, now []domain.SystemPart) string {
	before := make(map[string]string, len(was))
	for _, p := range was {
		before[p.Key] = p.Text
	}
	current := make(map[string]bool, len(now))
	var changed, withdrawn []agentprompt.Part
	for _, p := range now {
		current[p.Key] = true
		if text, ok := before[p.Key]; !ok || text != p.Text {
			changed = append(changed, agentprompt.Part(p))
		}
	}
	for _, p := range was {
		if !current[p.Key] {
			withdrawn = append(withdrawn, agentprompt.Part(p))
		}
	}
	return agentprompt.InstructionsUpdate(changed, withdrawn)
}

// systemClaim is what one claim sends and tells a conversation.
type systemClaim struct {
	// send is the system text this claim's requests carry.
	send domain.SystemText
	// current is this claim's composition, which a compaction makes the
	// conversation's text.
	current domain.SystemText
	// update is the system update this claim appends, "" for none.
	update string
	// stamp is whether this claim records send as the conversation's text
	// before it launches.
	stamp bool
}

// resolveSystemClaim decides that from the conversation's frozen text, what it
// was last told (the parts of its newest system update still in its window,
// nil for none), and this claim's composition. opened is whether the
// conversation has run before.
func resolveSystemClaim(opened bool, frozen domain.SystemText, lastTold, composed []domain.SystemPart) systemClaim {
	if !opened {
		current := renderSystem(composed)
		return systemClaim{send: current, current: current, stamp: true}
	}
	if frozen.Parts == nil {
		// Opened before its parts were recorded. It keeps the block it was
		// launched with, which is all that was kept, and records this
		// claim's composition as its parts from here on.
		current := renderSystem(composed)
		send := current
		if frozen.Block != "" {
			send.Block = frozen.Block
		}
		return systemClaim{send: send, current: current, stamp: true}
	}
	composed = carryOver(composed, frozen.Parts)
	baseline := lastTold
	if baseline == nil {
		baseline = frozen.Parts
	}
	return systemClaim{
		send:    domain.SystemText{Prompt: frozen.Prompt, Block: frozen.Block, Parts: frozen.Parts},
		current: renderSystem(composed),
		update:  systemUpdate(baseline, composed),
	}
}

// lastToldParts is the parts of the newest system update in rows still in the
// conversation's window, queued or delivered, or nil when there is none. A
// compaction takes older ones out of the window and makes its own composition
// the conversation's text, so the frozen text is then the comparison point.
func lastToldParts(rows []domain.Message) []domain.SystemPart {
	for i := len(rows) - 1; i >= 0; i-- {
		r := rows[i]
		if r.Subtype != domain.MessageSubtypeInjectionSystemUpdate || r.WindowState == domain.MessageWindowInactive {
			continue
		}
		if parts, ok := decodeUpdateParts(r.Metadata[domain.SystemUpdateMetadataKey]); ok {
			return parts
		}
	}
	return nil
}

// decodeUpdateParts reads the parts a system update recorded, in the shape
// they were written (in-process) or the one a store hands back (decoded
// JSON).
func decodeUpdateParts(v any) ([]domain.SystemPart, bool) {
	if parts, ok := v.([]domain.SystemPart); ok {
		return parts, true
	}
	if v == nil {
		return nil, false
	}
	raw, err := json.Marshal(v)
	if err != nil {
		return nil, false
	}
	parts := []domain.SystemPart{}
	if err := json.Unmarshal(raw, &parts); err != nil {
		return nil, false
	}
	return parts, true
}

// readSystemState reads what a re-claim compares against: the conversation's
// frozen text and what it was last told.
func (s *Spawner) readSystemState(ctx context.Context, orgID, conversationID string) (domain.SystemText, []domain.SystemPart, error) {
	frozen, err := s.conversations.SystemTextSystem(ctx, orgID, conversationID)
	if err != nil {
		return domain.SystemText{}, nil, err
	}
	rows, err := s.conversations.ListForAssemblySystem(ctx, orgID, conversationID)
	if err != nil {
		return domain.SystemText{}, nil, err
	}
	return frozen, lastToldParts(rows), nil
}

// resumeSystemClaim is what an SDK message-resume sends and tells the
// conversation. The resume composes only the parts a re-claim composes again,
// because the inputs of the carried parts are a launch's; those come from what
// the conversation was frozen with. So does whether the step-ending note
// applies: that is the step's place in its plan, which never changes, and a
// read of the plan that failed must not withdraw the note.
//
// A resume never records the conversation's text. One whose parts were never
// recorded resumes on the block it was launched with, and the next launch that
// composes every part records them. A read that fails sends this claim's
// composition and records nothing, so a transient error cannot replace what
// the conversation was launched with.
func (s *Spawner) resumeSystemClaim(ctx context.Context, orgID string, conv *domain.Conversation, task domain.Task) systemClaim {
	ctx = context.WithoutCancel(ctx)
	frozen, lastTold, err := s.readSystemState(ctx, orgID, conv.ID)
	if err != nil {
		delegateLog.Warn("read the system text this conversation was launched with failed; this resume sends its own and records nothing",
			"conversation", conv.ID, "error", err)
	}
	stepEnd := ""
	if slices.ContainsFunc(frozen.Parts, func(p domain.SystemPart) bool { return p.Key == partStepEnd }) {
		stepEnd = agentprompt.NonTerminalCompletion()
	}
	composed := composeSystemParts(systemInputs{
		spec:           sdkSpec(surfaceFor(conv.Type)),
		taskState:      s.closedTaskContext(ctx, orgID, task.ID),
		siblingWork:    s.siblingWorkContext(ctx, orgID, conv.ID),
		branchTemplate: s.resolveBranchTemplate(ctx, task),
		sources:        s.sourceDocsFor(ctx, orgID, conv.CreatorUserID, conv.ID, task.EntitySource),
		stepEnd:        stepEnd,
	})
	if err != nil {
		current := renderSystem(composed)
		return systemClaim{send: current, current: current}
	}
	system := resolveSystemClaim(true, frozen, lastTold, composed)
	system.stamp = false
	return system
}
