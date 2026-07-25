package tooldefs

// Ls lists a directory. The only tool with no required argument: an
// agent that knows nothing about the workspace can still call it.
//
// Implemented by the harness in the jail; the loop only forwards the call.
var Ls = Tool{
	Name:        "ls",
	Description: "List directory contents. Returns entries sorted alphabetically, with '/' suffix for directories. Includes dotfiles. Output is truncated to 500 entries or 50KB (whichever is hit first).",
	Params: obj(nil,
		str("path", "Directory to list (default: current directory)"),
		num("limit", "Maximum number of entries to return (default: 500)"),
	),
}
