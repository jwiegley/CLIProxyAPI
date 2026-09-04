package executor

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"sync"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
)

const factoryProvider = "factory"

//go:embed factory_bridge.py
var factoryBridgeSource string

type factoryBridgeModel struct {
	ID                     string   `json:"id"`
	DisplayName            string   `json:"display_name"`
	Provider               string   `json:"provider"`
	ReasoningEfforts       []string `json:"reasoning_efforts"`
	DefaultReasoningEffort string   `json:"default_reasoning_effort"`
	NoImageSupport         *bool    `json:"no_image_support"`
}

type factoryBridge interface {
	ListModels(context.Context) ([]factoryBridgeModel, error)
	Run(context.Context, factoryRunRequest) (*factoryBridgeRun, error)
}

type subprocessFactoryBridge struct {
	config  config.FactoryConfig
	command func(context.Context, string, string) *exec.Cmd
}

type factoryBridgeRequest struct {
	Operation    string `json:"operation"`
	DroidCommand string `json:"droid_command,omitempty"`
	CWD          string `json:"cwd,omitempty"`
}

type factoryBridgeResponse struct {
	Type    string               `json:"type"`
	Code    string               `json:"code,omitempty"`
	Message string               `json:"message,omitempty"`
	Models  []factoryBridgeModel `json:"models,omitempty"`
}

func (b *subprocessFactoryBridge) ListModels(ctx context.Context) ([]factoryBridgeModel, error) {
	var response factoryBridgeResponse
	err := b.call(ctx, factoryBridgeRequest{
		Operation:    "models",
		DroidCommand: b.config.DroidCommand,
		CWD:          b.config.CWD,
	}, &response)
	if err != nil {
		return nil, err
	}
	if response.Type != "models" {
		return nil, fmt.Errorf("factory SDK bridge returned unexpected response type %q", response.Type)
	}
	return response.Models, nil
}

func (b *subprocessFactoryBridge) call(ctx context.Context, request factoryBridgeRequest, response *factoryBridgeResponse) error {
	if ctx == nil {
		ctx = context.Background()
	}
	pythonCommand := strings.TrimSpace(b.config.PythonCommand)
	if pythonCommand == "" {
		pythonCommand = "python3"
	}
	payload, errMarshal := json.Marshal(request)
	if errMarshal != nil {
		return fmt.Errorf("encode Factory SDK bridge request: %w", errMarshal)
	}
	command := b.command
	if command == nil {
		command = func(ctx context.Context, executable, source string) *exec.Cmd {
			return exec.CommandContext(ctx, executable, "-c", source)
		}
	}
	cmd := command(ctx, pythonCommand, factoryBridgeSource)
	cmd.Stdin = bytes.NewReader(append(payload, '\n'))
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if errRun := cmd.Run(); errRun != nil {
		if errContext := ctx.Err(); errContext != nil {
			return errContext
		}
		var execErr *exec.Error
		if errors.As(errRun, &execErr) || errors.Is(errRun, os.ErrNotExist) {
			return fmt.Errorf("Factory Python executable %q is unavailable: %w", pythonCommand, errRun)
		}
		if errDecode := json.Unmarshal(bytes.TrimSpace(stdout.Bytes()), response); errDecode == nil && response.Type == "error" {
			return fmt.Errorf("Factory SDK bridge %s: %s", response.Code, redactFactorySecret(response.Message))
		}
		detail := strings.TrimSpace(stderr.String())
		if detail == "" {
			detail = strings.TrimSpace(stdout.String())
		}
		if detail == "" {
			return fmt.Errorf("Factory SDK bridge failed: %w", errRun)
		}
		return fmt.Errorf("Factory SDK bridge failed: %w: %s", errRun, redactFactorySecret(detail))
	}
	if errDecode := json.Unmarshal(bytes.TrimSpace(stdout.Bytes()), response); errDecode != nil {
		return fmt.Errorf("decode Factory SDK bridge response: %w", errDecode)
	}
	if response.Type == "error" {
		return fmt.Errorf("Factory SDK bridge %s: %s", response.Code, redactFactorySecret(response.Message))
	}
	return nil
}

