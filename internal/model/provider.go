package model

import (
	"cmp"
	"context"
	"fmt"
	"net/http"
	"net/url"
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
	// inference profile id — "us.anthropic.claude-sonnet-4-5-20250929-v1:0" in
	// the commercial partition, "us-gov.anthropic.…" in GovCloud. fantasy
	// v0.41.1 passes the id through verbatim and adds no prefix of its own, so
	// a commercial id in GovCloud fails rather than being corrected.
	// Empty resolves from $PORKCHOP_MODEL, then $MEAT_MODEL, then the
	// provider's default — Bedrock and openai-compat have none.
	Model string
	// Region is the AWS region for Bedrock. Empty resolves from
	// $PORKCHOP_BEDROCK_REGION, $AWS_REGION, $AWS_DEFAULT_REGION, and then —
	// on the SigV4 path only — the standard AWS configuration.
	Region string
	// APIKey is the credential for whichever provider needs one, resolved
	// per-provider from that provider's own variable: $AWS_BEARER_TOKEN_BEDROCK,
	// $ANTHROPIC_API_KEY, $OPENAI_API_KEY, or $PORKCHOP_API_KEY for
	// openai-compat. There is deliberately no flag for it — a secret on a
	// command line is visible to every process on the box.
	//
	// For Bedrock it is a Bedrock API key: a bearer token, not a SigV4 key
	// pair. When set it replaces the AWS credential chain entirely, which as a
	// side effect makes the silent-egress hazard structurally impossible:
	// fantasy takes a different branch that always installs the Bedrock option.
	// The Bedrock resolution deliberately takes no preset fallback — the
	// presence of this field selects that branch, and the no-fallback guarantee
	// is not something a stored config file should be able to reach into.
	APIKey string
	// BaseURL overrides the endpoint. Required for openai-compat. For Bedrock
	// it is optional and exists for FIPS and VPC endpoints, which the SDK's
	// hardcoded hostname cannot express; the transport pin follows it.
	BaseURL string
}

// Resolve fills in a Config from the environment and validates the combination.
// It performs no network, credential, or filesystem work, so a caller can learn
// the exact model id — and therefore the cache key — before deciding whether
// any inference is needed at all. Open calls it too, so callers key and compute
// off the same resolution.
func Resolve(cfg Config) (Config, error) {
	return ResolveWithDefaults(cfg, Config{})
}

