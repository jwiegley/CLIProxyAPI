package executor

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	_ "github.com/router-for-me/CLIProxyAPI/v7/internal/translator"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
	"github.com/tidwall/gjson"
)

type recordingFactoryBridge struct {
	mu       sync.Mutex
	requests []factoryRunRequest
	run      func(factoryRunRequest) []factoryBridgeEvent
	err      error
}

func (*recordingFactoryBridge) ListModels(context.Context) ([]factoryBridgeModel, error) {
	return nil, nil
}

func (b *recordingFactoryBridge) Run(_ context.Context, request factoryRunRequest) (*factoryBridgeRun, error) {
	if b.err != nil {
		return nil, b.err
	}
	b.mu.Lock()
	b.requests = append(b.requests, request)
	b.mu.Unlock()
	events := make(chan factoryBridgeEvent, 8)
	done := make(chan struct{})
	for _, event := range b.run(request) {
		events <- event
	}
	close(events)
	close(done)
	return &factoryBridgeRun{Events: events, Done: done}, nil
}

func (b *recordingFactoryBridge) request(index int) factoryRunRequest {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.requests[index]
}

func newFactoryResponseTestExecutor(bridge factoryBridge) *FactoryExecutor {
	executor := newFactoryRequestTestExecutor()
	executor.bridge = bridge
	return executor
}

func successfulFactoryEvents(text string) []factoryBridgeEvent {
	return []factoryBridgeEvent{
		{Type: "text_delta", Text: text[:len(text)/2]},
		{Type: "text_delta", Text: text[len(text)/2:]},
		{
			Type:    "result",
			Success: true,
			Subtype: "success",
			Text:    text,
			Usage: &factoryBridgeUsage{
				InputTokens:         11,
				OutputTokens:        5,
				CacheCreationTokens: 2,
				CacheReadTokens:     3,
				ThinkingTokens:      1,
			},
		},
	}
}

func TestFactoryExecuteChatCompletion(t *testing.T) {
	bridge := &recordingFactoryBridge{run: func(factoryRunRequest) []factoryBridgeEvent {
		return successfulFactoryEvents("Hello")
	}}
	executor := newFactoryResponseTestExecutor(bridge)
	body := []byte(`{"model":"factory/model-a","messages":[{"role":"user","content":"Hi"}]}`)
	response, err := executor.Execute(
		context.Background(),
		nil,
		cliproxyexecutor.Request{Model: "model-a", Payload: body},
		cliproxyexecutor.Options{
			SourceFormat:    sdktranslator.FormatOpenAI,
			ResponseFormat:  sdktranslator.FormatOpenAI,
			OriginalRequest: body,
			Metadata: map[string]any{
				cliproxyexecutor.RequestedModelMetadataKey: "factory/model-a",
			},
		},
	)
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if got := gjson.GetBytes(response.Payload, "object").String(); got != "chat.completion" {
		t.Fatalf("object = %q; body=%s", got, response.Payload)
	}
	if got := gjson.GetBytes(response.Payload, "model").String(); got != "factory/model-a" {
		t.Fatalf("model = %q", got)
	}
	if got := gjson.GetBytes(response.Payload, "choices.0.message.content").String(); got != "Hello" {
		t.Fatalf("content = %q", got)
	}
	if got := gjson.GetBytes(response.Payload, "usage.total_tokens").Int(); got != 16 {
		t.Fatalf("total_tokens = %d", got)
	}
	if got := gjson.GetBytes(response.Payload, "usage.prompt_tokens_details.cached_tokens").Int(); got != 3 {
		t.Fatalf("cached_tokens = %d", got)
	}
	if request := bridge.request(0); request.Prompt != "Hi" || request.Model != "model-a" {
		t.Fatalf("bridge request = %#v", request)
	}
}

