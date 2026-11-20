package domain

// SystemBlocks is the system prompt a conversation's claims send, as saved on
// its row: the shared framework prompt (block 1) and the conversation's own
// block (block 2), exactly as a claim sent them, and the parts the two were
// composed from.
//
// The first claim saves them, and every later claim sends them back unchanged,
// because the model provider binds each thinking block to the system prompt it
// was produced under. A later change to either block reaches the conversation
// as a system-update row instead (MessageSubtypeSystemUpdate), and the parts
// are what the next claim compares its own composition with.
type SystemBlocks struct {
	// Prompt is block 1, conversations.system_prompt. Empty on a row saved
	// before the shared prompt was saved.
	Prompt string
	// Block is block 2, conversations.system_block. The empty string is a
	// value: every section of the block is optional.
	Block string
	// Parts are the parts Prompt and Block were composed from,
	// conversations.system_parts. Nil on a row saved before parts were
	// recorded, which leaves a claim nothing to compare with.
	Parts *SystemParts
}

// SystemParts is a conversation's two system blocks as the pieces a re-claim
// compares one by one: each framework block file of the shared prompt, and
// each line, verb reference and note of the conversation's own block. Each
// list is in the order its block renders it.
//
// Stored as JSON on the conversation row and on every system-update row, so
// the field names are fixed.
type SystemParts struct {
	Shared       []SystemPart `json:"shared"`
	Conversation []SystemPart `json:"conversation"`
}

// SystemPart is one piece of a system block: a stable key naming what it is
// (a block file's path, a run-context line's name, a source's verb
// reference) and its text.
type SystemPart struct {
	Key  string `json:"key"`
	Text string `json:"text"`
}

// MessageSubtypeSystemUpdate marks a role=system row a re-claim writes when
// the system blocks it composed differ from the ones the conversation last
// received. Its content names each part that changed, with its current text,
// and each part that no longer applies. The native loop sends it with role
// "system" after the claim's input; on the SDK runtime it is the record of
// the text the resume's opening input carried.
const MessageSubtypeSystemUpdate = "system-update"

// MessageMetadataSystemParts is the Message.Metadata key under which a
// system-update row records the parts the conversation's blocks were composed
// from at that claim (a SystemParts), which is what the next claim compares
// with. Stored rows carry this key, so it cannot be renamed.
const MessageMetadataSystemParts = "system_parts"
