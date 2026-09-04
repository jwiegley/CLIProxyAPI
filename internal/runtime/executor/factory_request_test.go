package executor

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
)

func newFactoryRequestTestExecutor() *FactoryExecutor {
	executor := NewFactoryExecutor(&config.Config{Factory: config.FactoryConfig{Enabled: true, CWD: "/workspace"}})
	executor.models = map[string]factoryBridgeModel{
		"model-a": {ID: "model-a", ReasoningEfforts: []string{"off", "low", "high"}},
		"auto":    {ID: "auto", ReasoningEfforts: []string{"none"}},
	}
	return executor
}

func TestPrepareFactoryRequestCarriesConfiguredAgentControls(t *testing.T) {
	executor := NewFactoryExecutor(&config.Config{Factory: config.FactoryConfig{
		Enabled:             true,
		Autonomy:            "high",
		Tools:               []string{"Read", "Execute"},
		EnableBuiltinSkills: true,
	}})
	executor.models = map[string]factoryBridgeModel{"model-a": {ID: "model-a"}}
	body := []byte(`{"model":"factory/model-a","messages":[{"role":"user","content":"Hi"}]}`)
	run, err := executor.prepareRunRequest(
		cliproxyexecutor.Request{Model: "model-a", Payload: body},
		cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatOpenAI},
	)
	if err != nil {
		t.Fatalf("prepareRunRequest() error = %v", err)
	}
	if run.Autonomy != "high" || strings.Join(run.Tools, ",") != "Read,Execute" || !run.EnableBuiltinSkills {
		t.Fatalf("agent controls = %#v", run)
	}
}

func TestPrepareFactoryChatRequest(t *testing.T) {
	executor := newFactoryRequestTestExecutor()
	body := []byte(`{
		"model":"factory/model-a",
		"messages":[
			{"role":"system","content":"System one"},
			{"role":"developer","content":[{"type":"text","text":"System two"}]},
			{"role":"user","content":"Hello"}
		],
		"reasoning_effort":"low",
		"response_format":{"type":"json_schema","json_schema":{"name":"answer","strict":true,"schema":{"type":"object","required":["ok"],"properties":{"ok":{"type":"boolean"}}}}},
		"stream":true,
		"stream_options":{"include_usage":true}
	}`)
	run, err := executor.prepareRunRequest(
		cliproxyexecutor.Request{Model: "model-a", Payload: body},
		cliproxyexecutor.Options{
			SourceFormat: sdktranslator.FormatOpenAI,
			Metadata: map[string]any{
				cliproxyexecutor.ReasoningEffortMetadataKey: "HIGH",
			},
		},
	)
	if err != nil {
		t.Fatalf("prepareRunRequest() error = %v", err)
	}
	if run.Operation != "run" || run.Model != "model-a" || run.Prompt != "Hello" {
		t.Fatalf("run identity/content = %#v", run)
	}
	if run.SystemPrompt != "System one\n\nSystem two" {
		t.Fatalf("SystemPrompt = %q", run.SystemPrompt)
	}
	if run.ReasoningEffort != "high" {
		t.Fatalf("ReasoningEffort = %q, want high", run.ReasoningEffort)
	}
	if run.OutputSchema["type"] != "object" || run.OutputSchema["title"] != "answer" || !run.IncludeUsage {
		t.Fatalf("structured stream options = %#v include_usage=%t", run.OutputSchema, run.IncludeUsage)
	}
	if run.DroidCommand != "droid" || run.CWD != "/workspace" {
		t.Fatalf("runtime config = %#v", run)
	}
}