// ResolveWithDefaults is Resolve with a weaker layer underneath the
// environment: any field left empty by both cfg and the environment falls back
// to fallback before the built-in default applies. The precedence is therefore
//
//	explicit (cfg) > environment > fallback > built-in default
//
// which is what lets cmd/porkchop offer stored presets without teaching that
// package the environment variable names — they stay spelled out exactly once,
// here. A preset sits *below* the environment deliberately: it is a stored
// default a reviewer wrote once, and an exported variable in the shell they are
// standing in is the more deliberate signal of the two.
//
// fallback is data, not policy. It cannot select a provider the switch below
// does not know, and it cannot skip any validation — a preset naming
// openai-compat with no base URL fails exactly as the flag would.
func ResolveWithDefaults(cfg, fallback Config) (Config, error) {
	cfg.Provider = cmp.Or(cfg.Provider, os.Getenv("PORKCHOP_PROVIDER"), fallback.Provider, DefaultProvider)
	// PORKCHOP_MODEL is read before MEAT_MODEL so a machine that also runs
	// plain meat can point porkchop at a Bedrock profile without disturbing the
	// public model meat uses.
	cfg.Model = cmp.Or(cfg.Model, os.Getenv("PORKCHOP_MODEL"), os.Getenv("MEAT_MODEL"), fallback.Model)

	switch cfg.Provider {
	case ProviderBedrock:
		cfg.Region = cmp.Or(cfg.Region, os.Getenv("PORKCHOP_BEDROCK_REGION"), os.Getenv("AWS_REGION"), os.Getenv("AWS_DEFAULT_REGION"), fallback.Region)
		cfg.APIKey = cmp.Or(cfg.APIKey, os.Getenv("AWS_BEARER_TOKEN_BEDROCK"))
		if cfg.Model == "" {
			return Config{}, fmt.Errorf("porkchop: bedrock needs a Bedrock inference profile id — an AWS resource naming a model, not the ~/.aws named profile $AWS_PROFILE selects: pass -model or set $PORKCHOP_MODEL (commercial: us.anthropic.claude-sonnet-4-5-20250929-v1:0; GovCloud: us-gov.anthropic.claude-sonnet-4-5-20250929-v1:0); list them with `aws bedrock list-inference-profiles --region <region>`")
		}
		// A Bedrock API key carries no region, and fantasy defaults a missing
		// one to commercial us-east-1. In GovCloud — or any partition that is
		// not the default — that would aim a CUI request at the wrong cloud
		// while looking configured. Demand the region here instead.
		if cfg.APIKey != "" && cfg.Region == "" {
			return Config{}, fmt.Errorf("porkchop: bedrock: a Bedrock API key carries no region, and guessing one would target the wrong partition: pass -region (e.g. -region us-gov-west-1) or set $PORKCHOP_BEDROCK_REGION")
		}
	case ProviderAnthropic:
		cfg.Model = cmp.Or(cfg.Model, meat.DefaultAnthropicModel)
		cfg.BaseURL = cmp.Or(cfg.BaseURL, fallback.BaseURL)
		cfg.APIKey = cmp.Or(cfg.APIKey, os.Getenv("ANTHROPIC_API_KEY"), fallback.APIKey)
	case ProviderOpenAI:
		cfg.Model = cmp.Or(cfg.Model, meat.DefaultOpenAIModel)
		cfg.BaseURL = cmp.Or(cfg.BaseURL, fallback.BaseURL)
		cfg.APIKey = cmp.Or(cfg.APIKey, os.Getenv("OPENAI_API_KEY"), fallback.APIKey)
	case ProviderCompat:
		if cfg.Model == "" {
			return Config{}, fmt.Errorf("porkchop: %s needs a model id: pass -model or set $PORKCHOP_MODEL", ProviderCompat)
		}
		cfg.BaseURL = cmp.Or(cfg.BaseURL, os.Getenv("PORKCHOP_BASE_URL"), fallback.BaseURL)
		if cfg.BaseURL == "" {
			return Config{}, fmt.Errorf("porkchop: %s needs an endpoint: pass -base-url or set $PORKCHOP_BASE_URL", ProviderCompat)
		}
		// Resolved here rather than read at Open time so that every credential
		// a run will use is decided in one offline place. Local servers vary:
		// llama.cpp ignores the key, Ollama ignores it, oMLX and vLLM enforce
		// one. Empty stays empty and Open substitutes a placeholder.
		cfg.APIKey = cmp.Or(cfg.APIKey, os.Getenv("PORKCHOP_API_KEY"), fallback.APIKey)
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
		if cfg.APIKey == "" {
			return nil, fmt.Errorf("porkchop: %s needs $ANTHROPIC_API_KEY", ProviderAnthropic)
		}
		opts := []anthropic.Option{anthropic.WithAPIKey(cfg.APIKey)}
		if cfg.BaseURL != "" {
			opts = append(opts, anthropic.WithBaseURL(cfg.BaseURL))
		}
		provider, err = anthropic.New(opts...)
	case ProviderOpenAI:
		if cfg.APIKey == "" {
			return nil, fmt.Errorf("porkchop: %s needs $OPENAI_API_KEY", ProviderOpenAI)
		}
		opts := []openai.Option{openai.WithAPIKey(cfg.APIKey)}
		if cfg.BaseURL != "" {
			opts = append(opts, openai.WithBaseURL(cfg.BaseURL))
		}
		provider, err = openai.New(opts...)
	case ProviderCompat:
		// Some local servers want no key at all; send a placeholder so the
		// client does not refuse to build.
		provider, err = openaicompat.New(
			openaicompat.WithBaseURL(cfg.BaseURL),
			openaicompat.WithAPIKey(cmp.Or(cfg.APIKey, "none")),
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

// openBedrock builds the Bedrock provider, but only after proving there is
// something to authenticate with.
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
//
// A Bedrock API key takes a different branch, described at withAPIKey.
func openBedrock(ctx context.Context, cfg Config) (fantasy.Provider, error) {
	if cfg.APIKey != "" {
		return withAPIKey(cfg)
	}

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

	pin, err := pinFor(cfg, region)
	if err != nil {
		return nil, err
	}
	opts := []bedrock.Option{bedrock.WithRegion(region), bedrock.WithHTTPClient(pinnedClient(pin))}
	if cfg.BaseURL != "" {
		opts = append(opts, bedrock.WithBaseURL(cfg.BaseURL))
	}
	return bedrock.New(opts...)
}

// withAPIKey builds the Bedrock provider from a Bedrock API key — a bearer
// token, presented as an Authorization header rather than a SigV4 signature.
//
// There is nothing to validate ahead of time: a bearer token has no local
// retrieve step that could fail, so its first real test is the first request.
// That is fine here, because this branch is the *safe* one. fantasy routes a
// non-empty API key through bedrockBasicAuthConfig, which builds the aws.Config
// itself and therefore always installs the Bedrock option — the swallowed-error
// fallback documented above cannot be reached from this path at all.
//
// The one hazard this branch has of its own is the region, and it is a bad one:
// bedrockBasicAuthConfig defaults a missing region to commercial "us-east-1",
// so a GovCloud key with no region set would aim at the wrong partition while
// looking perfectly configured. Resolve refuses that combination before we get
// here, and the transport pin refuses it again on the way out.
func withAPIKey(cfg Config) (fantasy.Provider, error) {
	if cfg.Region == "" {
		return nil, fmt.Errorf("porkchop: bedrock: a Bedrock API key needs an explicit region")
	}
	pin, err := pinFor(cfg, cfg.Region)
	if err != nil {
		return nil, err
	}
	opts := []bedrock.Option{
		bedrock.WithRegion(cfg.Region),
		bedrock.WithAPIKey(cfg.APIKey),
		bedrock.WithHTTPClient(pinnedClient(pin)),
	}
	if cfg.BaseURL != "" {
		opts = append(opts, bedrock.WithBaseURL(cfg.BaseURL))
	}
	return bedrock.New(opts...)
}

// pinFor is the host the transport will allow: the endpoint an explicit
// BaseURL names, or the one anthropic-sdk-go's bedrock.WithConfig derives from
// the region. Deriving it the same way is what lets pinnedHost tell an intended
// request from a fallback.
func pinFor(cfg Config, region string) (string, error) {
	if cfg.BaseURL == "" {
		return bedrockHost(region), nil
	}
	u, err := url.Parse(cfg.BaseURL)
	if err != nil {
		return "", fmt.Errorf("porkchop: bedrock: cannot parse -base-url %q: %w", cfg.BaseURL, err)
	}
	if u.Scheme != "https" || u.Hostname() == "" {
		return "", fmt.Errorf("porkchop: bedrock: -base-url must be an https URL with a host, got %q", cfg.BaseURL)
	}
	return u.Hostname(), nil
}

// bedrockHost is the endpoint anthropic-sdk-go's bedrock.WithConfig sets for a
// region. GovCloud falls out of the same shape (bedrock-runtime.us-gov-west-1
// .amazonaws.com); a FIPS or VPC endpoint does not, and needs -base-url.
func bedrockHost(region string) string {
	return "bedrock-runtime." + region + ".amazonaws.com"
}

func pinnedClient(host string) *http.Client {
	return &http.Client{
		Timeout:   2 * time.Minute,
		Transport: &pinnedHost{host: host, rt: http.DefaultTransport},
	}
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
