package modelcatalog

import (
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
)

// SDKClaudeCode is the Claude Code SDK, the one agent harness TF drives as a
// subprocess. The id is the key sdk_models.json files its list under.
const SDKClaudeCode = "claude-code"

//go:embed sdk_models.json
var sdkModelsJSON []byte

// sdkModel is one row of sdk_models.json. Two fields, and no third: an SDK
// alias names no provider and joins no datasheet, so there is nothing here for
// a price, a window or a vendor to be written into.
type sdkModel struct {
	Key string `json:"key"`
	// DisplayName renders verbatim. It carries no version, because the alias
	// carries none either — the SDK resolves it to whichever model currently
	// heads that family, and a name promising a version would be a claim TF
	// cannot keep across a vendor release.
	DisplayName string `json:"display_name"`
}

var (
	sdkModels  map[string][]Model
	sdkLoadErr error
)

func init() {
	sdkModels, sdkLoadErr = loadSDKModels(sdkModelsJSON)
}

// SDKModels returns the models sdk names, in display order, and an empty slice
// for an SDK this build carries no list for — never nil, so a caller ranging
// over the result and one checking its length read the same thing. The slice is
// freshly copied per call, for the same reason Entries is: it is read on an API
// request path.
func SDKModels(sdk string) []Model {
	src := sdkModels[sdk]
	out := make([]Model, len(src))
	copy(out, src)
	return out
}

// SDKs returns the SDK ids this build carries lists for, sorted.
func SDKs() []string {
	out := make([]string, 0, len(sdkModels))
	for id := range sdkModels {
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}

// SDKLoadError names the rows sdk_models.json could not be read into a list.
// They are absent from SDKModels; this is how a caller learns they were dropped
// rather than never named. The file is compiled in, so the answer is settled
// when the binary is linked — the same standing as JoinError, and it is checked
// beside it at boot.
func SDKLoadError() error { return sdkLoadErr }

// loadSDKModels parses the per-SDK lists. Rows that do not resolve are omitted
// and named in the returned error — every one of them, so an author fixing the
// file sees the whole list rather than one row per build.
//
// There is no join. The whole point of a per-SDK list is that the harness owns
// what the alias resolves to, so this validates the shape and nothing else: two
// non-empty fields, and no key repeated inside one SDK's list. Keys are NOT
// checked for uniqueness across SDKs — TF does not control those vocabularies,
// and two harnesses spelling a model the same way is their business, not a
// conflict.
func loadSDKModels(fileJSON []byte) (map[string][]Model, error) {
	var file map[string][]sdkModel
	if err := json.Unmarshal(fileJSON, &file); err != nil {
		return nil, fmt.Errorf("modelcatalog: parse sdk_models.json: %w", err)
	}

	out := make(map[string][]Model, len(file))
	var problems []error
	for _, sdk := range sortedKeys(file) {
		rows := file[sdk]
		if sdk == "" {
			problems = append(problems, errors.New("empty sdk id"))
			continue
		}
		models := make([]Model, 0, len(rows))
		seen := make(map[string]bool, len(rows))
		for i, row := range rows {
			switch {
			case row.Key == "":
				problems = append(problems, fmt.Errorf("%s: entry %d: empty key", sdk, i))
				continue
			case row.DisplayName == "":
				problems = append(problems, fmt.Errorf("%s: %s: empty display_name", sdk, row.Key))
				continue
			case seen[row.Key]:
				// Two rows for one key would give it two display names and two
				// positions, and which one wins would be an accident of order.
				problems = append(problems, fmt.Errorf("%s: %s: duplicate key", sdk, row.Key))
				continue
			}
			seen[row.Key] = true
			models = append(models, Model{
				Key:         row.Key,
				DisplayName: row.DisplayName,
				// Position in the built slice, not in the file: a dropped row
				// must not leave a hole in the ordering the API publishes.
				DisplayOrder: len(models),
			})
		}
		if len(models) == 0 {
			// An SDK with no usable rows is an SDK nothing can be picked for,
			// and publishing an empty universe for it would read as "this
			// harness offers no models" rather than as the file being wrong.
			problems = append(problems, fmt.Errorf("%s: names no usable model", sdk))
			continue
		}
		out[sdk] = models
	}
	if len(problems) > 0 {
		return out, fmt.Errorf("modelcatalog: %d sdk model row(s) unusable: %w", len(problems), errors.Join(problems...))
	}
	return out, nil
}

// sortedKeys orders a map's keys so the load reports its problems in a stable
// order rather than in Go's randomized map order.
func sortedKeys(m map[string][]sdkModel) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
