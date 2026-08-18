// Package model is porkchop's inference backend: a meat.Model implemented over
// charm.land/fantasy, so a review can run against AWS Bedrock — the
// CUI-compliant path — without touching meat's core.
//
// meat is already the agent. It runs its own turn loop with its own tools and
// calls Generate once per turn, so what porkchop needs here is a *model*, not
// an agent; fantasy's Agent half is a separate concern. The adapter is
// deliberately thin, because fantasy is pre-1.0: a thin adapter plus the
// fake-LanguageModel tests beside it are the tripwire for an upstream API
// change.
package model

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"charm.land/fantasy"

	"github.com/brandonbosch/porkchop/meat"
)

// maxOutputTokens is the per-turn output cap. It mirrors meat's own hardcoded
// value so an abridgement computed through porkchop is the same shape as one
// computed through meat — the two share a cache, so they must agree. It is set
// explicitly on every Call: fantasy.Call.MaxOutputTokens is a *int64, and
// leaving it nil would silently accept whatever the provider defaults to.
const maxOutputTokens int64 = 16384

// Model adapts a fantasy.LanguageModel to meat.Model.
type Model struct {
	lm    fantasy.LanguageModel
	retry fantasy.RetryFunction[*fantasy.Response]
}

// retryOptions mirrors meat's own backoff (1s doubling, three retries after the
// first attempt). Retry is porkchop's job here, not fantasy's: fantasy builds
// its Anthropic client with option.WithMaxRetries(0) and applies its own
// exponential backoff only inside fantasy.Agent, so a bare LanguageModel has no
// retry at all. Routing meat through fantasy therefore bypasses meat's
// postJSONWithRetry and lands on nothing unless we put it back.
func retryOptions() fantasy.RetryOptions {
	return fantasy.RetryOptions{
		MaxRetries:     3,
		InitialDelayIn: time.Second,
		BackoffFactor:  2.0,
	}
}

// New wraps lm as a meat.Model.
func New(lm fantasy.LanguageModel) *Model {
	return &Model{
		lm:    lm,
		retry: fantasy.RetryWithExponentialBackoffRespectingRetryHeaders[*fantasy.Response](retryOptions()),
	}
}

// Generate implements meat.Model.
func (m *Model) Generate(ctx context.Context, system string, messages []meat.Message, tools []meat.Tool) (*meat.Response, error) {
	prompt, err := toPrompt(system, messages)
	if err != nil {
		return nil, err
	}
	converted, err := toTools(tools)
	if err != nil {
		return nil, err
	}

	maxOut := maxOutputTokens
	call := fantasy.Call{
		Prompt:          prompt,
		Tools:           converted,
		MaxOutputTokens: &maxOut,
	}

	resp, err := m.retry(ctx, func() (*fantasy.Response, error) {
		return m.lm.Generate(ctx, call)
	})
	if err != nil {
		return nil, fmt.Errorf("porkchop: %s generate: %w", m.lm.Provider(), err)
	}
	if resp == nil {
		return nil, fmt.Errorf("porkchop: %s returned no response", m.lm.Provider())
	}

	// A length stop means the reply was cut off mid-thought — likely inside a
	// tool_use JSON block. meat treats that as an error rather than silently
	// caching a truncated abridgement, and so must porkchop.
	if resp.FinishReason == fantasy.FinishReasonLength {
		return nil, fmt.Errorf("porkchop: response truncated at max_tokens (%d); the diff may be too large to abridge in one pass", maxOutputTokens)
	}

	return fromResponse(resp)
}

// toPrompt builds the fantasy conversation: the system prompt as a leading
// system message, then the turns so far.
func toPrompt(system string, messages []meat.Message) (fantasy.Prompt, error) {
	var prompt fantasy.Prompt
	if system != "" {
		prompt = append(prompt, fantasy.Message{
			Role:    fantasy.MessageRoleSystem,
			Content: []fantasy.MessagePart{fantasy.TextPart{Text: system}},
		})
	}
	for i, msg := range messages {
		converted, err := toMessages(msg)
		if err != nil {
			return nil, fmt.Errorf("message %d: %w", i, err)
		}
		prompt = append(prompt, converted...)
	}
	return prompt, nil
}

