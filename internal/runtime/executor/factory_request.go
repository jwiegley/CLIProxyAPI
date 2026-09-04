package executor

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/thinking"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
)

type factoryRunRequest struct {
	Operation           string              `json:"operation"`
	Model               string              `json:"model"`
	Prompt              string              `json:"prompt"`
	SystemPrompt        string              `json:"system_prompt,omitempty"`
	ReasoningEffort     string              `json:"reasoning_effort,omitempty"`
	OutputSchema        map[string]any      `json:"output_schema,omitempty"`
	Images              []factoryInputImage `json:"images,omitempty"`
	Files               []factoryInputFile  `json:"files,omitempty"`
	IncludeUsage        bool                `json:"include_usage,omitempty"`
	Autonomy            string              `json:"autonomy"`
	Tools               []string            `json:"tools"`
	EnableBuiltinSkills bool                `json:"enable_builtin_skills,omitempty"`
	DroidCommand        string              `json:"droid_command,omitempty"`
	CWD                 string              `json:"cwd,omitempty"`
}

type factoryInputImage struct {
	MediaType string `json:"media_type"`
	Data      string `json:"data"`
}

type factoryInputFile struct {
	Kind     string `json:"kind"`
	Data     string `json:"data"`
	Name     string `json:"name,omitempty"`
	MIMEType string `json:"mime_type,omitempty"`
}

type factoryRequestError struct {
	Code    string
	Message string
	Param   string
}

func (e *factoryRequestError) Error() string {
	payload := struct {
		Error struct {
			Message string `json:"message"`
			Type    string `json:"type"`
			Param   string `json:"param,omitempty"`
			Code    string `json:"code"`
		} `json:"error"`
	}{}
	payload.Error.Message = e.Message
	payload.Error.Type = "invalid_request_error"
	payload.Error.Param = e.Param
	payload.Error.Code = e.Code
	body, _ := json.Marshal(payload)
	return string(body)
}

func (*factoryRequestError) StatusCode() int       { return http.StatusBadRequest }
func (*factoryRequestError) IsRequestScoped() bool { return true }

func factoryInvalid(param, message string) error {
	return &factoryRequestError{Code: "invalid_value", Message: message, Param: param}
}

func factoryUnsupported(param string) error {
	return &factoryRequestError{
		Code:    "unsupported_parameter",
		Message: fmt.Sprintf("Factory Droid backend does not support parameter %q", param),
		Param:   param,
	}
}

