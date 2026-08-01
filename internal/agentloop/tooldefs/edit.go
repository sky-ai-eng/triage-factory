package tooldefs

// Edit applies exact text replacements. The uniqueness and non-overlap rules
// are stated at length in the descriptions because a model that batches
// overlapping edits corrupts a file in a way no later read can explain.
//
// Implemented by the harness in the jail; the loop only forwards the call.
var Edit = Tool{
	Name:        "edit",
	Description: "Edit a single file using exact text replacement. Every edits[].oldText must match a unique, non-overlapping region of the original file. If two changes affect the same block or nearby lines, merge them into one edit instead of emitting overlapping edits. Do not include large unchanged regions just to connect distant changes.",
	Params: obj([]string{"path", "edits"},
		str("path", "Path to the file to edit (relative or absolute)"),
		Field{Name: "edits", Schema: Schema{
			Type:        "array",
			Description: "One or more targeted replacements. Each edit is matched against the original file, not incrementally. Do not include overlapping or nested edits. If two changes touch the same block or nearby lines, merge them into one edit instead.",
			Items: &Schema{
				Type: "object",
				Fields: []Field{
					str("oldText", "Exact text for one targeted replacement. It must be unique in the original file and must not overlap with any other edits[].oldText in the same call."),
					str("newText", "Replacement text for this targeted edit."),
				},
				Required: []string{"oldText", "newText"},
			},
		}},
	),
}