func TestPrepareFactoryChatAttachments(t *testing.T) {
	body := []byte(`{
		"model":"factory/model-a",
		"messages":[{"role":"user","content":[
			{"type":"text","text":"Inspect these"},
			{"type":"image_url","image_url":{"url":"data:image/png;base64,iVBORw0KGgo=","detail":"auto"}},
			{"type":"file","file":{"filename":"notes.txt","file_data":"data:text/plain;base64,aGVsbG8="}},
			{"type":"file","file":{"filename":"paper.pdf","file_data":"data:application/pdf;base64,JVBERi0xLjQ="}}
		]}]
	}`)
	run, err := newFactoryRequestTestExecutor().prepareRunRequest(
		cliproxyexecutor.Request{Model: "model-a", Payload: body},
		cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatOpenAI},
	)
	if err != nil {
		t.Fatalf("prepareRunRequest() error = %v", err)
	}
	if run.Prompt != "Inspect these" || len(run.Images) != 1 || len(run.Files) != 2 {
		t.Fatalf("attachment request = %#v", run)
	}
	if run.Images[0].MediaType != "image/png" || run.Images[0].Data != "iVBORw0KGgo=" {
		t.Fatalf("image = %#v", run.Images[0])
	}
	if run.Files[0].Kind != "text" || run.Files[0].Data != "hello" || run.Files[0].Name != "notes.txt" {
		t.Fatalf("text file = %#v", run.Files[0])
	}
	if run.Files[1].Kind != "pdf" || run.Files[1].Name != "paper.pdf" {
		t.Fatalf("PDF file = %#v", run.Files[1])
	}
}

func TestPrepareFactoryResponsesRequests(t *testing.T) {
	tests := []struct {
		name       string
		body       string
		wantPrompt string
		wantSystem string
		wantImages int
		wantFiles  int
		wantSchema bool
	}{
		{
			name:       "string input",
			body:       `{"model":"factory/model-a","instructions":"Be concise","input":"Hello","reasoning":{"effort":"low"},"text":{"format":{"type":"json_object"}}}`,
			wantPrompt: "Hello",
			wantSystem: "Be concise",
			wantSchema: true,
		},
		{
			name: "message input",
			body: `{"model":"factory/model-a","instructions":"Top","input":[
				{"type":"message","role":"developer","content":[{"type":"input_text","text":"Nested"}]},
				{"type":"message","role":"user","content":[{"type":"input_text","text":"Look"},{"type":"input_image","image_url":"data:image/jpeg;base64,/9j/","detail":"auto"},{"type":"input_file","filename":"a.txt","file_data":"data:text/plain;base64,YQ=="}]}
			]}`,
			wantPrompt: "Look",
			wantSystem: "Top\n\nNested",
			wantImages: 1,
			wantFiles:  1,
		},
		{
			name:       "direct content parts",
			body:       `{"model":"factory/model-a","input":[{"type":"input_text","text":"A"},{"type":"input_text","text":"B"}]}`,
			wantPrompt: "A\nB",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			run, err := newFactoryRequestTestExecutor().prepareRunRequest(
				cliproxyexecutor.Request{Model: "model-a", Payload: []byte(test.body)},
				cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatOpenAIResponse},
			)
			if err != nil {
				t.Fatalf("prepareRunRequest() error = %v", err)
			}
			if run.Prompt != test.wantPrompt || run.SystemPrompt != test.wantSystem || len(run.Images) != test.wantImages || len(run.Files) != test.wantFiles {
				t.Fatalf("run = %#v", run)
			}
			if (run.OutputSchema != nil) != test.wantSchema {
				t.Fatalf("OutputSchema = %#v", run.OutputSchema)
			}
		})
	}
}

func TestPrepareFactoryCompletionUsesOriginalRouteRequest(t *testing.T) {
	original := []byte(`{"model":"factory/model-a","prompt":"Complete me","stream":true}`)
	run, err := newFactoryRequestTestExecutor().prepareRunRequest(
		cliproxyexecutor.Request{Model: "model-a", Payload: []byte(`{"model":"factory/model-a","messages":[{"role":"user","content":"converted"}],"stream":true}`)},
		cliproxyexecutor.Options{
			SourceFormat: sdktranslator.FormatOpenAI,
			Metadata: map[string]any{
				cliproxyexecutor.RequestPathMetadataKey:             "/v1/completions",
				cliproxyexecutor.OriginalEndpointRequestMetadataKey: original,
			},
		},
	)
	if err != nil {
		t.Fatalf("prepareRunRequest() error = %v", err)
	}
	if run.Prompt != "Complete me" {
		t.Fatalf("Prompt = %q", run.Prompt)
	}
}