func (e *FactoryExecutor) prepareRunRequest(req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (factoryRunRequest, error) {
	model := strings.TrimSpace(thinking.ParseSuffix(req.Model).ModelName)
	if model == "" {
		return factoryRunRequest{}, factoryInvalid("model", "model is required")
	}

	var (
		run factoryRunRequest
		err error
	)
	path, _ := opts.Metadata[cliproxyexecutor.RequestPathMetadataKey].(string)
	if opts.Alt == "responses/compact" || path == "/v1/responses/compact" {
		return factoryRunRequest{}, factoryUnsupported("endpoint")
	}
	switch {
	case path == "/v1/completions":
		body, _ := opts.Metadata[cliproxyexecutor.OriginalEndpointRequestMetadataKey].([]byte)
		if len(body) == 0 {
			return factoryRunRequest{}, factoryInvalid("prompt", "original completions request is unavailable")
		}
		run, err = parseFactoryCompletionRequest(body)
	case opts.SourceFormat == sdktranslator.FormatOpenAI:
		run, err = parseFactoryChatRequest(req.Payload)
	case opts.SourceFormat == sdktranslator.FormatOpenAIResponse:
		run, err = parseFactoryResponsesRequest(req.Payload)
	default:
		return factoryRunRequest{}, factoryUnsupported("protocol")
	}
	if err != nil {
		return factoryRunRequest{}, err
	}

	run.Operation = "run"
	run.Model = model
	run.DroidCommand = e.cfg.DroidCommand
	run.CWD = e.cfg.CWD
	run.Autonomy = e.cfg.Autonomy
	run.Tools = append([]string(nil), e.cfg.Tools...)
	run.EnableBuiltinSkills = e.cfg.EnableBuiltinSkills
	if effort, ok := opts.Metadata[cliproxyexecutor.ReasoningEffortMetadataKey].(string); ok && strings.TrimSpace(effort) != "" {
		run.ReasoningEffort = effort
	}
	run.ReasoningEffort = strings.ToLower(strings.TrimSpace(run.ReasoningEffort))
	if err := e.validateReasoningEffort(model, run.ReasoningEffort); err != nil {
		return factoryRunRequest{}, err
	}
	return run, nil
}

func (e *FactoryExecutor) validateReasoningEffort(model, effort string) error {
	e.modelMu.RLock()
	modelInfo, found := e.models[model]
	e.modelMu.RUnlock()
	if !found {
		return factoryInvalid("model", fmt.Sprintf("Factory model %q is not in the discovered catalog", model))
	}
	if effort == "" {
		return nil
	}
	for _, supported := range modelInfo.ReasoningEfforts {
		if strings.EqualFold(strings.TrimSpace(supported), effort) {
			return nil
		}
	}
	return factoryInvalid("reasoning_effort", fmt.Sprintf("reasoning effort %q is not supported by Factory model %q", effort, model))
}

func parseFactoryChatRequest(body []byte) (factoryRunRequest, error) {
	object, err := factoryObject(body, "request")
	if err != nil {
		return factoryRunRequest{}, err
	}
	if err := rejectFactoryUnknown(object, "", "model", "messages", "stream", "stream_options", "response_format", "reasoning_effort"); err != nil {
		return factoryRunRequest{}, err
	}
	if _, err = factoryString(object["model"], "model"); err != nil {
		return factoryRunRequest{}, err
	}
	if err := validateFactoryBool(object, "stream"); err != nil {
		return factoryRunRequest{}, err
	}

	run, err := parseFactoryChatMessages(object["messages"])
	if err != nil {
		return factoryRunRequest{}, err
	}
	if raw := object["reasoning_effort"]; !factoryNull(raw) {
		run.ReasoningEffort, err = factoryString(raw, "reasoning_effort")
		if err != nil {
			return factoryRunRequest{}, err
		}
	}
	if raw := object["response_format"]; !factoryNull(raw) {
		run.OutputSchema, err = parseFactoryChatOutput(raw)
		if err != nil {
			return factoryRunRequest{}, err
		}
	}
	if raw := object["stream_options"]; !factoryNull(raw) {
		run.IncludeUsage, err = parseFactoryStreamOptions(raw)
		if err != nil {
			return factoryRunRequest{}, err
		}
	}
	return run, nil
}

func parseFactoryCompletionRequest(body []byte) (factoryRunRequest, error) {
	object, err := factoryObject(body, "request")
	if err != nil {
		return factoryRunRequest{}, err
	}
	if err := rejectFactoryUnknown(object, "", "model", "prompt", "stream"); err != nil {
		return factoryRunRequest{}, err
	}
	if _, err = factoryString(object["model"], "model"); err != nil {
		return factoryRunRequest{}, err
	}
	if err := validateFactoryBool(object, "stream"); err != nil {
		return factoryRunRequest{}, err
	}
	prompt, err := factoryString(object["prompt"], "prompt")
	if err != nil {
		return factoryRunRequest{}, err
	}
	if prompt == "" {
		return factoryRunRequest{}, factoryInvalid("prompt", "prompt must not be empty")
	}
	return factoryRunRequest{Prompt: prompt}, nil
}

func parseFactoryResponsesRequest(body []byte) (factoryRunRequest, error) {
	object, err := factoryObject(body, "request")
	if err != nil {
		return factoryRunRequest{}, err
	}
	if err := rejectFactoryUnknown(object, "", "model", "input", "instructions", "stream", "text", "reasoning"); err != nil {
		return factoryRunRequest{}, err
	}
	if _, err = factoryString(object["model"], "model"); err != nil {
		return factoryRunRequest{}, err
	}
	if err := validateFactoryBool(object, "stream"); err != nil {
		return factoryRunRequest{}, err
	}

	run, err := parseFactoryResponsesInput(object["input"])
	if err != nil {
		return factoryRunRequest{}, err
	}
	if raw := object["instructions"]; !factoryNull(raw) {
		instructions, stringErr := factoryString(raw, "instructions")
		if stringErr != nil {
			return factoryRunRequest{}, stringErr
		}
		run.SystemPrompt = joinFactoryText(instructions, run.SystemPrompt)
	}
	if raw := object["text"]; !factoryNull(raw) {
		textObject, objectErr := factoryObject(raw, "text")
		if objectErr != nil {
			return factoryRunRequest{}, objectErr
		}
		if objectErr = rejectFactoryUnknown(textObject, "text", "format"); objectErr != nil {
			return factoryRunRequest{}, objectErr
		}
		if format := textObject["format"]; !factoryNull(format) {
			run.OutputSchema, objectErr = parseFactoryResponsesOutput(format)
			if objectErr != nil {
				return factoryRunRequest{}, objectErr
			}
		}
	}
	if raw := object["reasoning"]; !factoryNull(raw) {
		reasoning, objectErr := factoryObject(raw, "reasoning")
		if objectErr != nil {
			return factoryRunRequest{}, objectErr
		}
		if objectErr = rejectFactoryUnknown(reasoning, "reasoning", "effort"); objectErr != nil {
			return factoryRunRequest{}, objectErr
		}
		if effort := reasoning["effort"]; !factoryNull(effort) {
			run.ReasoningEffort, objectErr = factoryString(effort, "reasoning.effort")
			if objectErr != nil {
				return factoryRunRequest{}, objectErr
			}
		}
	}
	return run, nil
}

func parseFactoryChatMessages(raw json.RawMessage) (factoryRunRequest, error) {
	var messages []json.RawMessage
	if factoryNull(raw) || json.Unmarshal(raw, &messages) != nil || len(messages) == 0 {
		return factoryRunRequest{}, factoryInvalid("messages", "messages must be a non-empty array")
	}
	var run factoryRunRequest
	var instructions []string
	seenUser := false
	for index, messageRaw := range messages {
		param := fmt.Sprintf("messages.%d", index)
		message, err := factoryObject(messageRaw, param)
		if err != nil {
			return factoryRunRequest{}, err
		}
		if err = rejectFactoryUnknown(message, param, "role", "content"); err != nil {
			return factoryRunRequest{}, err
		}
		role, err := factoryString(message["role"], param+".role")
		if err != nil {
			return factoryRunRequest{}, err
		}
		switch role {
		case "system", "developer":
			if seenUser {
				return factoryRunRequest{}, factoryInvalid(param+".role", "system and developer instructions must precede user content")
			}
			text, textErr := parseFactoryTextContent(message["content"], param+".content", "text")
			if textErr != nil {
				return factoryRunRequest{}, textErr
			}
			if text != "" {
				instructions = append(instructions, text)
			}
		case "user":
			if seenUser || index != len(messages)-1 {
				return factoryRunRequest{}, factoryInvalid(param+".role", "Factory accepts exactly one final user message")
			}
			seenUser = true
			run, err = parseFactoryUserContent(message["content"], param+".content", factoryChatContent)
			if err != nil {
				return factoryRunRequest{}, err
			}
		default:
			return factoryRunRequest{}, factoryUnsupported(param + ".role")
		}
	}
	if !seenUser {
		return factoryRunRequest{}, factoryInvalid("messages", "messages must contain one user message")
	}
	run.SystemPrompt = strings.Join(instructions, "\n\n")
	return run, nil
}

type factoryContentFormat int

const (
	factoryChatContent factoryContentFormat = iota
	factoryResponsesContent
)

func parseFactoryUserContent(raw json.RawMessage, param string, format factoryContentFormat) (factoryRunRequest, error) {
	if factoryNull(raw) {
		return factoryRunRequest{}, factoryInvalid(param, "user content is required")
	}
	var text string
	if json.Unmarshal(raw, &text) == nil {
		if text == "" {
			return factoryRunRequest{}, factoryInvalid(param, "user content must not be empty")
		}
		return factoryRunRequest{Prompt: text}, nil
	}

	var parts []json.RawMessage
	if json.Unmarshal(raw, &parts) != nil || len(parts) == 0 {
		return factoryRunRequest{}, factoryInvalid(param, "content must be a string or non-empty array")
	}
	var run factoryRunRequest
	var texts []string
	for index, partRaw := range parts {
		partParam := fmt.Sprintf("%s.%d", param, index)
		part, err := factoryObject(partRaw, partParam)
		if err != nil {
			return factoryRunRequest{}, err
		}
		partType, err := factoryString(part["type"], partParam+".type")
		if err != nil {
			return factoryRunRequest{}, err
		}
		switch {
		case format == factoryChatContent && partType == "text", format == factoryResponsesContent && partType == "input_text":
			if err = rejectFactoryUnknown(part, partParam, "type", "text"); err != nil {
				return factoryRunRequest{}, err
			}
			value, stringErr := factoryString(part["text"], partParam+".text")
			if stringErr != nil {
				return factoryRunRequest{}, stringErr
			}
			if value != "" {
				texts = append(texts, value)
			}
		case format == factoryChatContent && partType == "image_url":
			image, imageErr := parseFactoryChatImage(part, partParam)
			if imageErr != nil {
				return factoryRunRequest{}, imageErr
			}
			run.Images = append(run.Images, image)
		case format == factoryResponsesContent && partType == "input_image":
			image, imageErr := parseFactoryResponsesImage(part, partParam)
			if imageErr != nil {
				return factoryRunRequest{}, imageErr
			}
			run.Images = append(run.Images, image)
		case format == factoryChatContent && partType == "file":
			file, fileErr := parseFactoryChatFile(part, partParam)
			if fileErr != nil {
				return factoryRunRequest{}, fileErr
			}
			run.Files = append(run.Files, file)
		case format == factoryResponsesContent && partType == "input_file":
			file, fileErr := parseFactoryResponsesFile(part, partParam)
			if fileErr != nil {
				return factoryRunRequest{}, fileErr
			}
			run.Files = append(run.Files, file)
		default:
			return factoryRunRequest{}, factoryUnsupported(partParam + ".type")
		}
	}
	run.Prompt = strings.Join(texts, "\n")
	if run.Prompt == "" && len(run.Images) == 0 && len(run.Files) == 0 {
		return factoryRunRequest{}, factoryInvalid(param, "user content must include text or an attachment")
	}
	return run, nil
}

func parseFactoryTextContent(raw json.RawMessage, param, partType string) (string, error) {
	if factoryNull(raw) {
		return "", factoryInvalid(param, "text content is required")
	}
	var text string
	if json.Unmarshal(raw, &text) == nil {
		return text, nil
	}
	var parts []json.RawMessage
	if json.Unmarshal(raw, &parts) != nil {
		return "", factoryInvalid(param, "instruction content must be text")
	}
	texts := make([]string, 0, len(parts))
	for index, rawPart := range parts {
		partParam := fmt.Sprintf("%s.%d", param, index)
		part, err := factoryObject(rawPart, partParam)
		if err != nil {
			return "", err
		}
		if err = rejectFactoryUnknown(part, partParam, "type", "text"); err != nil {
			return "", err
		}
		kind, err := factoryString(part["type"], partParam+".type")
		if err != nil {
			return "", err
		}
		if kind != partType {
			return "", factoryUnsupported(partParam + ".type")
		}
		value, err := factoryString(part["text"], partParam+".text")
		if err != nil {
			return "", err
		}
		if value != "" {
			texts = append(texts, value)
		}
	}
	return strings.Join(texts, "\n"), nil
}

func parseFactoryResponsesInput(raw json.RawMessage) (factoryRunRequest, error) {
	if factoryNull(raw) {
		return factoryRunRequest{}, factoryInvalid("input", "input is required")
	}
	var text string
	if json.Unmarshal(raw, &text) == nil {
		if text == "" {
			return factoryRunRequest{}, factoryInvalid("input", "input must not be empty")
		}
		return factoryRunRequest{Prompt: text}, nil
	}
	var items []json.RawMessage
	if json.Unmarshal(raw, &items) != nil || len(items) == 0 {
		return factoryRunRequest{}, factoryInvalid("input", "input must be a string or non-empty array")
	}

	if first, objectErr := factoryObject(items[0], "input.0"); objectErr == nil {
		if firstType, _ := factoryOptionalString(first["type"], "input.0.type"); firstType != "message" {
			return parseFactoryUserContent(raw, "input", factoryResponsesContent)
		}
	}

	var run factoryRunRequest
	var instructions []string
	seenUser := false
	for index, itemRaw := range items {
		param := fmt.Sprintf("input.%d", index)
		item, err := factoryObject(itemRaw, param)
		if err != nil {
			return factoryRunRequest{}, err
		}
		if err = rejectFactoryUnknown(item, param, "type", "role", "content"); err != nil {
			return factoryRunRequest{}, err
		}
		itemType, err := factoryString(item["type"], param+".type")
		if err != nil {
			return factoryRunRequest{}, err
		}
		if itemType != "message" {
			return factoryRunRequest{}, factoryUnsupported(param + ".type")
		}
		role, err := factoryString(item["role"], param+".role")
		if err != nil {
			return factoryRunRequest{}, err
		}
		switch role {
		case "system", "developer":
			if seenUser {
				return factoryRunRequest{}, factoryInvalid(param+".role", "system and developer instructions must precede user content")
			}
			value, textErr := parseFactoryTextContent(item["content"], param+".content", "input_text")
			if textErr != nil {
				return factoryRunRequest{}, textErr
			}
			if value != "" {
				instructions = append(instructions, value)
			}
		case "user":
			if seenUser || index != len(items)-1 {
				return factoryRunRequest{}, factoryInvalid(param+".role", "Factory accepts exactly one final user message")
			}
			seenUser = true
			run, err = parseFactoryUserContent(item["content"], param+".content", factoryResponsesContent)
			if err != nil {
				return factoryRunRequest{}, err
			}
		default:
			return factoryRunRequest{}, factoryUnsupported(param + ".role")
		}
	}
	if !seenUser {
		return factoryRunRequest{}, factoryInvalid("input", "input must contain one user message")
	}
	run.SystemPrompt = strings.Join(instructions, "\n\n")
	return run, nil
}

func parseFactoryChatImage(part map[string]json.RawMessage, param string) (factoryInputImage, error) {
	if err := rejectFactoryUnknown(part, param, "type", "image_url"); err != nil {
		return factoryInputImage{}, err
	}
	imageURL, err := factoryObject(part["image_url"], param+".image_url")
	if err != nil {
		return factoryInputImage{}, err
	}
	if err = rejectFactoryUnknown(imageURL, param+".image_url", "url", "detail"); err != nil {
		return factoryInputImage{}, err
	}
	if detail, detailErr := factoryOptionalString(imageURL["detail"], param+".image_url.detail"); detailErr != nil {
		return factoryInputImage{}, detailErr
	} else if detail != "" && detail != "auto" {
		return factoryInputImage{}, factoryUnsupported(param + ".image_url.detail")
	}
	url, err := factoryString(imageURL["url"], param+".image_url.url")
	if err != nil {
		return factoryInputImage{}, err
	}
	return decodeFactoryImageURL(url, param+".image_url.url")
}

func parseFactoryResponsesImage(part map[string]json.RawMessage, param string) (factoryInputImage, error) {
	if err := rejectFactoryUnknown(part, param, "type", "image_url", "detail"); err != nil {
		return factoryInputImage{}, err
	}
	if detail, err := factoryOptionalString(part["detail"], param+".detail"); err != nil {
		return factoryInputImage{}, err
	} else if detail != "" && detail != "auto" {
		return factoryInputImage{}, factoryUnsupported(param + ".detail")
	}
	url, err := factoryString(part["image_url"], param+".image_url")
	if err != nil {
		return factoryInputImage{}, err
	}
	return decodeFactoryImageURL(url, param+".image_url")
}

func decodeFactoryImageURL(url, param string) (factoryInputImage, error) {
	mediaType, data, err := decodeFactoryDataURL(url, param)
	if err != nil {
		return factoryInputImage{}, err
	}
	switch mediaType {
	case "image/png", "image/jpeg", "image/gif", "image/webp":
		return factoryInputImage{MediaType: mediaType, Data: data}, nil
	default:
		return factoryInputImage{}, factoryInvalid(param, fmt.Sprintf("unsupported image media type %q", mediaType))
	}
}

func parseFactoryChatFile(part map[string]json.RawMessage, param string) (factoryInputFile, error) {
	if err := rejectFactoryUnknown(part, param, "type", "file"); err != nil {
		return factoryInputFile{}, err
	}
	fileObject, err := factoryObject(part["file"], param+".file")
	if err != nil {
		return factoryInputFile{}, err
	}
	if err = rejectFactoryUnknown(fileObject, param+".file", "file_data", "filename"); err != nil {
		return factoryInputFile{}, err
	}
	return parseFactoryInlineFile(fileObject["file_data"], fileObject["filename"], param+".file")
}

func parseFactoryResponsesFile(part map[string]json.RawMessage, param string) (factoryInputFile, error) {
	if err := rejectFactoryUnknown(part, param, "type", "file_data", "filename"); err != nil {
		return factoryInputFile{}, err
	}
	return parseFactoryInlineFile(part["file_data"], part["filename"], param)
}

func parseFactoryInlineFile(dataRaw, nameRaw json.RawMessage, param string) (factoryInputFile, error) {
	dataURL, err := factoryString(dataRaw, param+".file_data")
	if err != nil {
		return factoryInputFile{}, err
	}
	name, err := factoryOptionalString(nameRaw, param+".filename")
	if err != nil {
		return factoryInputFile{}, err
	}
	mediaType, encoded, err := decodeFactoryDataURL(dataURL, param+".file_data")
	if err != nil {
		return factoryInputFile{}, err
	}
	decoded, _ := base64.StdEncoding.DecodeString(encoded)
	if mediaType == "application/pdf" {
		return factoryInputFile{Kind: "pdf", Data: encoded, Name: name, MIMEType: mediaType}, nil
	}
	if !strings.HasPrefix(mediaType, "text/") {
		return factoryInputFile{}, factoryInvalid(param+".file_data", fmt.Sprintf("unsupported document media type %q", mediaType))
	}
	if !utf8.Valid(decoded) {
		return factoryInputFile{}, factoryInvalid(param+".file_data", "text document is not valid UTF-8")
	}
	return factoryInputFile{Kind: "text", Data: string(decoded), Name: name, MIMEType: mediaType}, nil
}

func decodeFactoryDataURL(value, param string) (string, string, error) {
	if !strings.HasPrefix(value, "data:") {
		return "", "", factoryUnsupported(param)
	}
	header, encoded, found := strings.Cut(strings.TrimPrefix(value, "data:"), ",")
	if !found {
		return "", "", factoryInvalid(param, "invalid data URL")
	}
	mediaType, marker, found := strings.Cut(header, ";")
	if !found || marker != "base64" || strings.TrimSpace(mediaType) == "" {
		return "", "", factoryInvalid(param, "data URL must use an explicit media type and base64 encoding")
	}
	decoded, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return "", "", factoryInvalid(param, "data URL contains invalid base64")
	}
	return strings.ToLower(strings.TrimSpace(mediaType)), base64.StdEncoding.EncodeToString(decoded), nil
}

