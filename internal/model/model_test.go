package model

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"charm.land/fantasy"

	"github.com/brandonbosch/porkchop/meat"
)

// fakeLM is a fantasy.LanguageModel that records the Call it was handed and
// replays a canned response. Every test in this file runs through it: an
// adapter this thin is only worth having if its translation is pinned, and
// pinning it must not need a network.
type fakeLM struct {
	calls []fantasy.Call
	resp  *fantasy.Response
	errs  []error // consumed one per call, then resp is returned
}

func (f *fakeLM) Generate(_ context.Context, call fantasy.Call) (*fantasy.Response, error) {
	f.calls = append(f.calls, call)
	if len(f.errs) > 0 {
		err := f.errs[0]
		f.errs = f.errs[1:]
		return nil, err
	}
	return f.resp, nil
}

func (f *fakeLM) Stream(context.Context, fantasy.Call) (fantasy.StreamResponse, error) {
	return nil, errors.New("not used")
}

func (f *fakeLM) GenerateObject(context.Context, fantasy.ObjectCall) (*fantasy.ObjectResponse, error) {
	return nil, errors.New("not used")
}

func (f *fakeLM) StreamObject(context.Context, fantasy.ObjectCall) (fantasy.ObjectStreamResponse, error) {
	return nil, errors.New("not used")
}

func (f *fakeLM) Provider() string { return "fake" }
func (f *fakeLM) Model() string    { return "fake-model" }

// newTestModel wraps a fake with a retry policy that does not sleep, so a retry
// test costs milliseconds rather than seconds.
func newTestModel(f *fakeLM) *Model {
	m := New(f)
	m.retry = fantasy.RetryWithExponentialBackoffRespectingRetryHeaders[*fantasy.Response](fantasy.RetryOptions{
		MaxRetries:     3,
		InitialDelayIn: time.Millisecond,
		BackoffFactor:  1,
	})
	return m
}

func okResponse() *fantasy.Response {
	return &fantasy.Response{
		Content:      fantasy.ResponseContent{fantasy.TextContent{Text: "ok"}},
		FinishReason: fantasy.FinishReasonStop,
		Usage:        fantasy.Usage{InputTokens: 11, OutputTokens: 7},
	}
}

// TestGenerate_TranslatesConversation: the shape meat hands in must arrive as
// the shape fantasy expects, including the system prompt as its own leading
// message and an explicit output cap.
func TestGenerate_TranslatesConversation(t *testing.T) {
	f := &fakeLM{resp: okResponse()}
	m := newTestModel(f)

	_, err := m.Generate(context.Background(), "be terse", []meat.Message{
		{Role: meat.RoleUser, Content: []meat.Block{{Type: "text", Text: "hello"}}},
		{Role: meat.RoleAssistant, Content: []meat.Block{
			{Type: "tool_use", ID: "t1", ToolName: "read_file", ToolInput: json.RawMessage(`{"path":"a.go"}`)},
		}},
	}, nil)
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if len(f.calls) != 1 {
		t.Fatalf("calls = %d, want 1", len(f.calls))
	}
	call := f.calls[0]

	if call.MaxOutputTokens == nil {
		t.Fatal("MaxOutputTokens is nil; the provider default would silently change abridgement behavior")
	}
	if *call.MaxOutputTokens != maxOutputTokens {
		t.Errorf("MaxOutputTokens = %d, want %d", *call.MaxOutputTokens, maxOutputTokens)
	}

	wantRoles := []fantasy.MessageRole{
		fantasy.MessageRoleSystem,
		fantasy.MessageRoleUser,
		fantasy.MessageRoleAssistant,
	}
	if got := roles(call.Prompt); !equalRoles(got, wantRoles) {
		t.Fatalf("roles = %v, want %v", got, wantRoles)
	}
	if text, ok := fantasy.AsMessagePart[fantasy.TextPart](call.Prompt[0].Content[0]); !ok || text.Text != "be terse" {
		t.Errorf("system message = %#v, want the system prompt", call.Prompt[0].Content[0])
	}
	toolCall, ok := fantasy.AsMessagePart[fantasy.ToolCallPart](call.Prompt[2].Content[0])
	if !ok {
		t.Fatalf("assistant part = %#v, want a ToolCallPart", call.Prompt[2].Content[0])
	}
	if toolCall.ToolCallID != "t1" || toolCall.ToolName != "read_file" || toolCall.Input != `{"path":"a.go"}` {
		t.Errorf("tool call = %+v, want id t1 / read_file / the raw input JSON", toolCall)
	}
}