func TestFactoryExecuteStructuredResponsesResult(t *testing.T) {
	bridge := &recordingFactoryBridge{run: func(factoryRunRequest) []factoryBridgeEvent {
		return []factoryBridgeEvent{{
			Type:             "result",
			Success:          true,
			Subtype:          "success",
			Text:             `{"ignored":true}`,
			StructuredOutput: map[string]any{"ok": true},
			Usage:            &factoryBridgeUsage{InputTokens: 4, OutputTokens: 2},
		}}
	}}
	executor := newFactoryResponseTestExecutor(bridge)
	body := []byte(`{"model":"factory/model-a","input":"Hi","text":{"format":{"type":"json_object"}}}`)
	response, err := executor.Execute(
		context.Background(),
		nil,
		cliproxyexecutor.Request{Model: "model-a", Payload: body},
		cliproxyexecutor.Options{
			SourceFormat:    sdktranslator.FormatOpenAIResponse,
			ResponseFormat:  sdktranslator.FormatOpenAIResponse,
			OriginalRequest: body,
			Metadata: map[string]any{
				cliproxyexecutor.RequestedModelMetadataKey: "factory/model-a",
			},
		},
	)
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if got := gjson.GetBytes(response.Payload, "object").String(); got != "response" {
		t.Fatalf("object = %q; body=%s", got, response.Payload)
	}
	if got := gjson.GetBytes(response.Payload, "status").String(); got != "completed" {
		t.Fatalf("status = %q; body=%s", got, response.Payload)
	}
	if got := gjson.GetBytes(response.Payload, "output.0.content.0.text").String(); got != `{"ok":true}` {
		t.Fatalf("output text = %q; body=%s", got, response.Payload)
	}
	if got := gjson.GetBytes(response.Payload, "usage.total_tokens").Int(); got != 6 {
		t.Fatalf("total_tokens = %d; body=%s", got, response.Payload)
	}
}

func TestFactoryExecuteChatStreamOrderingAndUsage(t *testing.T) {
	bridge := &recordingFactoryBridge{run: func(factoryRunRequest) []factoryBridgeEvent {
		return successfulFactoryEvents("Hello")
	}}
	executor := newFactoryResponseTestExecutor(bridge)
	body := []byte(`{"model":"factory/model-a","messages":[{"role":"user","content":"Hi"}],"stream":true,"stream_options":{"include_usage":true}}`)
	stream, err := executor.ExecuteStream(
		context.Background(), nil,
		cliproxyexecutor.Request{Model: "model-a", Payload: body},
		cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatOpenAI, ResponseFormat: sdktranslator.FormatOpenAI, OriginalRequest: body},
	)
	if err != nil {
		t.Fatalf("ExecuteStream() error = %v", err)
	}
	chunks := collectFactoryStream(t, stream)
	if len(chunks) != 5 {
		t.Fatalf("chunk count = %d, want 5: %q", len(chunks), chunks)
	}
	if got := gjson.GetBytes(chunks[0], "choices.0.delta.role").String(); got != "assistant" {
		t.Fatalf("first role = %q", got)
	}
	if got := gjson.GetBytes(chunks[1], "choices.0.delta.content").String() + gjson.GetBytes(chunks[2], "choices.0.delta.content").String(); got != "Hello" {
		t.Fatalf("streamed content = %q", got)
	}
	if got := gjson.GetBytes(chunks[3], "choices.0.finish_reason").String(); got != "stop" {
		t.Fatalf("finish_reason = %q", got)
	}
	if got := gjson.GetBytes(chunks[4], "usage.total_tokens").Int(); got != 16 {
		t.Fatalf("stream usage total = %d", got)
	}
	id := gjson.GetBytes(chunks[0], "id").String()
	for _, chunk := range chunks[1:] {
		if got := gjson.GetBytes(chunk, "id").String(); got != id {
			t.Fatalf("chunk id = %q, want %q", got, id)
		}
	}
}

func TestFactoryExecuteResponsesStreamLifecycle(t *testing.T) {
	bridge := &recordingFactoryBridge{run: func(factoryRunRequest) []factoryBridgeEvent {
		return successfulFactoryEvents("Hello")
	}}
	executor := newFactoryResponseTestExecutor(bridge)
	body := []byte(`{"model":"factory/model-a","input":"Hi","stream":true}`)
	stream, err := executor.ExecuteStream(
		context.Background(), nil,
		cliproxyexecutor.Request{Model: "model-a", Payload: body},
		cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatOpenAIResponse, ResponseFormat: sdktranslator.FormatOpenAIResponse, OriginalRequest: body},
	)
	if err != nil {
		t.Fatalf("ExecuteStream() error = %v", err)
	}
	chunks := collectFactoryStream(t, stream)
	events := factorySSEEventTypes(chunks)
	want := []string{
		"response.created",
		"response.in_progress",
		"response.output_item.added",
		"response.content_part.added",
		"response.output_text.delta",
		"response.output_text.delta",
		"response.output_text.done",
		"response.content_part.done",
		"response.output_item.done",
		"response.completed",
	}
	if strings.Join(events, ",") != strings.Join(want, ",") {
		t.Fatalf("SSE events = %#v, want %#v; chunks=%q", events, want, chunks)
	}
	last := chunks[len(chunks)-1]
	if !strings.Contains(string(last), `"status":"completed"`) || !strings.Contains(string(last), `"total_tokens":16`) {
		t.Fatalf("terminal Responses event = %s", last)
	}
}

