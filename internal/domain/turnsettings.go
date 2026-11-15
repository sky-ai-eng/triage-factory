package domain

import (
	"slices"
	"time"
)

// The reasoning efforts a person's message may ask for. TF uses Anthropic's
// five levels as its own vocabulary on every runtime, and a model takes the
// subset its catalog entry lists. The values are the wire and stored
// spelling.
const (
	EffortLow    = "low"
	EffortMedium = "medium"
	EffortHigh   = "high"
	EffortXHigh  = "xhigh"
	EffortMax    = "max"
)

// EffortLevels is every effort TF knows, lowest first. A model's own list is
// a subset in this order.
var EffortLevels = []string{EffortLow, EffortMedium, EffortHigh, EffortXHigh, EffortMax}

// ValidEffort reports whether effort is one of EffortLevels. Whether a given
// model takes it is a separate question, answered from the model's catalog
// entry.
func ValidEffort(effort string) bool {
	return slices.Contains(EffortLevels, effort)
}

// TurnSettings are the model and the reasoning effort one message runs on.
//
// On the way in, from the message route, an empty field means the message did
// not give it. Once resolved, Model names a model and an empty Effort means
// the provider's default.
type TurnSettings struct {
	Model  string
	Effort string
}

// ProviderCacheTTL is how long a provider keeps a prompt cache warm after the
// call that wrote it: the 5-minute ephemeral TTL the stamping policy leaves
// unset (inference/cachecontrol.go). Warmth is read from the newest active
// assistant row's age against it and never stored.
const ProviderCacheTTL = 5 * time.Minute

// PromptOccupancy is an assistant row's whole-prompt token accounting: input,
// output, cache read and cache creation, which is approximately what the next
// call's prompt costs. It reads only the usage the row was stamped with, and
// a row with none answers 0.
func PromptOccupancy(m Message) int {
	total := 0
	for _, p := range []*int{m.InputTokens, m.OutputTokens, m.CacheReadTokens, m.CacheCreationTokens} {
		if p != nil {
			total += *p
		}
	}
	return total
}
