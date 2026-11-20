// A conversation's system blocks, as the parts a re-claim compares.
//
// A conversation keeps the two system blocks its first claim sent, the shared
// framework prompt and its own block, because the model provider binds every
// thinking block to the system prompt it was produced under and refuses a
// request that replays one under another. Every claim still composes both
// blocks from today's inputs, and when they differ from what the conversation
// last received, the claim adds a system message carrying only what changed.
// Compared part by part: each framework block file, each <run_context> line,
// each source's verb reference and the step-ending note.

package delegate

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/sky-ai-eng/triage-factory/internal/agentloop"
	"github.com/sky-ai-eng/triage-factory/internal/agentproc"
	"github.com/sky-ai-eng/triage-factory/internal/agentprompt"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/eventing/eventsource"
	"github.com/sky-ai-eng/triage-factory/internal/sandbox"
)

// The keys of a conversation block's parts. Stored rows carry them, so they
// cannot be renamed.
const (
	partScope            = "run_context.scope"
	partTaskState        = "run_context.task_state"
	partSiblingWork      = "run_context.sibling_work"
	partRunRoot          = "run_context.run_root"
	partPRCheckout       = "run_context.pr_checkout"
	partBaseCheckout     = "run_context.base_checkout"
	partBranchConvention = "run_context.branch_convention"
	partRunURL           = "run_context.run_url"
	partSourceNotes      = "run_context.source_notes"
	partKnowledge        = "run_context.knowledge"
	partStepEnd          = "step_end"

	// partToolsPrefix leads the key of one source's verb reference, followed
	// by the source kind or agentprompt.ToolsWorkspaceKey.
	partToolsPrefix = "tools."
)

// partNames are the conversation block's parts as an update names them.
var partNames = map[string]string{
	partScope:            "the run context's scope",
	partTaskState:        "the run context's task-state line",
	partSiblingWork:      "the run context's list of overlapping work",
	partRunRoot:          "the run context's run root",
	partPRCheckout:       "the run context's PR checkout",
	partBaseCheckout:     "the run context's base-branch checkout",
	partBranchConvention: "the run context's branch naming convention",
	partRunURL:           "the run context's run URL",
	partSourceNotes:      "the run context's notes on sources",
	partKnowledge:        "the run context's knowledge listing",
	partStepEnd:          "the step-ending note",
}

// toolsSourceNames are the sources an update names by something other than
// their kind with a capital letter.
var toolsSourceNames = map[string]string{
	"github":                      "GitHub",
	agentprompt.ToolsWorkspaceKey: "workspace",
}

// systemComposition is what one claim composes a conversation's two system
// blocks from.
type systemComposition struct {
	// spec selects the shared framework prompt.
	spec agentprompt.Spec
	// mission is the step's instruction. It is built from the plan the
	// blueprint run froze at its start, so it is never compared: every claim
	// composes the same one.
	mission    string
	runContext []domain.SystemPart
	tools      []agentprompt.ToolsPart
	// stepEnd is the step-ending note, "" on a final step.
	stepEnd string
}

// blocks renders the composition: the two blocks as they are sent, and the
// parts they were rendered from.
func (c systemComposition) blocks() domain.SystemBlocks {
	tools := make([]string, 0, len(c.tools))
	for _, t := range c.tools {
		tools = append(tools, t.Text)
	}
	return domain.SystemBlocks{
		Prompt: agentprompt.Build(c.spec),
		Block:  composeConversationSystemBlock(c.mission, renderRunContext(c.runContext), strings.Join(tools, "\n\n"), c.stepEnd),
		Parts:  c.parts(),
	}
}

// parts is the composition as the pieces an update compares, in the order
// each block renders them.
func (c systemComposition) parts() *domain.SystemParts {
	sections := agentprompt.Sections(c.spec)
	parts := &domain.SystemParts{Shared: make([]domain.SystemPart, 0, len(sections))}
	for _, sec := range sections {
		parts.Shared = append(parts.Shared, domain.SystemPart{Key: sec.Path, Text: sec.Text})
	}
	parts.Conversation = append(parts.Conversation, c.runContext...)
	for _, t := range c.tools {
		if text := strings.TrimSpace(t.Text); text != "" {
			parts.Conversation = append(parts.Conversation, domain.SystemPart{Key: partToolsPrefix + t.Key, Text: text})
		}
	}
	if text := strings.TrimSpace(c.stepEnd); text != "" {
		parts.Conversation = append(parts.Conversation, domain.SystemPart{Key: partStepEnd, Text: text})
	}
	return parts
}