func parseFactoryChatOutput(raw json.RawMessage) (map[string]any, error) {
	object, err := factoryObject(raw, "response_format")
	if err != nil {
		return nil, err
	}
	if err = rejectFactoryUnknown(object, "response_format", "type", "json_schema"); err != nil {
		return nil, err
	}
	outputType, err := factoryString(object["type"], "response_format.type")
	if err != nil {
		return nil, err
	}
	switch outputType {
	case "text":
		if !factoryNull(object["json_schema"]) {
			return nil, factoryUnsupported("response_format.json_schema")
		}
		return nil, nil
	case "json_object":
		return map[string]any{"type": "object"}, nil
	case "json_schema":
		return parseFactorySchemaEnvelope(object["json_schema"], "response_format.json_schema")
	default:
		return nil, factoryInvalid("response_format.type", fmt.Sprintf("unsupported response format %q", outputType))
	}
}

func parseFactoryResponsesOutput(raw json.RawMessage) (map[string]any, error) {
	object, err := factoryObject(raw, "text.format")
	if err != nil {
		return nil, err
	}
	outputType, err := factoryString(object["type"], "text.format.type")
	if err != nil {
		return nil, err
	}
	switch outputType {
	case "text":
		if err = rejectFactoryUnknown(object, "text.format", "type"); err != nil {
			return nil, err
		}
		return nil, nil
	case "json_object":
		if err = rejectFactoryUnknown(object, "text.format", "type"); err != nil {
			return nil, err
		}
		return map[string]any{"type": "object"}, nil
	case "json_schema":
		return parseFactorySchemaObject(object, "text.format")
	default:
		return nil, factoryInvalid("text.format.type", fmt.Sprintf("unsupported text format %q", outputType))
	}
}