func TestFactoryExecuteBridgeErrorBeforePayload(t *testing.T) {
	bridge := &recordingFactoryBridge{run: func(factoryRunRequest) []factoryBridgeEvent {
		return []factoryBridgeEvent{{Type: "error", Code: "sdk_unavailable", Message: "install droid-sdk"}}
	}}
	executor := newFactoryResponseTestExecutor(bridge)
	body := []byte(`{"model":"factory/model-a","messages":[{"role":"user","content":"Hi"}],"stream":true}`)
	stream, err := executor.ExecuteStream(context.Background(), nil, cliproxyexecutor.Request{Model: "model-a", Payload: body}, cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatOpenAI})
	if err != nil {
		t.Fatalf("ExecuteStream() error = %v", err)
	}
	chunk := <-stream.Chunks
	if chunk.Payload != nil || chunk.Err == nil {
		t.Fatalf("first chunk = %#v", chunk)
	}
	var status interface{ StatusCode() int }
	if !errors.As(chunk.Err, &status) || status.StatusCode() != 503 {
		t.Fatalf("stream error = %v", chunk.Err)
	}
	if _, ok := <-stream.Chunks; ok {
		t.Fatal("stream emitted data after terminal error")
	}
}

func TestFactoryExecuteMapsSDKFailure(t *testing.T) {
	bridge := &recordingFactoryBridge{run: func(factoryRunRequest) []factoryBridgeEvent {
		return []factoryBridgeEvent{{Type: "result", Subtype: "error_during_execution", Message: "Factory execution failed"}}
	}}
	executor := newFactoryResponseTestExecutor(bridge)
	body := []byte(`{"model":"factory/model-a","messages":[{"role":"user","content":"Hi"}]}`)
	_, err := executor.Execute(context.Background(), nil, cliproxyexecutor.Request{Model: "model-a", Payload: body}, cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatOpenAI})
	var status interface{ StatusCode() int }
	if !errors.As(err, &status) || status.StatusCode() != 502 || !strings.Contains(err.Error(), "Factory execution failed") {
		t.Fatalf("Execute() error = %v", err)
	}
}

func TestFactoryExecuteMapsInterruptedDroidResult(t *testing.T) {
	bridge := &recordingFactoryBridge{run: func(factoryRunRequest) []factoryBridgeEvent {
		return []factoryBridgeEvent{{Type: "result", Subtype: "interrupted", Text: "partial"}}
	}}
	executor := newFactoryResponseTestExecutor(bridge)
	body := []byte(`{"model":"factory/model-a","messages":[{"role":"user","content":"Hi"}]}`)
	_, err := executor.Execute(context.Background(), nil, cliproxyexecutor.Request{Model: "model-a", Payload: body}, cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatOpenAI})
	var status interface{ StatusCode() int }
	if !errors.As(err, &status) || status.StatusCode() != 502 || !strings.Contains(err.Error(), "interrupted") {
		t.Fatalf("Execute() error = %v", err)
	}
}

func TestFactoryExecuteMapsInvalidAttachmentToRequestError(t *testing.T) {
	bridge := &recordingFactoryBridge{run: func(factoryRunRequest) []factoryBridgeEvent {
		return []factoryBridgeEvent{{Type: "error", Code: "invalid_attachment", Message: "InvalidAttachmentError: bad image"}}
	}}
	executor := newFactoryResponseTestExecutor(bridge)
	body := []byte(`{"model":"factory/model-a","messages":[{"role":"user","content":"Hi"}]}`)
	_, err := executor.Execute(context.Background(), nil, cliproxyexecutor.Request{Model: "model-a", Payload: body}, cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatOpenAI})
	var requestErr *factoryRequestError
	if !errors.As(err, &requestErr) || requestErr.Param != "input" || requestErr.Code != "invalid_value" {
		t.Fatalf("Execute() error = %v", err)
	}
}