// TestGenerate_ToolResultsBecomeTheirOwnMessage: meat follows the Anthropic
// wire shape and carries tool_result blocks in a user message; fantasy gives
// them role "tool". The results must lead, because once fantasy regroups them
// into one user turn Anthropic requires tool results before user text.
func TestGenerate_ToolResultsBecomeTheirOwnMessage(t *testing.T) {
	f := &fakeLM{resp: okResponse()}
	m := newTestModel(f)

	_, err := m.Generate(context.Background(), "", []meat.Message{
		{Role: meat.RoleUser, Content: []meat.Block{
			{Type: "text", Text: "and now this"},
			{Type: "tool_result", ToolUseID: "t1", ToolResult: "file contents"},
			{Type: "tool_result", ToolUseID: "t2", ToolResult: "boom", ToolError: true},
		}},
	}, nil)
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}

	prompt := f.calls[0].Prompt
	wantRoles := []fantasy.MessageRole{fantasy.MessageRoleTool, fantasy.MessageRoleUser}
	if got := roles(prompt); !equalRoles(got, wantRoles) {
		t.Fatalf("roles = %v, want %v (results first)", got, wantRoles)
	}
	if len(prompt[0].Content) != 2 {
		t.Fatalf("tool message parts = %d, want 2", len(prompt[0].Content))
	}

	first, _ := fantasy.AsMessagePart[fantasy.ToolResultPart](prompt[0].Content[0])
	text, ok := fantasy.AsToolResultOutputType[fantasy.ToolResultOutputContentText](first.Output)
	if !ok || text.Text != "file contents" {
		t.Errorf("first result = %#v, want text output", first.Output)
	}

	second, _ := fantasy.AsMessagePart[fantasy.ToolResultPart](prompt[0].Content[1])
	errOut, ok := fantasy.AsToolResultOutputType[fantasy.ToolResultOutputContentError](second.Output)
	if !ok {
		t.Fatalf("second result = %#v, want error output", second.Output)
	}
	if errOut.Error.Error() != "boom" {
		t.Errorf("error text = %q, want %q", errOut.Error.Error(), "boom")
	}
}

// TestGenerate_EmptyToolInputBecomesEmptyObject: fantasy carries tool input as
// a JSON string, and "" is not JSON.
func TestGenerate_EmptyToolInputBecomesEmptyObject(t *testing.T) {
	f := &fakeLM{resp: okResponse()}
	m := newTestModel(f)

	_, err := m.Generate(context.Background(), "", []meat.Message{
		{Role: meat.RoleAssistant, Content: []meat.Block{{Type: "tool_use", ID: "t1", ToolName: "submit"}}},
	}, nil)
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	part, _ := fantasy.AsMessagePart[fantasy.ToolCallPart](f.calls[0].Prompt[0].Content[0])
	if part.Input != "{}" {
		t.Errorf("input = %q, want %q", part.Input, "{}")
	}
}

// TestGenerate_ProviderStateIsRefused: meat documents provider_state as opaque
// state to be replayed exactly. fantasy has no equivalent, and an approximate
// translation would corrupt a multi-turn loop without saying so.
func TestGenerate_ProviderStateIsRefused(t *testing.T) {
	f := &fakeLM{resp: okResponse()}
	m := newTestModel(f)

	_, err := m.Generate(context.Background(), "", []meat.Message{
		{Role: meat.RoleAssistant, Content: []meat.Block{
			{Type: "provider_state", Provider: "openai", ProviderData: json.RawMessage(`{"x":1}`)},
		}},
	}, nil)
	if err == nil {
		t.Fatal("want an error for a provider_state block")
	}
	if !strings.Contains(err.Error(), "provider_state") {
		t.Errorf("error = %v, want it to name provider_state", err)
	}
	if len(f.calls) != 0 {
		t.Errorf("calls = %d, want 0: nothing should reach the provider", len(f.calls))
	}
}

// TestGenerate_UnknownBlockIsRefused: a dropped block is a hole in a
// conversation the model is about to reason over.
func TestGenerate_UnknownBlockIsRefused(t *testing.T) {
	f := &fakeLM{resp: okResponse()}
	if _, err := newTestModel(f).Generate(context.Background(), "", []meat.Message{
		{Role: meat.RoleUser, Content: []meat.Block{{Type: "image"}}},
	}, nil); err == nil {
		t.Fatal("want an error for an unknown block type")
	}
}

