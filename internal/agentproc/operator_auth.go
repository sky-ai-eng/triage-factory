package agentproc

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"slices"

	"github.com/sky-ai-eng/triage-factory/internal/paths"
)

// operatorAuthHelperKeys are the keys of the operator's user settings that
// name a command Claude Code runs to authenticate. Each one is forwarded
// verbatim as flag settings, because a run reads no user settings of its own
// (see settingSources).
var operatorAuthHelperKeys = []string{
	"apiKeyHelper",
	"proxyAuthHelper",
	"awsAuthRefresh",
	"awsCredentialExport",
	"gcpAuthRefresh",
}

// operatorAuth is the part of the operator's ~/.claude/settings.json that a
// run started without a TF-resolved credential still honors: the auth helper
// commands, and the settings env narrowed to the keys isCredentialEnvKey
// matches. Under "Use system Claude Code credentials" these are how an
// operator who set up Claude Code with a gateway, Bedrock, Vertex, Foundry or
// an apiKeyHelper authenticates, and they are the only keys of that file a
// run sees. A run with a resolved credential forwards none of it, so the
// stored key decides.
//
// The env half rides the process environment rather than --settings because
// the SDK hands --settings to Claude Code in argv, where any local user can
// read it. Layered over the inherited environment, it wins the way Claude
// Code applies a settings env over the process env. The helper commands carry
// no secret of their own, and Claude Code already puts each one in a shell's
// argv when it runs it.
type operatorAuth struct {
	// settings is the --settings JSON object holding the helper keys, or ""
	// when the file sets none.
	settings string
	// env is KEY=VALUE pairs in key order.
	env []string
}

// loadOperatorAuth reads the operator's user settings for a run TF resolved
// no credential for. A missing file forwards nothing. One TF cannot read or
// parse forwards nothing either, with a warning: Claude Code ignores a broken
// settings file the same way, and a run that authenticates some other way
// must not fail over a file it never needed.
func loadOperatorAuth() operatorAuth {
	path, err := paths.ExpandHome("~/.claude/settings.json")
	if err != nil {
		agentprocLog.Warn("resolve the operator's Claude Code settings; forwarding no auth settings", "error", err)
		return operatorAuth{}
	}
	auth, err := readOperatorAuth(path)
	if err != nil {
		agentprocLog.Warn("read the operator's Claude Code settings; forwarding no auth settings", "path", path, "error", err)
		return operatorAuth{}
	}
	return auth
}

func readOperatorAuth(path string) (operatorAuth, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return operatorAuth{}, nil
	}
	if err != nil {
		return operatorAuth{}, err
	}
	var file map[string]json.RawMessage
	if err := json.Unmarshal(data, &file); err != nil {
		return operatorAuth{}, fmt.Errorf("parse: %w", err)
	}

	var auth operatorAuth
	helpers := map[string]string{}
	for _, key := range operatorAuthHelperKeys {
		var cmd string
		if json.Unmarshal(file[key], &cmd) == nil && cmd != "" {
			helpers[key] = cmd
		}
	}
	if len(helpers) > 0 {
		b, err := json.Marshal(helpers)
		if err != nil {
			return operatorAuth{}, err
		}
		auth.settings = string(b)
	}

	var env map[string]json.RawMessage
	if json.Unmarshal(file["env"], &env) == nil {
		for key, raw := range env {
			if !isCredentialEnvKey(key) {
				continue
			}
			if value, ok := settingsEnvValue(raw); ok {
				auth.env = append(auth.env, key+"="+value)
			}
		}
		slices.Sort(auth.env)
	}
	return auth, nil
}

// settingsEnvValue renders one settings env value as Claude Code would set
// it: a string as itself, a number or boolean as its literal.
func settingsEnvValue(raw json.RawMessage) (string, bool) {
	var v any
	if json.Unmarshal(raw, &v) != nil {
		return "", false
	}
	switch v := v.(type) {
	case string:
		return v, true
	case float64, bool:
		return string(raw), true
	}
	return "", false
}