func TestFactoryExecuteRejectsDownstreamWebsocket(t *testing.T) {
	executor := newFactoryResponseTestExecutor(&recordingFactoryBridge{run: func(factoryRunRequest) []factoryBridgeEvent { return nil }})
	body := []byte(`{"model":"factory/model-a","input":"Hi","stream":true}`)
	ctx := cliproxyexecutor.WithDownstreamWebsocket(context.Background())
	_, err := executor.ExecuteStream(ctx, nil, cliproxyexecutor.Request{Model: "model-a", Payload: body}, cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatOpenAIResponse})
	var requestErr *factoryRequestError
	if !errors.As(err, &requestErr) || requestErr.Param != "websocket" {
		t.Fatalf("ExecuteStream() error = %v", err)
	}
}

func TestFactoryExecuteRejectsMismatchedTerminalStreamText(t *testing.T) {
	bridge := &recordingFactoryBridge{run: func(factoryRunRequest) []factoryBridgeEvent {
		return []factoryBridgeEvent{
			{Type: "text_delta", Text: "first"},
			{Type: "result", Success: true, Subtype: "success", Text: "different"},
		}
	}}
	executor := newFactoryResponseTestExecutor(bridge)
	body := []byte(`{"model":"factory/model-a","messages":[{"role":"user","content":"Hi"}],"stream":true}`)
	stream, err := executor.ExecuteStream(context.Background(), nil, cliproxyexecutor.Request{Model: "model-a", Payload: body}, cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatOpenAI})
	if err != nil {
		t.Fatalf("ExecuteStream() error = %v", err)
	}
	var terminal error
	for chunk := range stream.Chunks {
		if chunk.Err != nil {
			terminal = chunk.Err
		}
	}
	if terminal == nil || !strings.Contains(terminal.Error(), "did not match") {
		t.Fatalf("terminal stream error = %v", terminal)
	}
}

func TestFactoryExecuteConcurrentRequestsStayIndependent(t *testing.T) {
	bridge := &recordingFactoryBridge{run: func(request factoryRunRequest) []factoryBridgeEvent {
		return []factoryBridgeEvent{{Type: "result", Success: true, Subtype: "success", Text: "reply:" + request.Prompt}}
	}}
	executor := newFactoryResponseTestExecutor(bridge)
	prompts := []string{"alpha", "beta"}
	var wg sync.WaitGroup
	errs := make(chan error, len(prompts))
	for _, prompt := range prompts {
		prompt := prompt
		wg.Add(1)
		go func() {
			defer wg.Done()
			body := []byte(`{"model":"factory/model-a","messages":[{"role":"user","content":"` + prompt + `"}]}`)
			response, err := executor.Execute(context.Background(), nil, cliproxyexecutor.Request{Model: "model-a", Payload: body}, cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatOpenAI})
			if err != nil {
				errs <- err
				return
			}
			if got := gjson.GetBytes(response.Payload, "choices.0.message.content").String(); got != "reply:"+prompt {
				errs <- errors.New("crossed response for " + prompt + ": " + got)
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
}

func collectFactoryStream(t *testing.T, stream *cliproxyexecutor.StreamResult) [][]byte {
	t.Helper()
	var chunks [][]byte
	for chunk := range stream.Chunks {
		if chunk.Err != nil {
			t.Fatalf("stream error = %v", chunk.Err)
		}
		chunks = append(chunks, append([]byte(nil), chunk.Payload...))
	}
	return chunks
}

func factorySSEEventTypes(chunks [][]byte) []string {
	var events []string
	for _, chunk := range chunks {
		for _, frame := range strings.Split(string(chunk), "\n\n") {
			for _, line := range strings.Split(frame, "\n") {
				if strings.HasPrefix(line, "event: ") {
					events = append(events, strings.TrimSpace(strings.TrimPrefix(line, "event: ")))
					break
				}
			}
		}
	}
	return events
}
