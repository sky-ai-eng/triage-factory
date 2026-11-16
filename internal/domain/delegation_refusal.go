package domain

// Why a delegation's plan refused it, as delegate.RefusalCause classifies the
// refusal. Each cause is a standing condition: the same firing planned again
// is refused the same way until a setting changes or, for a spend cap, the UTC
// day rolls over. A trigger's firing that meets one is dropped rather than
// retried, and the trigger's team admins are told which.
const (
	// DelegationRefusedSpendCap: the org's or the run team's daily spend cap
	// is reached.
	DelegationRefusedSpendCap = "spend_cap"
	// DelegationRefusedNoDefaultModel: the run's team has picked no default
	// model.
	DelegationRefusedNoDefaultModel = "no_default_model"
	// DelegationRefusedProviderNotConnected: the org has not connected the
	// provider of a model the run would use, or has connected no provider at
	// all.
	DelegationRefusedProviderNotConnected = "provider_not_connected"
	// DelegationRefusedModelNotEnabled: a model the run would use, the team's
	// default or a step's pin, is not in the team's enabled models.
	DelegationRefusedModelNotEnabled = "model_not_enabled"
	// DelegationRefusedBlueprintUnusable: the blueprint is missing or has no
	// steps, or a step's prompt is missing.
	DelegationRefusedBlueprintUnusable = "blueprint_unusable"
)
