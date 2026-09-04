package executor

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/runtime/executor/helps"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	coreusage "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/usage"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
)

func (e *FactoryExecutor) execute(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (response cliproxyexecutor.Response, err error) {
	if cliproxyexecutor.DownstreamWebsocket(ctx) {
		return response, factoryUnsupported("websocket")
	}
	run, err := e.prepareRunRequest(req, opts)
	if err != nil {
		return response, err
	}
	responseFormat := cliproxyexecutor.ResponseFormatOrSource(opts)
	if err = validateFactoryResponseFormat(responseFormat); err != nil {
		return response, err
	}
	reporter := helps.NewExecutorUsageReporter(ctx, e, run.Model, auth)
	reporter.SetStream(false)
	defer reporter.TrackFailure(ctx, &err)
	reporter.StartResponseTTFT()

	bridgeRun, err := e.bridge.Run(ctx, run)
	if err != nil {
		return response, factoryBridgeExecutionError("bridge_start", err.Error())
	}
	defer waitFactoryBridge(bridgeRun)
	var result factoryBridgeEvent
	for {
		select {
		case <-ctx.Done():
			return response, ctx.Err()
		case event, ok := <-bridgeRun.Events:
			if !ok {
				return response, statusErr{code: http.StatusBadGateway, msg: "Factory SDK bridge ended without a result"}
			}
			switch event.Type {
			case "text_delta":
				reporter.ObserveTokenEvent(true)
			case "error":
				return response, factoryBridgeExecutionError(event.Code, event.Message)
			case "result":
				result = event
				if !event.Success {
					return response, factoryResultError(event)
				}
				reporter.ObserveTokenEvent(event.Text != "" || event.StructuredOutput != nil)
				goto complete
			}
		}
	}

complete:
	chatBody := buildFactoryChatResponse(factoryResponseID("chatcmpl"), factoryRequestedModel(req, opts), time.Now().Unix(), result)
	if responseFormat == sdktranslator.FormatOpenAIResponse {
		var state any
		chatBody = sdktranslator.TranslateNonStream(
			ctx,
			sdktranslator.FormatOpenAI,
			sdktranslator.FormatOpenAIResponse,
			run.Model,
			opts.OriginalRequest,
			req.Payload,
			chatBody,
			&state,
		)
		chatBody = helps.EnsureResponsesUsageDetails(chatBody)
	}
	if result.Usage != nil {
		reporter.Publish(ctx, factoryUsageDetail(result.Usage))
	}
	reporter.EnsurePublished(ctx)
	return cliproxyexecutor.Response{
		Payload: chatBody,
		Headers: http.Header{"Content-Type": []string{"application/json"}},
	}, nil
}

func (e *FactoryExecutor) executeStream(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (*cliproxyexecutor.StreamResult, error) {
	if cliproxyexecutor.DownstreamWebsocket(ctx) {
		return nil, factoryUnsupported("websocket")
	}
	run, err := e.prepareRunRequest(req, opts)
	if err != nil {
		return nil, err
	}
	responseFormat := cliproxyexecutor.ResponseFormatOrSource(opts)
	if err = validateFactoryResponseFormat(responseFormat); err != nil {
		return nil, err
	}
	bridgeRun, err := e.bridge.Run(ctx, run)
	if err != nil {
		return nil, factoryBridgeExecutionError("bridge_start", err.Error())
	}

	out := make(chan cliproxyexecutor.StreamChunk)
	reporter := helps.NewExecutorUsageReporter(ctx, e, run.Model, auth)
	reporter.SetStream(true)
	reporter.StartResponseTTFT()
	go e.forwardFactoryStream(ctx, reporter, req, opts, run, responseFormat, bridgeRun, out)
	return &cliproxyexecutor.StreamResult{
		Headers: http.Header{"Content-Type": []string{"text/event-stream"}},
		Chunks:  out,
	}, nil
}

func (e *FactoryExecutor) forwardFactoryStream(
	ctx context.Context,
	reporter *helps.UsageReporter,
	req cliproxyexecutor.Request,
	opts cliproxyexecutor.Options,
	run factoryRunRequest,
	responseFormat sdktranslator.Format,
	bridgeRun *factoryBridgeRun,
	out chan<- cliproxyexecutor.StreamChunk,
) {
	defer close(out)
	defer waitFactoryBridge(bridgeRun)
	events := bridgeRun.Events
	id := factoryResponseID("chatcmpl")
	model := factoryRequestedModel(req, opts)
	created := time.Now().Unix()
	var translatorState any
	var streamed strings.Builder
	started := false
	terminal := false

	emitPayload := func(payload []byte) bool {
		outputs := [][]byte{payload}
		if responseFormat == sdktranslator.FormatOpenAIResponse {
			line := append([]byte("data: "), payload...)
			outputs = sdktranslator.TranslateStream(
				ctx,
				sdktranslator.FormatOpenAI,
				sdktranslator.FormatOpenAIResponse,
				run.Model,
				opts.OriginalRequest,
				req.Payload,
				line,
				&translatorState,
			)
		}
		for _, output := range outputs {
			if len(output) == 0 {
				continue
			}
			select {
			case out <- cliproxyexecutor.StreamChunk{Payload: output}:
			case <-ctx.Done():
				return false
			}
		}
		return true
	}
	emitError := func(err error) {
		reporter.PublishFailure(ctx, err)
		select {
		case out <- cliproxyexecutor.StreamChunk{Err: err}:
		case <-ctx.Done():
		}
	}
	emitStart := func() bool {
		if started {
			return true
		}
		started = true
		return emitPayload(buildFactoryChatStreamChunk(id, model, created, map[string]any{"role": "assistant", "content": ""}, nil, nil))
	}
	emitDelta := func(text string) bool {
		if text == "" {
			return true
		}
		if !emitStart() {
			return false
		}
		streamed.WriteString(text)
		reporter.ObserveTokenEvent(true)
		return emitPayload(buildFactoryChatStreamChunk(id, model, created, map[string]any{"content": text}, nil, nil))
	}

	for event := range events {
		if ctx.Err() != nil {
			return
		}
		switch event.Type {
		case "text_delta":
			if !emitDelta(event.Text) {
				return
			}
		case "error":
			emitError(factoryBridgeExecutionError(event.Code, event.Message))
			return
		case "result":
			terminal = true
			if !event.Success {
				emitError(factoryResultError(event))
				return
			}
			finalText := event.Text
			if finalText == "" && event.StructuredOutput != nil {
				finalText = marshalFactoryStructuredOutput(event.StructuredOutput)
			}
			current := streamed.String()
			switch {
			case current == finalText:
			case strings.HasPrefix(finalText, current):
				if !emitDelta(strings.TrimPrefix(finalText, current)) {
					return
				}
			default:
				emitError(statusErr{code: http.StatusBadGateway, msg: "Factory SDK terminal text did not match streamed text"})
				return
			}
			if !emitStart() {
				return
			}
			finish := "stop"
			if !emitPayload(buildFactoryChatStreamChunk(id, model, created, map[string]any{}, &finish, nil)) {
				return
			}
			if event.Usage != nil && (run.IncludeUsage || responseFormat == sdktranslator.FormatOpenAIResponse) {
				if !emitPayload(buildFactoryChatStreamChunk(id, model, created, nil, nil, event.Usage)) {
					return
				}
			}
			if responseFormat == sdktranslator.FormatOpenAIResponse {
				for _, output := range sdktranslator.TranslateStream(
					ctx,
					sdktranslator.FormatOpenAI,
					sdktranslator.FormatOpenAIResponse,
					run.Model,
					opts.OriginalRequest,
					req.Payload,
					[]byte("data: [DONE]"),
					&translatorState,
				) {
					select {
					case out <- cliproxyexecutor.StreamChunk{Payload: output}:
					case <-ctx.Done():
						return
					}
				}
			}
			if event.Usage != nil {
				reporter.Publish(ctx, factoryUsageDetail(event.Usage))
			}
			reporter.EnsurePublished(ctx)
			return
		}
	}
	if !terminal && ctx.Err() == nil {
		emitError(statusErr{code: http.StatusBadGateway, msg: "Factory SDK bridge ended without a terminal result"})
	}
}

func waitFactoryBridge(run *factoryBridgeRun) {
	if run != nil && run.Done != nil {
		<-run.Done
	}
}

func validateFactoryResponseFormat(format sdktranslator.Format) error {
	switch format {
	case sdktranslator.FormatOpenAI, sdktranslator.FormatOpenAIResponse:
		return nil
	default:
		return factoryUnsupported("response_protocol")
	}
}

func factoryBridgeExecutionError(code, message string) error {
	message = strings.TrimSpace(redactFactorySecret(message))
	if message == "" {
		message = "Factory SDK bridge failed"
	}
	if code == "invalid_attachment" {
		return factoryInvalid("input", message)
	}
	status := http.StatusBadGateway
	lower := strings.ToLower(message)
	switch {
	case code == "sdk_unavailable" || code == "bridge_start":
		status = http.StatusServiceUnavailable
	case strings.Contains(lower, "unauthorized"), strings.Contains(lower, "authentication"), strings.Contains(lower, "api key"):
		status = http.StatusUnauthorized
	}
	return statusErr{code: status, msg: message}
}

func factoryResultError(event factoryBridgeEvent) error {
	message := strings.TrimSpace(event.Message)
	if message == "" {
		message = "Factory Droid run ended with " + event.Subtype
	}
	return factoryBridgeExecutionError(event.Subtype, message)
}

func factoryRequestedModel(req cliproxyexecutor.Request, opts cliproxyexecutor.Options) string {
	if requested, ok := opts.Metadata[cliproxyexecutor.RequestedModelMetadataKey].(string); ok && strings.TrimSpace(requested) != "" {
		return strings.TrimSpace(requested)
	}
	return strings.TrimSpace(req.Model)
}

func factoryResponseID(prefix string) string {
	return prefix + "_" + strings.ReplaceAll(uuid.NewString(), "-", "")
}

func factoryResultText(event factoryBridgeEvent) string {
	if event.StructuredOutput != nil {
		return marshalFactoryStructuredOutput(event.StructuredOutput)
	}
	return event.Text
}

func marshalFactoryStructuredOutput(output map[string]any) string {
	body, err := json.Marshal(output)
	if err != nil {
		return ""
	}
	return string(body)
}

func buildFactoryChatResponse(id, model string, created int64, event factoryBridgeEvent) []byte {
	response := map[string]any{
		"id":      id,
		"object":  "chat.completion",
		"created": created,
		"model":   model,
		"choices": []any{map[string]any{
			"index": 0,
			"message": map[string]any{
				"role":    "assistant",
				"content": factoryResultText(event),
				"refusal": nil,
			},
			"logprobs":      nil,
			"finish_reason": "stop",
		}},
	}
	if event.Usage != nil {
		response["usage"] = factoryChatUsage(event.Usage)
	}
	body, _ := json.Marshal(response)
	return body
}

func buildFactoryChatStreamChunk(id, model string, created int64, delta map[string]any, finishReason *string, usage *factoryBridgeUsage) []byte {
	response := map[string]any{
		"id":      id,
		"object":  "chat.completion.chunk",
		"created": created,
		"model":   model,
	}
	if usage == nil {
		response["choices"] = []any{map[string]any{
			"index":         0,
			"delta":         delta,
			"logprobs":      nil,
			"finish_reason": finishReason,
		}}
	} else {
		response["choices"] = []any{}
		response["usage"] = factoryChatUsage(usage)
	}
	body, _ := json.Marshal(response)
	return body
}

func factoryChatUsage(usage *factoryBridgeUsage) map[string]any {
	return map[string]any{
		"prompt_tokens":     usage.InputTokens,
		"completion_tokens": usage.OutputTokens,
		"total_tokens":      usage.InputTokens + usage.OutputTokens,
		"prompt_tokens_details": map[string]any{
			"cached_tokens": usage.CacheReadTokens,
		},
		"completion_tokens_details": map[string]any{
			"reasoning_tokens": usage.ThinkingTokens,
		},
	}
}

func factoryUsageDetail(usage *factoryBridgeUsage) coreusage.Detail {
	return coreusage.Detail{
		InputTokens:         usage.InputTokens,
		OutputTokens:        usage.OutputTokens,
		ReasoningTokens:     usage.ThinkingTokens,
		CachedTokens:        usage.CacheReadTokens,
		CacheReadTokens:     usage.CacheReadTokens,
		CacheCreationTokens: usage.CacheCreationTokens,
		TotalTokens:         usage.InputTokens + usage.OutputTokens,
	}
}
