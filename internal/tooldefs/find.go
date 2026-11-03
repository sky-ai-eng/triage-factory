package tooldefs

// Find searches for files by glob.
//
// Implemented by the harness in the jail; the loop only forwards the call.
var Find = Tool{
	Name:        "find",
	Description: "Search for files by glob pattern. Returns matching file paths relative to the search directory. Respects .gitignore. Output is truncated to 1000 results or 50KB (whichever is hit first).",
	Params: obj([]string{"pattern"},
		str("pattern", "Glob pattern to match files, e.g. '*.ts', '**/*.json', or 'src/**/*.spec.ts'"),
		str("path", "Directory to search in (default: current directory)"),
		num("limit", "Maximum number of results (default: 1000)"),
	),
}
