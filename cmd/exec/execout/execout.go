// Package execout is how a `triagefactory exec` verb writes its result and
// its failure: indented JSON on stdout, a message on stderr and exit status 1.
// Every verb surface (gh, jira, linear, memory, workspace) uses it, so an
// agent reads one output shape from all of them.
package execout

import (
	"encoding/json"
	"fmt"
	"os"
)

// PrintJSON writes v to stdout as indented JSON.
func PrintJSON(v any) {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	_ = enc.Encode(v)
}

// ExitOnErr prints err to stderr and exits 1; a nil err does nothing.
func ExitOnErr(err error) {
	if err != nil {
		ExitErr(err.Error())
	}
}

// ExitErr prints msg to stderr and exits 1.
func ExitErr(msg string) {
	fmt.Fprintln(os.Stderr, msg)
	os.Exit(1)
}