// claimSystem is the system prompt one claim sends, and what it tells the
// conversation about how that prompt changed.
type claimSystem struct {
	// sent is what every call of the claim sends until a compaction commits.
	sent domain.SystemBlocks
	// current is what the claim composed from today's inputs, which a
	// compaction makes the conversation's.
	current domain.SystemBlocks
	// stamp is whether the claim saves sent on the row before the agent
	// starts: the launch, and a conversation whose row predates the parts.
	stamp bool
	// update is the system message's text, "" when nothing changed.
	update string
}

// updateMetadata is what the update row records: the parts current was
// composed from, which the next claim compares with.
func (c claimSystem) updateMetadata() map[string]any {
	return map[string]any{domain.MessageMetadataSystemParts: c.current.Parts}
}

// sdkSystemUpdate is the update as an SDK resume records it, nil for none: the
// row the transcript shows, whose content is the text the resume's opening
// input carries, with every verb pointed at the binary this launch runs
// (resolveCLIPath), as the append's are.
func sdkSystemUpdate(c claimSystem, binaryPath string) *domain.Message {
	if c.update == "" {
		return nil
	}
	delivered := true
	return &domain.Message{
		Role:      "system",
		Subtype:   domain.MessageSubtypeSystemUpdate,
		Content:   resolveCLIPath(c.update, binaryPath),
		Metadata:  c.updateMetadata(),
		Delivered: &delivered,
	}
}

// systemUpdate is the update as the native engine writes it, nil for none.
func (c claimSystem) systemUpdate() *agentloop.SystemUpdate {
	if c.update == "" {
		return nil
	}
	return &agentloop.SystemUpdate{Content: c.update, Metadata: c.updateMetadata()}
}

// resolveClaimSystem decides what a claim sends, given whether its
// conversation has been opened, what its row holds (readErr when either read
// that answers those failed) and what the claim composed.
//
//   - A launch sends what it composed and saves it.
//   - A conversation that has been opened sends what its row holds, and tells
//     the conversation what changed since the newest of the saved parts and
//     its last update.
//   - An opened row with no parts is an SDK conversation an earlier release
//     saved, or one whose first claim's save did not land. It is sent the
//     shared prompt this claim composed and the block it holds (or the
//     composed one, when it holds none), and those are saved with the
//     composed parts, so the next claim can compare.
//   - A row this claim could not read is sent what the claim composed and is
//     left alone: it may hold the first claim's blocks, and a transient
//     failure must not overwrite them.
func (s *Spawner) resolveClaimSystem(ctx context.Context, orgID, conversationID string, opened bool, saved domain.SystemBlocks, readErr error, current domain.SystemBlocks) claimSystem {
	switch {
	case readErr != nil:
		return claimSystem{sent: current, current: current}
	case !opened:
		return claimSystem{sent: current, current: current, stamp: true}
	case saved.Parts == nil:
		sent := domain.SystemBlocks{Prompt: saved.Prompt, Block: saved.Block, Parts: current.Parts}
		if sent.Prompt == "" {
			sent.Prompt = current.Prompt
		}
		if sent.Block == "" {
			sent.Block = current.Block
		}
		return claimSystem{sent: sent, current: current, stamp: true}
	}
	last := s.lastReceivedParts(ctx, orgID, conversationID, saved.Parts)
	return claimSystem{sent: saved, current: current, update: systemUpdateText(last, current.Parts)}
}

// lastReceivedParts is what a conversation was last told its system blocks
// were made of: the parts its newest system update recorded, or else the ones
// saved with its blocks. A failed read answers the saved ones, which at worst
// tells the conversation again what an update already did.
func (s *Spawner) lastReceivedParts(ctx context.Context, orgID, conversationID string, saved *domain.SystemParts) *domain.SystemParts {
	row, err := s.conversations.LatestSystemUpdateSystem(ctx, orgID, conversationID)
	if err != nil {
		delegateLog.Warn("read the conversation's newest system update failed; comparing with its saved blocks",
			"conversation", conversationID, "error", err)
		return saved
	}
	if row == nil {
		return saved
	}
	parts, ok := partsFromMetadata(row.Metadata)
	if !ok {
		delegateLog.Warn("the conversation's newest system update records no parts; comparing with its saved blocks",
			"conversation", conversationID, "message", row.ID)
		return saved
	}
	return parts
}

// partsFromMetadata reads the parts a system-update row recorded. Read back
// from the store they are decoded JSON; written in this process they are the
// value itself. Both go through JSON so there is one decoding.
func partsFromMetadata(metadata map[string]any) (*domain.SystemParts, bool) {
	raw, ok := metadata[domain.MessageMetadataSystemParts]
	if !ok || raw == nil {
		return nil, false
	}
	b, err := json.Marshal(raw)
	if err != nil {
		return nil, false
	}
	var parts domain.SystemParts
	if err := json.Unmarshal(b, &parts); err != nil {
		return nil, false
	}
	return &parts, true
}

