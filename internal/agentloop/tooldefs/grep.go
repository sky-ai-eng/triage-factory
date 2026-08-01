package tooldefs

// Grep searches file contents.
//
// Implemented by the harness in the jail; the loop only forwards the call.
var Grep = Tool{
	Name:        "grep",
	Description: "Search file contents for a pattern. Returns matching lines with file paths and line numbers. Respects .gitignore. Output is truncated to 100 matches or 50KB (whichever is hit first). Long lines are truncated to 500 chars.",
	Params: obj([]string{"pattern"},
		str("pattern", "Search pattern (regex or literal string)"),
		str("path", "Directory or file to search (default: current directory)"),
		str("glob", "Filter files by glob pattern, e.g. '*.ts' or '**/*.spec.ts'"),
		boolean("ignoreCase", "Case-insensitive search (default: false)"),
		boolean("literal", "Treat pattern as literal string instead of regex (default: false)"),
		num("context", "Number of lines to show before and after each match (default: 0)"),
		num("limit", "Maximum number of matches to return (default: 100)"),
	),
}
