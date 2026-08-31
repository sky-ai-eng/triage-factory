package systemllm

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/modelaccess"
	"github.com/sky-ai-eng/triage-factory/internal/modelcatalog"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// settingsReader is a one-method stub for the org-settings read.
type settingsReader struct {
	set domain.OrgSettings
	err error
}

func (s settingsReader) GetSettingsSystem(context.Context, string) (domain.OrgSettings, error) {
	return s.set, s.err
}

// bothConnected is an org holding a credential for every provider the build
// drives, so the provider gate never decides these cases.
func bothConnected() domain.OrgSettings {
	return domain.OrgSettings{AnthropicAPIKeyRef: "anthropic_api_key", BedrockCredentialsRef: "aws_role_arn"}
}

// TestModelForSettings_Accepts pins that the picker's whole universe is
// available to the background jobs: they are toolless and short-context, so the
// gates that narrow a delegation's picker (tool support, a 64k window)
// deliberately do not narrow this one.
//
// Both universes, because the accepted set is the deployment's own vocabulary —
// a local install stores and dispatches the harness alias, a multi one the
// concrete wire id, and each must accept its own.
func TestModelForSettings_Accepts(t *testing.T) {
	for _, mode := range []runmode.Mode{runmode.ModeMulti, runmode.ModeLocal} {
		t.Run(string(mode), func(t *testing.T) {
			runmode.SetForTest(t, mode)
			for _, key := range modelcatalog.UniverseFor(mode == runmode.ModeMulti).Keys() {
				set := bothConnected()
				set.BackgroundJobsModel = key
				got, err := ModelForSettings(set)
				if err != nil {
					t.Errorf("%s: %v", key, err)
					continue
				}
				if got != key {
					t.Errorf("%s: resolved to %q", key, got)
				}
			}
		})
	}
}

// The other deployment's vocabulary is refused, in both directions: a stored
// value is dispatched verbatim, so an alias on the native wire persists unpriced
// and a concrete id handed to the harness is a version pin nobody asked for.
func TestModelForSettings_RefusesTheOtherVocabulary(t *testing.T) {
	for _, tc := range []struct {
		mode  runmode.Mode
		model string
	}{
		{runmode.ModeMulti, domain.ModelAliasSonnet},
		{runmode.ModeLocal, domain.ModelSonnet},
	} {
		t.Run(string(tc.mode), func(t *testing.T) {
			runmode.SetForTest(t, tc.mode)
			set := bothConnected()
			set.BackgroundJobsModel = tc.model
			got, err := ModelForSettings(set)
			if !errors.Is(err, ErrNoModel) {
				t.Fatalf("err = %v, want ErrNoModel", err)
			}
			if got != "" {
				t.Errorf("resolved to %q; a refusal must name no model at all", got)
			}
			if !strings.Contains(err.Error(), tc.model) {
				t.Errorf("error %q does not name the refused value", err)
			}
		})
	}
}