func TestPrepareFactoryRequestRejectsUnsupportedOrInvalidInput(t *testing.T) {
	validChat := `{"model":"factory/model-a","messages":[{"role":"user","content":"Hello"}]}`
	tests := []struct {
		name      string
		body      string
		format    sdktranslator.Format
		path      string
		alt       string
		original  string
		model     string
		wantParam string
		wantCode  string
	}{
		{name: "unknown top-level field", body: `{"model":"factory/model-a","messages":[{"role":"user","content":"x"}],"future_option":true}`, format: sdktranslator.FormatOpenAI, model: "model-a", wantParam: "future_option", wantCode: "unsupported_parameter"},
		{name: "client tools", body: `{"model":"factory/model-a","messages":[{"role":"user","content":"x"}],"tools":[]}`, format: sdktranslator.FormatOpenAI, model: "model-a", wantParam: "tools", wantCode: "unsupported_parameter"},
		{name: "sampling", body: `{"model":"factory/model-a","messages":[{"role":"user","content":"x"}],"temperature":0.2}`, format: sdktranslator.FormatOpenAI, model: "model-a", wantParam: "temperature", wantCode: "unsupported_parameter"},
		{name: "assistant history", body: `{"model":"factory/model-a","messages":[{"role":"assistant","content":"old"},{"role":"user","content":"new"}]}`, format: sdktranslator.FormatOpenAI, model: "model-a", wantParam: "messages.0.role", wantCode: "unsupported_parameter"},
		{name: "multiple user turns", body: `{"model":"factory/model-a","messages":[{"role":"user","content":"one"},{"role":"user","content":"two"}]}`, format: sdktranslator.FormatOpenAI, model: "model-a", wantParam: "messages.0.role", wantCode: "invalid_value"},
		{name: "remote image", body: `{"model":"factory/model-a","messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"https://example.com/a.png"}}]}]}`, format: sdktranslator.FormatOpenAI, model: "model-a", wantParam: "messages.0.content.0.image_url.url", wantCode: "unsupported_parameter"},
		{name: "image detail", body: `{"model":"factory/model-a","messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"data:image/png;base64,iVBORw0KGgo=","detail":"high"}}]}]}`, format: sdktranslator.FormatOpenAI, model: "model-a", wantParam: "messages.0.content.0.image_url.detail", wantCode: "unsupported_parameter"},
		{name: "invalid base64", body: `{"model":"factory/model-a","messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"data:image/png;base64,not-base64"}}]}]}`, format: sdktranslator.FormatOpenAI, model: "model-a", wantParam: "messages.0.content.0.image_url.url", wantCode: "invalid_value"},
		{name: "schema description", body: `{"model":"factory/model-a","messages":[{"role":"user","content":"x"}],"response_format":{"type":"json_schema","json_schema":{"name":"x","description":"ignored","schema":{"type":"object"}}}}`, format: sdktranslator.FormatOpenAI, model: "model-a", wantParam: "response_format.json_schema.description", wantCode: "unsupported_parameter"},
		{name: "schema name title conflict", body: `{"model":"factory/model-a","messages":[{"role":"user","content":"x"}],"response_format":{"type":"json_schema","json_schema":{"name":"answer","schema":{"type":"object","title":"different"}}}}`, format: sdktranslator.FormatOpenAI, model: "model-a", wantParam: "response_format.json_schema.name", wantCode: "invalid_value"},
		{name: "non-strict schema", body: `{"model":"factory/model-a","messages":[{"role":"user","content":"x"}],"response_format":{"type":"json_schema","json_schema":{"name":"x","strict":false,"schema":{"type":"object"}}}}`, format: sdktranslator.FormatOpenAI, model: "model-a", wantParam: "response_format.json_schema.strict", wantCode: "unsupported_parameter"},
		{name: "stored response", body: `{"model":"factory/model-a","input":"x","previous_response_id":"resp_1"}`, format: sdktranslator.FormatOpenAIResponse, model: "model-a", wantParam: "previous_response_id", wantCode: "unsupported_parameter"},
		{name: "response compaction", body: `{"model":"factory/model-a","input":"x"}`, format: sdktranslator.FormatOpenAIResponse, alt: "responses/compact", model: "model-a", wantParam: "endpoint", wantCode: "unsupported_parameter"},
		{name: "reasoning summary", body: `{"model":"factory/model-a","input":"x","reasoning":{"effort":"low","summary":"auto"}}`, format: sdktranslator.FormatOpenAIResponse, model: "model-a", wantParam: "reasoning.summary", wantCode: "unsupported_parameter"},
		{name: "remote file id", body: `{"model":"factory/model-a","input":[{"type":"input_file","file_id":"file_1"}]}`, format: sdktranslator.FormatOpenAIResponse, model: "model-a", wantParam: "input.0.file_id", wantCode: "unsupported_parameter"},
		{name: "completion token limit", body: validChat, format: sdktranslator.FormatOpenAI, path: "/v1/completions", original: `{"model":"factory/model-a","prompt":"x","max_tokens":10}`, model: "model-a", wantParam: "max_tokens", wantCode: "unsupported_parameter"},
		{name: "completion prompt array", body: validChat, format: sdktranslator.FormatOpenAI, path: "/v1/completions", original: `{"model":"factory/model-a","prompt":["x"]}`, model: "model-a", wantParam: "prompt", wantCode: "invalid_value"},
		{name: "unsupported reasoning effort", body: `{"model":"factory/model-a","messages":[{"role":"user","content":"x"}],"reasoning_effort":"max"}`, format: sdktranslator.FormatOpenAI, model: "model-a", wantParam: "reasoning_effort", wantCode: "invalid_value"},
		{name: "unknown model", body: validChat, format: sdktranslator.FormatOpenAI, model: "missing-model", wantParam: "model", wantCode: "invalid_value"},
		{name: "unsupported protocol", body: validChat, format: sdktranslator.FormatClaude, model: "model-a", wantParam: "protocol", wantCode: "unsupported_parameter"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			metadata := map[string]any{}
			if test.path != "" {
				metadata[cliproxyexecutor.RequestPathMetadataKey] = test.path
				metadata[cliproxyexecutor.OriginalEndpointRequestMetadataKey] = []byte(test.original)
			}
			_, err := newFactoryRequestTestExecutor().prepareRunRequest(
				cliproxyexecutor.Request{Model: test.model, Payload: []byte(test.body)},
				cliproxyexecutor.Options{SourceFormat: test.format, Alt: test.alt, Metadata: metadata},
			)
			if err == nil {
				t.Fatal("prepareRunRequest() error = nil")
			}
			var requestErr *factoryRequestError
			if !errors.As(err, &requestErr) {
				t.Fatalf("error type = %T, want *factoryRequestError: %v", err, err)
			}
			if requestErr.Param != test.wantParam || requestErr.Code != test.wantCode {
				t.Fatalf("error = %#v, want param=%q code=%q", requestErr, test.wantParam, test.wantCode)
			}
			if requestErr.StatusCode() != 400 || !requestErr.IsRequestScoped() || !json.Valid([]byte(requestErr.Error())) {
				t.Fatalf("invalid request error contract: %v", requestErr)
			}
			if !strings.Contains(requestErr.Error(), `"type":"invalid_request_error"`) {
				t.Fatalf("error body = %s", requestErr.Error())
			}
		})
	}
}