func redactFactorySecret(value string) string {
	secret := os.Getenv("FACTORY_API_KEY")
	if secret == "" {
		return value
	}
	return strings.ReplaceAll(value, secret, "[REDACTED]")
}

// FactoryExecutor executes Factory models through the official Droid SDK bridge.
type FactoryExecutor struct {
	cfg     config.FactoryConfig
	bridge  factoryBridge
	modelMu sync.RWMutex
	models  map[string]factoryBridgeModel
}

func NewFactoryExecutor(cfg *config.Config) *FactoryExecutor {
	factoryConfig := config.FactoryConfig{}
	if cfg != nil {
		normalized := &config.Config{Factory: cfg.Factory}
		normalized.SanitizeFactory()
		factoryConfig = normalized.Factory
	}
	return &FactoryExecutor{
		cfg:    factoryConfig,
		bridge: &subprocessFactoryBridge{config: factoryConfig},
		models: make(map[string]factoryBridgeModel),
	}
}

func (e *FactoryExecutor) Identifier() string { return factoryProvider }

func (e *FactoryExecutor) RequestToFormat(_ cliproxyexecutor.Request, opts cliproxyexecutor.Options) sdktranslator.Format {
	return opts.SourceFormat
}

func (e *FactoryExecutor) ListModels(ctx context.Context) ([]*registry.ModelInfo, error) {
	models, err := e.bridge.ListModels(ctx)
	if err != nil {
		e.modelMu.Lock()
		clear(e.models)
		e.modelMu.Unlock()
		return nil, err
	}
	seen := make(map[string]struct{}, len(models))
	catalog := make(map[string]factoryBridgeModel, len(models))
	out := make([]*registry.ModelInfo, 0, len(models))
	for _, model := range models {
		model.ID = strings.TrimSpace(model.ID)
		if model.ID == "" {
			continue
		}
		if _, exists := seen[model.ID]; exists {
			continue
		}
		seen[model.ID] = struct{}{}
		catalog[model.ID] = model
		inputModalities := []string{"text"}
		if model.NoImageSupport == nil || !*model.NoImageSupport {
			inputModalities = append(inputModalities, "image")
		}
		out = append(out, &registry.ModelInfo{
			ID:                        model.ID,
			Object:                    "model",
			OwnedBy:                   "factory",
			Type:                      factoryProvider,
			DisplayName:               strings.TrimSpace(model.DisplayName),
			SupportedParameters:       []string{"reasoning_effort", "response_format", "stream"},
			SupportedInputModalities:  inputModalities,
			SupportedOutputModalities: []string{"text"},
			Thinking:                  &registry.ThinkingSupport{Levels: append([]string(nil), model.ReasoningEfforts...)},
		})
	}
	e.modelMu.Lock()
	e.models = catalog
	e.modelMu.Unlock()
	return out, nil
}

func (e *FactoryExecutor) Execute(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	return e.execute(ctx, auth, req, opts)
}

func (e *FactoryExecutor) ExecuteStream(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (*cliproxyexecutor.StreamResult, error) {
	return e.executeStream(ctx, auth, req, opts)
}

func (e *FactoryExecutor) Refresh(_ context.Context, auth *cliproxyauth.Auth) (*cliproxyauth.Auth, error) {
	return auth, nil
}

func (e *FactoryExecutor) CountTokens(context.Context, *cliproxyauth.Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	return cliproxyexecutor.Response{}, statusErr{code: http.StatusNotImplemented, msg: "Factory token counting is not supported"}
}

func (e *FactoryExecutor) HttpRequest(context.Context, *cliproxyauth.Auth, *http.Request) (*http.Response, error) {
	return nil, statusErr{code: http.StatusNotImplemented, msg: "Factory raw HTTP requests are not supported"}
}