// TestModelForSettings_Refusals pins the three ways the setting is unusable.
// Every one answers ErrNoModel and names the setting, because picking a
// different model is the whole remedy — and none of them substitutes a model of
// TF's choosing, which is what a caller would do with any other error.
func TestModelForSettings_Refusals(t *testing.T) {
	// The provider-specific cases only exist on the native path, where the id
	// names the provider that serves it; multi mode is where that path runs.
	runmode.SetForTest(t, runmode.ModeMulti)
	anthropicModel := modelOn(t, modelcatalog.ProviderAnthropic)
	bedrockModel := modelOn(t, modelcatalog.ProviderBedrock)

	cases := []struct {
		name string
		set  domain.OrgSettings
	}{
		{
			// The org predates the setting, or an admin deliberately turned the
			// jobs off.
			name: "unset",
			set:  bothConnected(),
		},
		{
			// A key from an older catalog, or a hand-edited row.
			name: func() string { return "not offered" }(),
			set: func() domain.OrgSettings {
				s := bothConnected()
				s.BackgroundJobsModel = "claude-from-a-catalog-that-no-longer-ships"
				return s
			}(),
		},
		{
			// The org picked a Bedrock model and then disconnected Bedrock. The
			// value is still a catalog key; there is just nothing to invoke it
			// with, and the Anthropic key it also holds is not a substitute.
			name: "provider disconnected",
			set: domain.OrgSettings{
				AnthropicAPIKeyRef:  "anthropic_api_key",
				BackgroundJobsModel: bedrockModel,
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ModelForSettings(tc.set)
			if !errors.Is(err, ErrNoModel) {
				t.Fatalf("err = %v, want ErrNoModel", err)
			}
			if got != "" {
				t.Errorf("resolved to %q; a refusal must name no model at all", got)
			}
			if !strings.Contains(err.Error(), "background jobs model") {
				t.Errorf("error %q does not name the setting to change", err)
			}
		})
	}

	t.Run("a disconnected provider still says which one", func(t *testing.T) {
		set := domain.OrgSettings{AnthropicAPIKeyRef: "anthropic_api_key", BackgroundJobsModel: bedrockModel}
		_, err := ModelForSettings(set)
		if !errors.Is(err, modelaccess.ErrProviderUnconfigured) {
			t.Errorf("err = %v, want the provider fault preserved under ErrNoModel", err)
		}
	})

	t.Run("an org on the host's credentials is not refused", func(t *testing.T) {
		// Local mode's zero-config subscription: it has chosen the host's
		// environment as its credential, so there is nothing for a model to
		// select between and nothing missing. Only local can hold that source,
		// which is also the mode whose universe the alias below belongs to.
		runmode.SetForTest(t, runmode.ModeLocal)
		set := domain.OrgSettings{
			BackgroundJobsModel: domain.LocalBackgroundJobsModel,
			LLMAuthMethod:       domain.LLMAuthSystem,
		}
		if _, err := ModelForSettings(set); err != nil {
			t.Errorf("ModelForSettings: %v", err)
		}
	})

	// The same empty credential set, the opposite answer, because the org said
	// it brings its own. A cycle here would otherwise authenticate from whatever
	// the operator's shell holds and bill this org's work to a credential nobody
	// configured — so it refuses, and names the gap rather than the model.
	t.Run("an org bringing its own credentials with none bound is refused", func(t *testing.T) {
		set := domain.OrgSettings{
			BackgroundJobsModel: anthropicModel,
			LLMAuthMethod:       domain.LLMAuthBYOK,
		}
		got, err := ModelForSettings(set)
		if !errors.Is(err, ErrNoModel) {
			t.Fatalf("err = %v, want ErrNoModel", err)
		}
		if !errors.Is(err, modelaccess.ErrNoCredentials) {
			t.Errorf("err = %v, want the credential fault preserved under ErrNoModel", err)
		}
		if got != "" {
			t.Errorf("resolved to %q; a refusal must name no model at all", got)
		}
	})

	// Multi resolves to BYOK whatever the row says, so the same org is refused
	// there even having chosen the host's credentials — a multi-mode deployment has
	// none to lend.
	t.Run("multi refuses a stored host credential source", func(t *testing.T) {
		runmode.SetForTest(t, runmode.ModeMulti)
		set := domain.OrgSettings{
			BackgroundJobsModel: anthropicModel,
			LLMAuthMethod:       domain.LLMAuthSystem,
		}
		if _, err := ModelForSettings(set); !errors.Is(err, modelaccess.ErrNoCredentials) {
			t.Errorf("err = %v, want ErrNoCredentials", err)
		}
	})
}

// TestNewModelFunc pins the store-backed adapter, including its two fail-closed
// paths: a read that fails and a reader that was never wired both answer with an
// error rather than a model, because a resolver that guesses is the shipped
// fallback this package exists to not have.
//
// Neither is ErrNoModel, and that separation is load-bearing: callers quiet
// ErrNoModel down to a WARN-and-skip because an org that has not picked is not a
// failure, and an unreadable settings row reported that way would be a wedged
// database showing up as a configuration state nobody can act on.
func TestNewModelFunc(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeMulti)
	ctx := context.Background()
	set := bothConnected()
	set.BackgroundJobsModel = domain.ModelHaiku

	t.Run("reads the org's setting", func(t *testing.T) {
		got, err := NewModelFunc(settingsReader{set: set}).Resolve(ctx, "org-1")
		if err != nil {
			t.Fatalf("Resolve: %v", err)
		}
		if got != domain.ModelHaiku {
			t.Errorf("model = %q, want %q", got, domain.ModelHaiku)
		}
	})

	t.Run("a failed read is a fault, not an unusable setting", func(t *testing.T) {
		cause := errors.New("db down")
		_, err := NewModelFunc(settingsReader{err: cause}).Resolve(ctx, "org-1")
		if !errors.Is(err, cause) {
			t.Fatalf("err = %v, want the read failure preserved", err)
		}
		if errors.Is(err, ErrNoModel) {
			t.Error("a failed read reported as ErrNoModel; callers would log a wedged database as an org that has not picked")
		}
	})

	t.Run("an unwired reader fails closed as a fault", func(t *testing.T) {
		_, err := NewModelFunc(nil).Resolve(ctx, "org-1")
		if err == nil {
			t.Fatal("expected an error")
		}
		if errors.Is(err, ErrNoModel) {
			t.Error("an unwired reader reported as ErrNoModel; it is a caller bug, not configuration")
		}
	})

	t.Run("a nil resolver fails closed as a fault", func(t *testing.T) {
		var f ModelFunc
		_, err := f.Resolve(ctx, "org-1")
		if err == nil {
			t.Fatal("expected an error")
		}
		if errors.Is(err, ErrNoModel) {
			t.Error("a nil resolver reported as ErrNoModel; it is a caller bug, not configuration")
		}
	})
}
