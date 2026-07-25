package tooldefs

// Bash executes a shell command in the workspace. Inside the jail that
// is the widest capability the agent has, which is why the sandbox exists at
// all rather than the tool surface being trusted to constrain it.
//
// Implemented by the harness in the jail; the loop only forwards the call.
var Bash = Tool{
	Name:        "bash",
	Description: "Execute a bash command in the current working directory. Returns stdout and stderr. Output is truncated to last 2000 lines or 50KB (whichever is hit first). If truncated, full output is saved to a temp file. Optionally provide a timeout in seconds.",
	Params: obj([]string{"command"},
		str("command", "Bash command to execute"),
		num("timeout", "Timeout in seconds (optional, no default timeout)"),
	),
}
