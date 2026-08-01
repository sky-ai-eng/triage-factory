package tooldefs

// Write replaces a file wholesale. Distinct from edit so the model
// picks deliberately between rewriting and patching.
//
// Implemented by the harness in the jail; the loop only forwards the call.
var Write = Tool{
	Name:        "write",
	Description: "Write content to a file. Creates the file if it doesn't exist, overwrites if it does. Automatically creates parent directories.",
	Params: obj([]string{"path", "content"},
		str("path", "Path to the file to write (relative or absolute)"),
		str("content", "Content to write to the file"),
	),
}