func parseFactorySchemaEnvelope(raw json.RawMessage, param string) (map[string]any, error) {
	object, err := factoryObject(raw, param)
	if err != nil {
		return nil, err
	}
	return parseFactorySchemaObject(object, param)
}

func parseFactorySchemaObject(object map[string]json.RawMessage, param string) (map[string]any, error) {
	if err := rejectFactoryUnknown(object, param, "type", "name", "description", "schema", "strict"); err != nil {
		return nil, err
	}
	if description := object["description"]; !factoryNull(description) {
		return nil, factoryUnsupported(param + ".description")
	}
	if strict := object["strict"]; !factoryNull(strict) {
		var value bool
		if json.Unmarshal(strict, &value) != nil {
			return nil, factoryInvalid(param+".strict", "strict must be a boolean")
		}
		if !value {
			return nil, factoryUnsupported(param + ".strict")
		}
	}
	schemaName := ""
	if name := object["name"]; !factoryNull(name) {
		var err error
		schemaName, err = factoryString(name, param+".name")
		if err != nil {
			return nil, err
		}
		schemaName = strings.TrimSpace(schemaName)
		if schemaName == "" {
			return nil, factoryInvalid(param+".name", "schema name must not be empty")
		}
	}
	schemaRaw := object["schema"]
	if factoryNull(schemaRaw) {
		return nil, factoryInvalid(param+".schema", "JSON schema is required")
	}
	var schema map[string]any
	if json.Unmarshal(schemaRaw, &schema) != nil || schema == nil {
		return nil, factoryInvalid(param+".schema", "schema must be a JSON object")
	}
	if schemaName != "" {
		if title, exists := schema["title"]; exists {
			titleString, ok := title.(string)
			if !ok || titleString != schemaName {
				return nil, factoryInvalid(param+".name", "schema name must match schema title when both are provided")
			}
		} else {
			schema["title"] = schemaName
		}
	}
	return schema, nil
}

