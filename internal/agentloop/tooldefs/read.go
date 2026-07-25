package tooldefs

// Read is the file-reading tool. It is the only way the agent sees
// file contents, and the truncation rules in its description are what stop a
// large file from evicting the rest of the window.
//
// Implemented by the harness in the jail; the loop only forwards the call.
var Read = Tool{
	Name:        "read",
	Description: "Read the contents of a file. Supports text files and images (jpg, png, gif, webp, bmp). Images are sent as attachments. For text files, output is truncated to 2000 lines or 50KB (whichever is hit first). Use offset/limit for large files. When you need the full file, continue with offset until complete.",
	Params: obj([]string{"path"},
		str("path", "Path to the file to read (relative or absolute)"),
		num("offset", "Line number to start reading from (1-indexed)"),
		num("limit", "Maximum number of lines to read"),
	),
}
