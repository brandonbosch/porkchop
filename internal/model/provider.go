package model

import (
	"cmp"
	"context"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"charm.land/fantasy"
	"charm.land/fantasy/providers/anthropic"
	"charm.land/fantasy/providers/bedrock"
	"charm.land/fantasy/providers/openai"
	"charm.land/fantasy/providers/openaicompat"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"

	"github.com/brandonbosch/porkchop/meat"
)

// The backends porkchop can talk to.
const (
	// ProviderBedrock is Claude on AWS Bedrock: the CUI-compliant path, and
	// porkchop's default. Note that fantasy's "bedrock" means Claude on
	// Bedrock, not Nova or Llama — which suits porkchop, since meat's rubric is
	// Claude-tuned, but it is not a general Bedrock gateway.
	ProviderBedrock = "bedrock"
	// ProviderAnthropic is the public Anthropic API.
	ProviderAnthropic = "anthropic"
	// ProviderOpenAI is the public OpenAI API.
	ProviderOpenAI = "openai"
	// ProviderCompat is any OpenAI-compatible endpoint, including a local
	// Ollama or llama.cpp server. It requires an explicit base URL.
	ProviderCompat = "openai-compat"
)

// DefaultProvider is what porkchop uses when no provider is named.
//
// It is Bedrock deliberately. Everything porkchop sends a model — the diff, and
// the surrounding source that meat's read_file and grep tools pull in — is
// assumed to be CUI, so reaching a public API has to be something a reviewer
// asks for by name. A default that could quietly egress is the failure this
// whole package exists to prevent.
const DefaultProvider = ProviderBedrock

// Config selects a backend. The zero value means "Bedrock, resolving the model
// and region from the environment".
type Config struct {
	// Provider is one of the Provider* constants. Empty resolves from
	// $PORKCHOP_PROVIDER, then DefaultProvider.
	Provider string
	// Model is the provider's model id. For Bedrock it must be a complete
	// inference profile id, e.g. "us.anthropic.claude-sonnet-4-5-20250929-v1:0";
	// fantasy v0.41.1 passes the id through verbatim and adds no region prefix.
	// Empty resolves from $PORKCHOP_MODEL, then $MEAT_MODEL, then the
	// provider's default — Bedrock and openai-compat have none.
	Model string
	// Region is the AWS region for Bedrock. Empty resolves from
	// $PORKCHOP_BEDROCK_REGION, then the standard AWS configuration.
	Region string
	// BaseURL overrides the endpoint. Required for openai-compat, ignored for
	// Bedrock, whose endpoint is derived from the region and pinned.
	BaseURL string
}

// Resolve fills in a Config from the environment and validates the combination.
// It performs no network, credential, or filesystem work, so a caller can learn
// the exact model id — and therefore the cache key — before deciding whether
// any inference is needed at all. Open calls it too, so callers key and compute
// off the same resolution.
func Resolve(cfg Config) (Config, error) {
	cfg.Provider = cmp.Or(cfg.Provider, os.Getenv("PORKCHOP_PROVIDER"), DefaultProvider)
	// PORKCHOP_MODEL is read before MEAT_MODEL so a machine that also runs
	// plain meat can point porkchop at a Bedrock profile without disturbing the
	// public model meat uses.
	cfg.Model = cmp.Or(cfg.Model, os.Getenv("PORKCHOP_MODEL"), os.Getenv("MEAT_MODEL"))

	switch cfg.Provider {
	case ProviderBedrock:
		cfg.Region = cmp.Or(cfg.Region, os.Getenv("PORKCHOP_BEDROCK_REGION"))
		if cfg.Model == "" {
			return Config{}, fmt.Errorf("porkchop: bedrock needs an inference profile id: pass -model (e.g. -model us.anthropic.claude-sonnet-4-5-20250929-v1:0) or set $PORKCHOP_MODEL")
		}
	case ProviderAnthropic:
		cfg.Model = cmp.Or(cfg.Model, meat.DefaultAnthropicModel)
	case ProviderOpenAI:
		cfg.Model = cmp.Or(cfg.Model, meat.DefaultOpenAIModel)
	case ProviderCompat:
		if cfg.Model == "" {
			return Config{}, fmt.Errorf("porkchop: %s needs a model id: pass -model or set $PORKCHOP_MODEL", ProviderCompat)
		}
		cfg.BaseURL = cmp.Or(cfg.BaseURL, os.Getenv("PORKCHOP_BASE_URL"))
		if cfg.BaseURL == "" {
			return Config{}, fmt.Errorf("porkchop: %s needs an endpoint: pass -base-url or set $PORKCHOP_BASE_URL", ProviderCompat)
		}
	default:
		return Config{}, fmt.Errorf("porkchop: unknown provider %q (want one of %s, %s, %s, %s)",
			cfg.Provider, ProviderBedrock, ProviderAnthropic, ProviderOpenAI, ProviderCompat)
	}
	return cfg, nil
}

