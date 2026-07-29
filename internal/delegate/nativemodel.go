package delegate

// nativeWireModel maps the stored CLI-alias vocabulary ("haiku" | "sonnet" |
// "opus") onto a concrete model id at the native dispatch boundary. The
// stored value is a Claude Code alias, which the raw provider API does not
// accept — and the translation must happen exactly once, before the value
// fans out, so the wire request, the key whitelist, and everything the loop
// logs or records agree on one concrete id. History then holds what actually
// ran, and this map moving later changes only future dispatches.
//
// Transitional by design: once stored configuration carries concrete ids,
// every input takes the passthrough arm and this function deletes cleanly.
// It lives here rather than in internal/inference because that package takes
// models as plain inputs and owns no vocabulary.
//
// haiku pins the dated id to stay aligned with the system jobs' pin
// (ai.SystemJobModelDirect); sonnet and opus pin the undated current ids,
// matching what a user choosing those words today means.
func nativeWireModel(model string) string {
	switch model {
	case "haiku":
		return "claude-haiku-4-5-20251001"
	case "sonnet":
		return "claude-sonnet-5"
	case "opus":
		return "claude-opus-5"
	}
	return model
}