// toMessages converts one meat message. It can yield two, because the two
// libraries disagree about where a tool result lives: meat follows the
// Anthropic wire shape and puts tool_result blocks in a *user* message, while
// fantasy gives them their own MessageRoleTool. The tool message is emitted
// first so that, once fantasy regroups them into a single user turn, the tool
// results precede any user text — which is the order Anthropic requires.
func toMessages(msg meat.Message) ([]fantasy.Message, error) {
	role, err := toRole(msg.Role)
	if err != nil {
		return nil, err
	}

	var main, results []fantasy.MessagePart
	for i, b := range msg.Content {
		part, isResult, err := toPart(b)
		if err != nil {
			return nil, fmt.Errorf("block %d: %w", i, err)
		}
		if isResult {
			results = append(results, part)
			continue
		}
		main = append(main, part)
	}

	var out []fantasy.Message
	if len(results) > 0 {
		out = append(out, fantasy.Message{Role: fantasy.MessageRoleTool, Content: results})
	}
	if len(main) > 0 {
		out = append(out, fantasy.Message{Role: role, Content: main})
	}
	return out, nil
}

func toRole(r meat.Role) (fantasy.MessageRole, error) {
	switch r {
	case meat.RoleUser:
		return fantasy.MessageRoleUser, nil
	case meat.RoleAssistant:
		return fantasy.MessageRoleAssistant, nil
	default:
		return "", fmt.Errorf("unsupported role %q", r)
	}
}

// toPart converts one meat block. isResult reports whether the part is a tool
// result, which toMessages routes to its own message.
//
// Unknown block types are refused rather than dropped. A dropped block is a
// hole in a conversation the model is about to reason over, and the failure
// would show up much later as a confusing answer instead of here as an error.
func toPart(b meat.Block) (part fantasy.MessagePart, isResult bool, err error) {
	switch b.Type {
	case "text":
		return fantasy.TextPart{Text: b.Text}, false, nil

	case "tool_use":
		// fantasy carries tool input as a JSON *string*, not raw bytes. An
		// absent input has to become "{}" — an empty string is not valid JSON
		// and the provider would reject the turn.
		input := string(b.ToolInput)
		if len(bytes.TrimSpace(b.ToolInput)) == 0 {
			input = "{}"
		}
		return fantasy.ToolCallPart{
			ToolCallID: b.ID,
			ToolName:   b.ToolName,
			Input:      input,
		}, false, nil

	case "tool_result":
		var out fantasy.ToolResultOutputContent = fantasy.ToolResultOutputContentText{Text: b.ToolResult}
		if b.ToolError {
			// fantasy models an errored tool result as an error value and
			// serializes err.Error() as the block text, so the string survives
			// the round trip and is_error is set on the wire.
			out = fantasy.ToolResultOutputContentError{Error: errors.New(b.ToolResult)}
		}
		return fantasy.ToolResultPart{ToolCallID: b.ToolUseID, Output: out}, true, nil

	case "provider_state":
		// meat documents this as opaque state that must be replayed *exactly*
		// on a later turn. fantasy has its own opaque-state mechanisms
		// (ProviderOptions/ProviderMetadata) and they do not obviously compose,
		// so an approximate translation would corrupt a multi-turn loop
		// silently. Claude on Bedrock without extended thinking never produces
		// one; refuse rather than guess if that ever changes.
		return nil, false, fmt.Errorf("provider_state block from %q cannot be replayed through fantasy", b.Provider)

	default:
		return nil, false, fmt.Errorf("unsupported block type %q", b.Type)
	}
}

