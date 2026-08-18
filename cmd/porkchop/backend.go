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
	provider *string
	modelID  *string
	region   *string
	baseURL  *string
}

func addBackendFlags(fs *flag.FlagSet) *backendFlags {
	return &backendFlags{
		provider: fs.String("provider", "", "inference backend: bedrock (default), anthropic, openai, openai-compat"),
		modelID:  fs.String("model", "", "model id (for bedrock, a full inference profile id)"),
		region:   fs.String("region", "", "AWS region for bedrock (default $PORKCHOP_BEDROCK_REGION or the AWS config)"),
		baseURL:  fs.String("base-url", "", "endpoint override; required for openai-compat"),
	}
}

func (b *backendFlags) config() model.Config {
	return model.Config{
		Provider: *b.provider,
		Model:    *b.modelID,
		Region:   *b.region,
		BaseURL:  *b.baseURL,
	}
}
