package inference

import (
	"encoding/json"
	"sync"

	"github.com/maximhq/bifrost/core/schemas"
)

// bifrost shapes each request from a per-model capability record: whether the
// model takes adaptive thinking and an output_config.effort, whether it
// refuses sampling parameters, its output ceiling, and the rest of what
// schemas.ModelCapabilities names. It reads the record through a resolver the
// application installs, and with none it guesses from the model's name, which
// trails the models TF offers.
//
// The embedded datasheet is the upstream file bifrost's own catalog is built
// from, under the same keys and field names, so each row decodes into
// bifrost's record as written. Installing it here means the datasheet TF
// prices and describes a model by is also what bifrost shapes that model's
// requests by.
func init() {
	schemas.SetCapabilityResolver(capabilitiesFor)
}

var (
	capabilitiesOnce  sync.Once
	capabilitiesTable map[string]*schemas.ModelCapabilities
)

// loadCapabilities decodes every datasheet row into bifrost's record once. A
// datasheet that fails to parse yields no table, and PricingLoadError reports
// the same failure. A single row bifrost's record cannot read is left out, so
// bifrost falls back to its own detection for that model rather than reading
// a half-decoded record.
func loadCapabilities() map[string]*schemas.ModelCapabilities {
	capabilitiesOnce.Do(func() {
		var rows map[string]json.RawMessage
		if err := json.Unmarshal(pricingDatasheetJSON, &rows); err != nil {
			return
		}
		table := make(map[string]*schemas.ModelCapabilities, len(rows))
		for key, row := range rows {
			var c schemas.ModelCapabilities
			if err := json.Unmarshal(row, &c); err != nil {
				continue
			}
			table[key] = &c
		}
		capabilitiesTable = table
	})
	return capabilitiesTable
}

// capabilitiesFor is the resolver bifrost calls: the datasheet row for model,
// found as pricing finds it (lookupDatasheetRow), or nil, which bifrost reads
// as a model the datasheet says nothing about. The datasheet's keys already
// separate providers (a Bedrock id is its own key), so the provider is not
// consulted. bifrost only reads the record it is handed.
func capabilitiesFor(_ schemas.ModelProvider, model string) *schemas.ModelCapabilities {
	c, ok := lookupDatasheetRow(loadCapabilities(), model)
	if !ok {
		return nil
	}
	return c
}
