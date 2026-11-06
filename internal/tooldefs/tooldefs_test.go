package tooldefs

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestTFTools_OfferFollowsTheBlueprint pins the one decision about which TF
// tools a run gets, and that TFTool agrees with it: a name resolves only where
// the tool was offered, and never for a name nothing defines.
func TestTFTools_OfferFollowsTheBlueprint(t *testing.T) {
	tests := []struct {
		name  string
		facts Facts
		want  []string
	}{
		{"with a blueprint", Facts{HasBlueprint: true}, []string{StopBlueprintName}},
		{"without a blueprint", Facts{}, nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var got []string
			for _, tool := range TFTools(tc.facts) {
				if tool.Handler == nil {
					t.Errorf("%s is offered as a TF tool with no handler", tool.Name)
				}
				got = append(got, tool.Name)
			}
			if strings.Join(got, ",") != strings.Join(tc.want, ",") {
				t.Fatalf("TFTools = %v, want %v", got, tc.want)
			}

			_, ok := TFTool(StopBlueprintName, tc.facts)
			if want := len(tc.want) > 0; ok != want {
				t.Errorf("TFTool(%s) resolved = %v, want %v: a name must resolve exactly where it is offered", StopBlueprintName, ok, want)
			}
			if _, ok := TFTool("no_such_tool", tc.facts); ok {
				t.Error("an unknown name resolved to a TF tool")
			}
			if _, ok := TFTool(Bash.Name, tc.facts); ok {
				t.Error("a sandbox tool resolved as a TF tool")
			}
		})
	}
}

// TestJSONSchema_RendersThePlainDocument pins what an SDK run's MCP server is
// built from: a JSON Schema object whose properties keep declaration order and
// whose required list, enums and minimum lengths survive.
//
// Every registered tool is rendered, because a schema that cannot render
// panics at launch.
func TestJSONSchema_RendersThePlainDocument(t *testing.T) {
	for _, tool := range append(Sandbox(), StopBlueprint) {
		if !json.Valid(tool.Params.JSONSchema()) {
			t.Errorf("%s's schema rendered invalid JSON", tool.Name)
		}
	}

	raw := StopBlueprint.Params.JSONSchema()
	var doc struct {
		Type       string                     `json:"type"`
		Properties map[string]json.RawMessage `json:"properties"`
		Required   []string                   `json:"required"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("not JSON: %v\n%s", err, raw)
	}
	if doc.Type != "object" || len(doc.Properties) != 3 || strings.Join(doc.Required, ",") != "type,reason,summary" {
		t.Fatalf("schema = %s, want an object with three properties, all required", raw)
	}
	if !strings.Contains(string(doc.Properties["type"]), `"enum":["finish","abort"]`) {
		t.Errorf("type property lost its enum: %s", doc.Properties["type"])
	}
	s := string(raw)
	if strings.Index(s, `"type":{`) > strings.Index(s, `"reason":{`) || strings.Index(s, `"reason":{`) > strings.Index(s, `"summary":{`) {
		t.Errorf("properties are not in declaration order: %s", raw)
	}

	bash := Bash.Params.JSONSchema()
	if !strings.Contains(string(bash), `"minLength":5`) {
		t.Errorf("bash's description minimum length is missing: %s", bash)
	}
}
