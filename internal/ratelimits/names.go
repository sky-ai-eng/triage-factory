package ratelimits

import (
	"context"
	"fmt"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	ghclient "github.com/sky-ai-eng/triage-factory/internal/github"
	"github.com/sky-ai-eng/triage-factory/internal/upstream"
)

// Namer returns the display names of an upstream's credentials in orgID,
// keyed by the opaque credential id the upstream's client records readings
// under. A credential it cannot name is left out rather than given a
// placeholder: the reader then shows the id, which is at least true.
//
// It runs in the reader's transaction, under the reader's claims in Postgres,
// so it reads only what that reader may see.
type Namer func(ctx context.Context, tx db.TxStores, orgID string) (map[string]string, error)

// namers holds a Namer per upstream. Only an upstream whose credential ids
// name something a person would recognize has one: a GitHub installation is on
// a GitHub account, a Slack bot token is in a workspace. An id that is the
// credential's kind ("pat", "cloud_api_token") needs no name; a client renders
// the kind.
var namers = map[upstream.Name]Namer{
	upstream.GitHub: githubNames,
}

// RegisterNamer declares the Namer for an upstream core does not name itself.
// Panics on a nil namer or an upstream that already has one: both are wiring
// bugs, and a boot failure beats credentials named by the wrong upstream.
func RegisterNamer(name upstream.Name, namer Namer) {
	if namer == nil {
		panic(fmt.Sprintf("ratelimits.RegisterNamer: %s must supply a Namer", name))
	}
	if _, exists := namers[name]; exists {
		panic(fmt.Sprintf("ratelimits.RegisterNamer: %s already has a Namer", name))
	}
	namers[name] = namer
}

// Names returns the display names of the credentials of every upstream in
// upstreams, keyed by upstream and then credential id. An upstream with no
// Namer contributes nothing.
//
// Naming is best-effort, because a name only labels a reading the reader gets
// either way: an upstream whose Namer fails is logged and left out, and the
// reader shows its credentials by id. Each Namer runs in its own transaction
// from inTx, so one that fails (in Postgres, aborting the transaction it ran
// in) costs no other upstream its names.
func Names(ctx context.Context, orgID string, upstreams []upstream.Name, inTx func(func(db.TxStores) error) error) map[upstream.Name]map[string]string {
	out := map[upstream.Name]map[string]string{}
	for _, name := range upstreams {
		namer, ok := namers[name]
		if !ok {
			continue
		}
		if _, done := out[name]; done {
			continue
		}
		var names map[string]string
		if err := inTx(func(tx db.TxStores) error {
			var err error
			names, err = namer(ctx, tx, orgID)
			return err
		}); err != nil {
			log.Warn("rate-limit credentials left unnamed", "upstream", name, "org_id", orgID, "error", err)
			continue
		}
		out[name] = names
	}
	return out
}

// githubNames names each of the org's App installations by the GitHub account
// it is installed on. A PAT needs no name: an org holds one.
func githubNames(ctx context.Context, tx db.TxStores, orgID string) (map[string]string, error) {
	installations, err := tx.GitHubApps.ListInstallationsForOrg(ctx, orgID)
	if err != nil {
		return nil, err
	}
	out := make(map[string]string, len(installations))
	for _, inst := range installations {
		if inst.AccountLogin != "" {
			out[ghclient.InstallationCredential(inst.InstallationID)] = inst.AccountLogin
		}
	}
	return out, nil
}