// systemUpdateText is the system message that brings a conversation from last
// to now, "" when the two are the same part for part.
//
// It carries only what changed. A part whose text differs, or that is new, is
// given in its current text, which replaces the earlier version of the part;
// a part that is gone is named in one line. A source's verb reference is a
// part like any other, so a source whose verbs became available arrives with
// its whole reference and one whose verbs went away is one line naming it.
func systemUpdateText(last, now *domain.SystemParts) string {
	if last == nil || now == nil {
		return ""
	}
	var given, gone []string
	diff := func(before, after []domain.SystemPart, name func(string) string) {
		was := make(map[string]string, len(before))
		for _, p := range before {
			was[p.Key] = p.Text
		}
		is := make(map[string]bool, len(after))
		for _, p := range after {
			is[p.Key] = true
			if old, had := was[p.Key]; !had || old != p.Text {
				given = append(given, updatePart(name(p.Key), p.Text, had))
			}
		}
		for _, p := range before {
			if !is[p.Key] {
				gone = append(gone, goneLine(p.Key, name(p.Key)))
			}
		}
	}
	diff(last.Shared, now.Shared, sharedPartName)
	diff(last.Conversation, now.Conversation, conversationPartName)
	if len(given) == 0 && len(gone) == 0 {
		return ""
	}
	sections := []string{"Your system prompt has changed since this conversation last received it. " +
		"Each part below is given in its current text and takes the place of the earlier version of that part. " +
		"Every part not named here is unchanged."}
	sections = append(sections, given...)
	if len(gone) > 0 {
		sections = append(sections, strings.Join(gone, "\n"))
	}
	return "<system-update>\n" + strings.Join(sections, "\n\n") + "\n</system-update>"
}

// updatePart is how an update gives a part in its current text: replacing the
// earlier version of it, or new.
func updatePart(name, text string, replaces bool) string {
	change := "new"
	if replaces {
		change = "replaces the earlier version"
	}
	return fmt.Sprintf("<part name=%q change=%q>\n%s\n</part>", name, change, text)
}

// goneLine is the one line an update gives a part that no longer applies.
func goneLine(key, name string) string {
	if strings.HasPrefix(key, partToolsPrefix) {
		return capitalize(name) + " are no longer available to this run; do not use them."
	}
	return capitalize(name) + " no longer applies."
}

func sharedPartName(path string) string {
	return "the framework section " + strings.TrimSuffix(path, ".txt")
}

func conversationPartName(key string) string {
	if source, ok := strings.CutPrefix(key, partToolsPrefix); ok {
		name, known := toolsSourceNames[source]
		if !known {
			name = capitalize(source)
		}
		return "the " + name + " verbs"
	}
	if name, ok := partNames[key]; ok {
		return name
	}
	return key
}

func capitalize(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}

// saveSystemBlocksHook is the engine's SaveSystemBlocks for a native
// engagement: a compaction committed, so the blocks the claim composed become
// the conversation's, saved with the parts they were composed from.
func (s *Spawner) saveSystemBlocksHook(orgID, conversationID, claimID string, current domain.SystemBlocks) func(context.Context, agentloop.SystemBlocks) error {
	return func(ctx context.Context, blocks agentloop.SystemBlocks) error {
		saved := domain.SystemBlocks{Prompt: blocks.Prompt, Block: blocks.Addendum, Parts: current.Parts}
		_, err := s.conversations.SetSystemBlocksForClaimSystem(context.WithoutCancel(ctx), orgID, conversationID, claimID, saved)
		return err
	}
}

// runContextOrder is the order the run-context parts render in, which is the
// order runContextLines writes them.
var runContextOrder = []string{
	partScope, partTaskState, partSiblingWork, partRunRoot, partPRCheckout,
	partBaseCheckout, partBranchConvention, partRunURL, partSourceNotes, partKnowledge,
}

// withPartsFrom is lines with the parts keys names taken from base instead:
// each such part base has, and none it lacks, in its place in the order the
// run context renders.
func withPartsFrom(lines []domain.SystemPart, base []domain.SystemPart, keys ...string) []domain.SystemPart {
	from := make(map[string]bool, len(keys))
	for _, k := range keys {
		from[k] = true
	}
	byKey := map[string]domain.SystemPart{}
	for _, p := range lines {
		if !from[p.Key] {
			byKey[p.Key] = p
		}
	}
	for _, p := range base {
		if from[p.Key] {
			byKey[p.Key] = p
		}
	}
	out := make([]domain.SystemPart, 0, len(byKey))
	for _, k := range runContextOrder {
		if p, ok := byKey[k]; ok {
			out = append(out, p)
		}
	}
	return out
}