// toTools converts meat's tool declarations, whose input schemas are raw JSON.
//
// The `required` normalization below is not cosmetic. fantasy's Anthropic
// provider forwards a FunctionTool's schema by lifting exactly two keys, and it
// reads `required` with a `req.([]string)` type assertion. A schema decoded
// from JSON yields []any there, never []string, so an unnormalized `required`
// is silently dropped and the model is told that nothing is mandatory. Handing
// fantasy a real []string is what makes it survive.
//
// The same lifting drops everything else, including meat's top-level
// "additionalProperties": false. That one has no seam to preserve it, and it
// degrades safely: meat decodes tool input with DisallowUnknownFields, so an
// invented key comes back as a tool error the model can correct on the next
// turn rather than as a bad abridgement.
func toTools(tools []meat.Tool) ([]fantasy.Tool, error) {
	if len(tools) == 0 {
		return nil, nil
	}
	out := make([]fantasy.Tool, 0, len(tools))
	for _, t := range tools {
		schema := map[string]any{}
		if len(bytes.TrimSpace(t.InputSchema)) > 0 {
			if err := json.Unmarshal(t.InputSchema, &schema); err != nil {
				return nil, fmt.Errorf("porkchop: tool %q: decode input schema: %w", t.Name, err)
			}
		}
		if req, ok := schema["required"]; ok {
			names, err := toStringSlice(req)
			if err != nil {
				return nil, fmt.Errorf("porkchop: tool %q: required: %w", t.Name, err)
			}
			schema["required"] = names
		}
		out = append(out, fantasy.FunctionTool{
			Name:        t.Name,
			Description: t.Description,
			InputSchema: schema,
		})
	}
	return out, nil
}

func toStringSlice(v any) ([]string, error) {
	switch typed := v.(type) {
	case []string:
		return typed, nil
	case []any:
		names := make([]string, 0, len(typed))
		for _, item := range typed {
			s, ok := item.(string)
			if !ok {
				return nil, fmt.Errorf("want a list of strings, got %T in the list", item)
			}
			names = append(names, s)
		}
		return names, nil
	default:
		return nil, fmt.Errorf("want a list of strings, got %T", v)
	}
}

// fromResponse converts a fantasy response back to meat's shape.
//
// It walks Content in order rather than using the Text()/ToolCalls() helpers:
// Text() returns only the *first* text part, so a reply that interleaves prose
// and tool calls would lose content and ordering — and meat appends this
// content verbatim as the next assistant message, where a reordered tool_use
// would no longer line up with the tool_result that answers it.
func fromResponse(resp *fantasy.Response) (*meat.Response, error) {
	out := &meat.Response{
		InputTokens:  int(resp.Usage.InputTokens),
		OutputTokens: int(resp.Usage.OutputTokens),
	}
	for _, c := range resp.Content {
		switch c.GetType() {
		case fantasy.ContentTypeText:
			text, ok := fantasy.AsContentType[fantasy.TextContent](c)
			if !ok {
				continue
			}
			out.Content = append(out.Content, meat.Block{Type: "text", Text: text.Text})

		case fantasy.ContentTypeToolCall:
			call, ok := fantasy.AsContentType[fantasy.ToolCallContent](c)
			if !ok {
				continue
			}
			if call.Invalid {
				// The provider could not parse the arguments it produced.
				// Passing them on would fail meat's strict decode a layer
				// deeper, with a worse error.
				return nil, fmt.Errorf("porkchop: model produced an invalid call to %q: %w", call.ToolName, call.ValidationError)
			}
			out.Content = append(out.Content, meat.Block{
				Type:      "tool_use",
				ID:        call.ToolCallID,
				ToolName:  call.ToolName,
				ToolInput: json.RawMessage(call.Input),
			})

		default:
			// Reasoning, files and sources carry nothing meat's loop reads, and
			// meat has no block type for them. Dropping them is safe precisely
			// because they are not replayed: unlike provider_state, nothing
			// downstream expects them back.
		}
	}
	return out, nil
}
