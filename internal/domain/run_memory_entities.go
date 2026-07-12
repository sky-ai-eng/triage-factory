package domain

// MemoryRole discriminators for run_memory_entities — free text, no CHECK
// constraint (house style: external_actions.action). "referenced" is
// reserved for a future eager trigger-referenced read set and is
// deliberately unused in v1.
const (
	MemoryRolePrimary  = "primary"
	MemoryRoleProduced = "produced"
	MemoryRoleTouched  = "touched"
)

// memoryRoleRank orders MemoryRole strings by precedence for the touch
// upsert: primary > produced > touched. An unrecognized role ranks below
// every known role so a garbage value never outranks (and therefore never
// overwrites) a row already carrying a real classification.
var memoryRoleRank = map[string]int{
	MemoryRolePrimary:  3,
	MemoryRoleProduced: 2,
	MemoryRoleTouched:  1,
}

// MemoryRoleOutranks reports whether candidate outranks current under the
// primary > produced > touched precedence — used by the touch upsert so a
// later stronger classification upgrades the stored row and a weaker one
// never downgrades it.
func MemoryRoleOutranks(candidate, current string) bool {
	return memoryRoleRank[candidate] > memoryRoleRank[current]
}