// resumeClaimSystem is what an SDK resume of a parked conversation sends: the
// blocks the session was started with, and the update that brings it to
// today's.
//
// The resume has no setup of its own, so it recomposes what it can read here
// — the framework blocks, the task state, the overlapping work, the run root,
// the branch convention, the run URL, the sources and their verbs, the
// knowledge the tree holds, and the step-ending note — and carries the parts
// only a setup knows (the scope and the checkouts) over from what the
// conversation last received. The mission is never compared. finalStep is
// whether the conversation is its blueprint's last step, and finalRead false
// when the blueprint run could not be read to say: the step-ending note is
// then carried over too, since a guess either way would tell the session
// something about its step that nothing established.
//
// A row this resume cannot read sends the framework blocks alone, with no
// update, which is what a resume of a conversation that saved no block sends.
// A row from before the parts were saved is sent what it holds, and saved with
// the parts this resume composed, so the next claim can compare.
func (s *Spawner) resumeClaimSystem(ctx context.Context, conv *domain.Conversation, task domain.Task, cwd string, finalStep, finalRead bool) claimSystem {
	orgID := conv.OrgID
	spec := sdkSpec(surfaceFor(conv.Type))
	saved, err := s.conversations.SystemBlocksSystem(ctx, orgID, conv.ID)
	if err != nil {
		delegateLog.Warn("read the conversation's system blocks failed; this turn resumes on the framework blocks alone",
			"conversation", conv.ID, "org_id", orgID, "error", err)
		sent := domain.SystemBlocks{Prompt: agentprompt.Build(spec)}
		return claimSystem{sent: sent, current: sent}
	}

	var base *domain.SystemParts
	if saved.Parts != nil {
		base = s.lastReceivedParts(ctx, orgID, conv.ID, saved.Parts)
	}
	withStepEnd := !finalStep
	if !finalRead {
		withStepEnd = hasPart(base, partStepEnd)
	}
	stepEnd := ""
	if conv.BlueprintRunID != "" && withStepEnd {
		stepEnd = agentprompt.NonTerminalCompletion()
	}
	sources := s.sourceDocsFor(ctx, orgID, conv.CreatorUserID, conv.ID, ownSourceKind(task))
	knowledge := s.knowledgeForClaim(ctx, orgID, conv.TeamID, cwd, sandbox.RunTreeHandedOff(cwd))
	lines := runContextLines("", s.closedTaskContext(context.WithoutCancel(ctx), orgID, task.ID),
		s.siblingWorkContext(context.WithoutCancel(ctx), orgID, conv.ID), agentproc.AgentVisibleRoot(cwd), "", "",
		s.resolveBranchTemplate(ctx, task), s.runURLFor(orgID, conv.ID), sources.notes, knowledge)
	if base != nil {
		lines = withPartsFrom(lines, base.Conversation, partScope, partPRCheckout, partBaseCheckout)
	}
	current := domain.SystemBlocks{
		Prompt: agentprompt.Build(spec),
		Parts: systemComposition{
			spec:       spec,
			runContext: lines,
			tools:      sources.toolParts(agentprompt.RuntimeSDK),
			stepEnd:    stepEnd,
		}.parts(),
	}

	if saved.Parts == nil {
		sent := domain.SystemBlocks{Prompt: saved.Prompt, Block: saved.Block, Parts: current.Parts}
		if sent.Prompt == "" {
			sent.Prompt = current.Prompt
		}
		s.persistSystemBlocks(ctx, orgID, conv.ID, conv.ClaimID, sent)
		return claimSystem{sent: sent, current: current}
	}
	return claimSystem{sent: saved, current: current, update: systemUpdateText(base, current.Parts)}
}

// hasPart reports whether parts holds a conversation part keyed key.
func hasPart(parts *domain.SystemParts, key string) bool {
	if parts == nil {
		return false
	}
	for _, p := range parts.Conversation {
		if p.Key == key {
			return true
		}
	}
	return false
}

// ownSourceKind is the source a task's own entity is in, which sourceDocsFor
// documents when it cannot read the run's source manifest.
func ownSourceKind(task domain.Task) string {
	switch task.EntitySource {
	case "jira":
		return eventsource.KindJira
	case "linear":
		return eventsource.KindLinear
	case "slack":
		return "slack"
	}
	return eventsource.KindGitHub
}