// Open resolves cfg, validates whatever credentials the chosen backend needs,
// and returns a meat.Model.
func Open(ctx context.Context, cfg Config) (meat.Model, error) {
	cfg, err := Resolve(cfg)
	if err != nil {
		return nil, err
	}

	var provider fantasy.Provider
	switch cfg.Provider {
	case ProviderBedrock:
		provider, err = openBedrock(ctx, cfg)
	case ProviderAnthropic:
		key := os.Getenv("ANTHROPIC_API_KEY")
		if key == "" {
			return nil, fmt.Errorf("porkchop: %s needs $ANTHROPIC_API_KEY", ProviderAnthropic)
		}
		opts := []anthropic.Option{anthropic.WithAPIKey(key)}
		if cfg.BaseURL != "" {
			opts = append(opts, anthropic.WithBaseURL(cfg.BaseURL))
		}
		provider, err = anthropic.New(opts...)
	case ProviderOpenAI:
		key := os.Getenv("OPENAI_API_KEY")
		if key == "" {
			return nil, fmt.Errorf("porkchop: %s needs $OPENAI_API_KEY", ProviderOpenAI)
		}
		opts := []openai.Option{openai.WithAPIKey(key)}
		if cfg.BaseURL != "" {
			opts = append(opts, openai.WithBaseURL(cfg.BaseURL))
		}
		provider, err = openai.New(opts...)
	case ProviderCompat:
		// A local server usually wants no key at all; send a placeholder so the
		// client does not refuse to build.
		provider, err = openaicompat.New(
			openaicompat.WithBaseURL(cfg.BaseURL),
			openaicompat.WithAPIKey(cmp.Or(os.Getenv("PORKCHOP_API_KEY"), "none")),
		)
	}
	if err != nil {
		return nil, err
	}

	lm, err := provider.LanguageModel(ctx, cfg.Model)
	if err != nil {
		return nil, fmt.Errorf("porkchop: %s: %w", cfg.Provider, err)
	}
	return New(lm), nil
}

// openBedrock builds the Bedrock provider, but only after proving the
// credentials exist.
//
// This is the whole reason internal/model resolves credentials itself. In
// fantasy v0.41.1 the Bedrock credential path is
//
//	if cfg, err := config.LoadDefaultConfig(ctx); err == nil { ...WithConfig(cfg) }
//
// with no else. When configuration loading fails the error is swallowed and no
// Bedrock option is appended at all, leaving a plain Anthropic client aimed at
// api.anthropic.com — which will happily pick up an $ANTHROPIC_API_KEY that
// happens to be set for home use. Silent egress of CUI, looking like success.
//
// Two guards, because either alone is thinner than it appears:
//
//   - Loading the AWS configuration is not the same as having credentials.
//     LoadDefaultConfig succeeds for an expired SSO session and fails only on
//     something structural like an unknown profile; an expired token surfaces
//     at Retrieve. So we Retrieve, and refuse a session that cannot produce
//     keys — the exact case of a reviewer whose SSO lapsed overnight.
//   - Validation still only makes the fallback unlikely, not impossible, since
//     fantasy loads its own configuration a moment later. Pinning the transport
//     to the Bedrock host makes it impossible: a request addressed anywhere
//     else cannot leave. It fails loudly and names the host it refused, so an
//     upstream change to the endpoint shows up as an error rather than as
//     egress.
func openBedrock(ctx context.Context, cfg Config) (fantasy.Provider, error) {
	awsCfg, err := awsconfig.LoadDefaultConfig(ctx)
	if err != nil {
		return nil, fmt.Errorf("porkchop: bedrock: cannot load AWS configuration: %w", err)
	}
	region := cmp.Or(cfg.Region, awsCfg.Region)
	if region == "" {
		return nil, fmt.Errorf("porkchop: bedrock: no AWS region: pass -region, or set $PORKCHOP_BEDROCK_REGION or $AWS_REGION")
	}
	if awsCfg.Credentials == nil {
		return nil, fmt.Errorf("porkchop: bedrock: no AWS credentials configured")
	}
	creds, err := awsCfg.Credentials.Retrieve(ctx)
	if err != nil {
		return nil, fmt.Errorf("porkchop: bedrock: cannot retrieve AWS credentials (an expired SSO session looks like this — try refreshing it): %w", err)
	}
	if !creds.HasKeys() {
		return nil, fmt.Errorf("porkchop: bedrock: AWS credentials from %q are empty", creds.Source)
	}

	return bedrock.New(
		bedrock.WithRegion(region),
		bedrock.WithHTTPClient(&http.Client{
			Timeout:   2 * time.Minute,
			Transport: &pinnedHost{host: bedrockHost(region), rt: http.DefaultTransport},
		}),
	)
}

// bedrockHost is the endpoint anthropic-sdk-go's bedrock.WithConfig sets for a
// region. Deriving it the same way is what lets pinnedHost tell an intended
// request from a fallback.
func bedrockHost(region string) string {
	return "bedrock-runtime." + region + ".amazonaws.com"
}

// pinnedHost is a RoundTripper that refuses to send anywhere but one host over
// HTTPS. It is the structural half of the no-fallback guarantee: whatever a
// provider decides about credentials, a request to the public API cannot leave
// this process.
type pinnedHost struct {
	host string
	rt   http.RoundTripper
}

func (p *pinnedHost) RoundTrip(req *http.Request) (*http.Response, error) {
	if !strings.EqualFold(req.URL.Hostname(), p.host) || req.URL.Scheme != "https" {
		return nil, fmt.Errorf("porkchop: refusing to send to %s://%s: this run is pinned to https://%s", req.URL.Scheme, req.URL.Host, p.host)
	}
	return p.rt.RoundTrip(req)
}
