package app

import (
	"errors"
	"strings"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/modelcatalog"
)

// The catalog that ships must join. Both files are compiled in, so this failing
// means the binary would boot with fewer models than it names.
func TestCheckModelCatalog_ShippedCatalogBoots(t *testing.T) {
	if err := checkModelCatalog("v1.2.3", modelcatalog.JoinError()); err != nil {
		t.Fatalf("released build refuses the shipped catalog: %v", err)
	}
	if err := checkModelCatalog(unreleasedVersion, modelcatalog.JoinError()); err != nil {
		t.Fatalf("unreleased build refuses the shipped catalog: %v", err)
	}
}

// The severity split: an unreleased build fails, because whoever built it can
// fix it; a released one carries on with a smaller catalog, because the
// operator running it cannot rebuild and a model retired upstream must not cost
// them the server.
func TestCheckModelCatalog_SeverityFollowsTheBuild(t *testing.T) {
	joinErr := errors.New("claude-retired-9: no priced row in the pricing datasheet")

	for _, version := range []string{"", unreleasedVersion} {
		err := checkModelCatalog(version, joinErr)
		if err == nil {
			t.Errorf("version %q: boot succeeded on a broken join", version)
			continue
		}
		if !strings.Contains(err.Error(), "claude-retired-9") {
			t.Errorf("version %q: boot error %q does not name the offending key", version, err)
		}
	}

	if err := checkModelCatalog("v1.2.3", joinErr); err != nil {
		t.Errorf("released build refused to boot on a broken join: %v", err)
	}
}
