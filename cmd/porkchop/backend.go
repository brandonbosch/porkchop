package main

import (
	"flag"

	"github.com/brandonbosch/porkchop/internal/model"
)

// backendFlags are the flags that choose where inference happens. Both the
// review and process commands take them, and both must resolve them the same
// way: the resolved model id goes into the cache key, so a disagreement between
// the two commands would split the cache.
type backendFlags struct {
	preset   *string
	provider *string
	modelID  *string
	region   *string
	baseURL  *string
}

func addBackendFlags(fs *flag.FlagSet) *backendFlags {
	return &backendFlags{
		preset:   fs.String("preset", "", "named backend preset from the config file (see -write-config)"),
		provider: fs.String("provider", "", "inference backend: bedrock (default), anthropic, openai, openai-compat"),
		modelID:  fs.String("model", "", "model id (for bedrock, a Bedrock inference profile id, not an ~/.aws named profile)"),
		region:   fs.String("region", "", "AWS region for bedrock (default $PORKCHOP_BEDROCK_REGION or the AWS config)"),
		baseURL:  fs.String("base-url", "", "endpoint override; required for openai-compat"),
	}
}

// config is what the flags alone say. Empty fields mean "not given" — none of
// these has a meaningful empty value — which is what lets the environment and
// then a preset fill them in underneath.
func (b *backendFlags) config() model.Config {
	return model.Config{
		Provider: *b.provider,
		Model:    *b.modelID,
		Region:   *b.region,
		BaseURL:  *b.baseURL,
	}
}

// resolve produces the backend this run will actually use, layering
//
//	flags > environment > preset > built-in default
//
// and applying every provider's validation. It touches the config file but no
// network and no credentials, so a caller still learns the model id — and so
// the cache key — before deciding whether any inference is needed.
func (b *backendFlags) resolve() (model.Config, error) {
	fallback, err := resolvePreset(*b.preset)
	if err != nil {
		return model.Config{}, err
	}
	return model.ResolveWithDefaults(b.config(), fallback)
}

// describe is the one-line statement of where this run is about to send a diff,
// printed at the moment inference actually happens.
//
// It exists because presets make the active backend invisible: the whole point
// of a stored config is that you stop typing the flags, and the cost of that is
// that "which cloud is this going to" stops being on screen. porkchop's posture
// is that egress is never silent, so the line is printed on every live call —
// not on a cache hit, which sends nothing anywhere.
func describeBackend(cfg model.Config) string {
	switch cfg.Provider {
	case model.ProviderBedrock:
		return "bedrock " + cfg.Region + " (" + cfg.Model + ")"
	case model.ProviderCompat:
		return "openai-compat " + cfg.BaseURL + " (" + cfg.Model + ")"
	default:
		where := cfg.BaseURL
		if where == "" {
			where = "the public API"
		}
		return cfg.Provider + " " + where + " (" + cfg.Model + ")"
	}
}