// TestGenerate_LengthFinishIsAnError mirrors meat's own behavior: a reply cut
// off at the output cap is likely truncated inside a tool_use JSON block, and
// using it would cache a truncated abridgement.
func TestGenerate_LengthFinishIsAnError(t *testing.T) {
	f := &fakeLM{resp: &fantasy.Response{
		Content:      fantasy.ResponseContent{fantasy.TextContent{Text: "half a th"}},
		FinishReason: fantasy.FinishReasonLength,
	}}
	_, err := newTestModel(f).Generate(context.Background(), "", nil, nil)
	if err == nil {
		t.Fatal("want an error when the response stops at the output cap")
	}
	if !strings.Contains(err.Error(), "truncated") {
		t.Errorf("error = %v, want it to say the response was truncated", err)
	}
}

// TestGenerate_PreservesContentOrder: meat appends this content verbatim as the
// next assistant message, so a reordered tool_use would no longer line up with
// the tool_result answering it. fantasy's Text() helper returns only the first
// text part, which is why the adapter walks Content itself.
func TestGenerate_PreservesContentOrder(t *testing.T) {
	f := &fakeLM{resp: &fantasy.Response{
		Content: fantasy.ResponseContent{
			fantasy.TextContent{Text: "first"},
			fantasy.ToolCallContent{ToolCallID: "t1", ToolName: "grep", Input: `{"pattern":"x"}`},
			fantasy.TextContent{Text: "second"},
			fantasy.ReasoningContent{Text: "ignored"},
		},
		FinishReason: fantasy.FinishReasonToolCalls,
		Usage:        fantasy.Usage{InputTokens: 3, OutputTokens: 4},
	}}

	resp, err := newTestModel(f).Generate(context.Background(), "", nil, nil)
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	want := []meat.Block{
		{Type: "text", Text: "first"},
		{Type: "tool_use", ID: "t1", ToolName: "grep", ToolInput: json.RawMessage(`{"pattern":"x"}`)},
		{Type: "text", Text: "second"},
	}
	if len(resp.Content) != len(want) {
		t.Fatalf("content = %d blocks, want %d: %+v", len(resp.Content), len(want), resp.Content)
	}
	for i := range want {
		if resp.Content[i].Type != want[i].Type || resp.Content[i].Text != want[i].Text ||
			resp.Content[i].ID != want[i].ID || resp.Content[i].ToolName != want[i].ToolName ||
			string(resp.Content[i].ToolInput) != string(want[i].ToolInput) {
			t.Errorf("block %d = %+v, want %+v", i, resp.Content[i], want[i])
		}
	}
	if resp.InputTokens != 3 || resp.OutputTokens != 4 {
		t.Errorf("tokens = %d/%d, want 3/4", resp.InputTokens, resp.OutputTokens)
	}
}

// TestGenerate_InvalidToolCallIsRefused: passing arguments the provider already
// failed to parse only moves the failure a layer deeper, into meat's strict
// decode, where the error says less.
func TestGenerate_InvalidToolCallIsRefused(t *testing.T) {
	f := &fakeLM{resp: &fantasy.Response{
		Content: fantasy.ResponseContent{fantasy.ToolCallContent{
			ToolCallID:      "t1",
			ToolName:        "submit",
			Input:           `{"summary":`,
			Invalid:         true,
			ValidationError: errors.New("unexpected end of JSON input"),
		}},
		FinishReason: fantasy.FinishReasonToolCalls,
	}}
	if _, err := newTestModel(f).Generate(context.Background(), "", nil, nil); err == nil {
		t.Fatal("want an error for an invalid tool call")
	}
}

// TestGenerate_RetriesRetryableErrors: fantasy builds its Anthropic client with
// MaxRetries(0) and applies backoff only inside fantasy.Agent, so routing meat
// through a bare LanguageModel bypasses meat's own postJSONWithRetry and lands
// on nothing. This asserts the adapter puts retry back.
func TestGenerate_RetriesRetryableErrors(t *testing.T) {
	f := &fakeLM{
		resp: okResponse(),
		errs: []error{&fantasy.ProviderError{StatusCode: http.StatusTooManyRequests}},
	}
	if _, err := newTestModel(f).Generate(context.Background(), "", nil, nil); err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if len(f.calls) != 2 {
		t.Errorf("calls = %d, want 2 (one failure, one retry)", len(f.calls))
	}
}