func parseFactoryStreamOptions(raw json.RawMessage) (bool, error) {
	object, err := factoryObject(raw, "stream_options")
	if err != nil {
		return false, err
	}
	if err = rejectFactoryUnknown(object, "stream_options", "include_usage"); err != nil {
		return false, err
	}
	if factoryNull(object["include_usage"]) {
		return false, nil
	}
	var include bool
	if json.Unmarshal(object["include_usage"], &include) != nil {
		return false, factoryInvalid("stream_options.include_usage", "include_usage must be a boolean")
	}
	return include, nil
}

func factoryObject(raw []byte, param string) (map[string]json.RawMessage, error) {
	if factoryNull(raw) {
		return nil, factoryInvalid(param, "value must be a JSON object")
	}
	var object map[string]json.RawMessage
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if err := decoder.Decode(&object); err != nil || object == nil {
		return nil, factoryInvalid(param, "value must be a JSON object")
	}
	return object, nil
}

func rejectFactoryUnknown(object map[string]json.RawMessage, prefix string, allowed ...string) error {
	allowedSet := make(map[string]struct{}, len(allowed))
	for _, key := range allowed {
		allowedSet[key] = struct{}{}
	}
	unknown := make([]string, 0)
	for key, raw := range object {
		if _, ok := allowedSet[key]; ok || factoryNull(raw) {
			continue
		}
		unknown = append(unknown, key)
	}
	if len(unknown) == 0 {
		return nil
	}
	sort.Strings(unknown)
	param := unknown[0]
	if prefix != "" {
		param = prefix + "." + param
	}
	return factoryUnsupported(param)
}

func factoryString(raw json.RawMessage, param string) (string, error) {
	if factoryNull(raw) {
		return "", factoryInvalid(param, "value is required")
	}
	var value string
	if json.Unmarshal(raw, &value) != nil {
		return "", factoryInvalid(param, "value must be a string")
	}
	return value, nil
}

func factoryOptionalString(raw json.RawMessage, param string) (string, error) {
	if factoryNull(raw) {
		return "", nil
	}
	return factoryString(raw, param)
}

func validateFactoryBool(object map[string]json.RawMessage, field string) error {
	raw := object[field]
	if factoryNull(raw) {
		return nil
	}
	var value bool
	if json.Unmarshal(raw, &value) != nil {
		return factoryInvalid(field, fmt.Sprintf("%s must be a boolean", field))
	}
	return nil
}

func factoryNull(raw []byte) bool {
	return len(bytes.TrimSpace(raw)) == 0 || bytes.Equal(bytes.TrimSpace(raw), []byte("null"))
}

func joinFactoryText(parts ...string) string {
	joined := make([]string, 0, len(parts))
	for _, part := range parts {
		if part != "" {
			joined = append(joined, part)
		}
	}
	return strings.Join(joined, "\n\n")
}