// TestToTools_RequiredSurvivesAsStringSlice pins a fantasy quirk, not a
// preference. Its Anthropic provider lifts `required` out of the schema with a
// req.([]string) type assertion; a schema decoded from JSON yields []any there,
// so an unnormalized `required` is dropped and the model is told nothing is
// mandatory. If this test starts failing, check whether fantasy fixed it before
// removing the normalization.
func TestToTools_RequiredSurvivesAsStringSlice(t *testing.T) {
	tools, err := toTools([]meat.Tool{{
		Name:        "read_file",
		Description: "read a file",
		InputSchema: json.RawMessage(`{"type":"object","additionalProperties":false,"properties":{"path":{"type":"string"}},"required":["path"]}`),
	}})
	if err != nil {
		t.Fatalf("toTools: %v", err)
	}
	fn, ok := tools[0].(fantasy.FunctionTool)
	if !ok {
		t.Fatalf("tool = %T, want a FunctionTool", tools[0])
	}
	required, ok := fn.InputSchema["required"].([]string)
	if !ok {
		t.Fatalf("required = %T, want []string; fantasy's provider drops any other type", fn.InputSchema["required"])
	}
	if len(required) != 1 || required[0] != "path" {
		t.Errorf("required = %v, want [path]", required)
	}
	if fn.Name != "read_file" || fn.Description != "read a file" {
		t.Errorf("tool = %+v, want name and description carried through", fn)
	}
}

// TestToTools_MeatsRealToolsTranslate guards against a schema in meat's own
// toolbox that the adapter cannot carry.
func TestToTools_MeatsRealToolsTranslate(t *testing.T) {
	tools, err := toTools([]meat.Tool{
		{Name: "read_file", InputSchema: json.RawMessage(`{"type":"object","properties":{"path":{"type":"string"}},"required":["path"]}`)},
		{Name: "grep", InputSchema: json.RawMessage(`{"type":"object","properties":{"pattern":{"type":"string"}},"required":["pattern"]}`)},
		{Name: "no_schema"},
	})
	if err != nil {
		t.Fatalf("toTools: %v", err)
	}
	if len(tools) != 3 {
		t.Fatalf("tools = %d, want 3", len(tools))
	}
	if fn := tools[2].(fantasy.FunctionTool); fn.InputSchema == nil {
		t.Error("a tool with no schema should still get an empty schema map, not nil")
	}
}

func TestToTools_BadSchemaIsAnError(t *testing.T) {
	if _, err := toTools([]meat.Tool{{Name: "x", InputSchema: json.RawMessage(`{`)}}); err == nil {
		t.Fatal("want an error for a malformed schema")
	}
	if _, err := toTools([]meat.Tool{{Name: "x", InputSchema: json.RawMessage(`{"required":[1]}`)}}); err == nil {
		t.Fatal("want an error for a non-string entry in required")
	}
}

// TestPinnedHost_RefusesEverythingElse covers the structural half of the
// no-fallback guarantee: whatever a provider decides about credentials, a
// request addressed at the public API cannot leave the process.
func TestPinnedHost_RefusesEverythingElse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	client := &http.Client{Transport: &pinnedHost{host: "bedrock-runtime.us-east-1.amazonaws.com", rt: http.DefaultTransport}}

	for _, url := range []string{
		"https://api.anthropic.com/v1/messages",
		"https://bedrock-runtime.us-west-2.amazonaws.com/v1/messages",
		"http://bedrock-runtime.us-east-1.amazonaws.com/v1/messages", // plaintext
		srv.URL,
	} {
		if _, err := client.Get(url); err == nil {
			t.Errorf("%s: want a refusal", url)
		} else if !strings.Contains(err.Error(), "pinned to") {
			t.Errorf("%s: error = %v, want the pinning refusal", url, err)
		}
	}
}

func TestBedrockHost(t *testing.T) {
	// Mirrors the endpoint anthropic-sdk-go's bedrock.WithConfig builds; if
	// that format ever changes, pinnedHost would refuse real traffic, so this
	// is the tripwire.
	if got := bedrockHost("us-gov-west-1"); got != "bedrock-runtime.us-gov-west-1.amazonaws.com" {
		t.Errorf("bedrockHost = %q", got)
	}
}

func roles(prompt fantasy.Prompt) []fantasy.MessageRole {
	out := make([]fantasy.MessageRole, len(prompt))
	for i, m := range prompt {
		out[i] = m.Role
	}
	return out
}

func equalRoles(a, b []fantasy.MessageRole) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
