package executor

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
)

type staticFactoryBridge struct {
	models []factoryBridgeModel
	err    error
}

func (b staticFactoryBridge) ListModels(context.Context) ([]factoryBridgeModel, error) {
	return b.models, b.err
}

func (b staticFactoryBridge) Run(context.Context, factoryRunRequest) (*factoryBridgeRun, error) {
	events := make(chan factoryBridgeEvent)
	done := make(chan struct{})
	close(events)
	close(done)
	return &factoryBridgeRun{Events: events, Done: done}, b.err
}

func TestFactoryExecutorListModels(t *testing.T) {
	noImages := true
	executor := NewFactoryExecutor(&config.Config{Factory: config.FactoryConfig{Enabled: true}})
	if executor.cfg.PythonCommand != "python3" || executor.cfg.DroidCommand != "droid" || executor.cfg.Prefix != "factory" {
		t.Fatalf("normalized Factory config = %#v", executor.cfg)
	}
	executor.bridge = staticFactoryBridge{models: []factoryBridgeModel{
		{ID: " model-a ", DisplayName: "Model A", ReasoningEfforts: []string{"off", "high"}},
		{ID: "model-a", DisplayName: "duplicate"},
		{ID: "model-b", DisplayName: "Model B", ReasoningEfforts: []string{"none"}, NoImageSupport: &noImages},
		{ID: "   "},
	}}

	models, err := executor.ListModels(context.Background())
	if err != nil {
		t.Fatalf("ListModels() error = %v", err)
	}
	if len(models) != 2 {
		t.Fatalf("len(models) = %d, want 2", len(models))
	}
	if models[0].ID != "model-a" || models[0].OwnedBy != "factory" || models[0].Type != "factory" {
		t.Fatalf("models[0] = %#v", models[0])
	}
	if got := strings.Join(models[0].SupportedInputModalities, ","); got != "text,image" {
		t.Fatalf("model-a input modalities = %q", got)
	}
	if got := strings.Join(models[1].SupportedInputModalities, ","); got != "text" {
		t.Fatalf("model-b input modalities = %q", got)
	}
	if got := strings.Join(models[0].Thinking.Levels, ","); got != "off,high" {
		t.Fatalf("model-a reasoning levels = %q", got)
	}
	if len(executor.models) != 2 {
		t.Fatalf("cached model count = %d, want 2", len(executor.models))
	}
}

func TestSubprocessFactoryBridgeListModels(t *testing.T) {
	bridge := &subprocessFactoryBridge{
		config:  config.FactoryConfig{PythonCommand: "ignored", DroidCommand: "/bin/droid", CWD: "/workspace"},
		command: factoryBridgeHelperCommand(t, "models"),
	}
	models, err := bridge.ListModels(context.Background())
	if err != nil {
		t.Fatalf("ListModels() error = %v", err)
	}
	if len(models) != 1 || models[0].ID != "factory-model" || models[0].DefaultReasoningEffort != "high" {
		t.Fatalf("models = %#v", models)
	}
}

func TestSubprocessFactoryBridgeMissingPython(t *testing.T) {
	bridge := &subprocessFactoryBridge{config: config.FactoryConfig{PythonCommand: t.TempDir() + "/missing-python"}}
	_, err := bridge.ListModels(context.Background())
	if err == nil || !strings.Contains(err.Error(), "Python executable") || !strings.Contains(err.Error(), "unavailable") {
		t.Fatalf("ListModels() error = %v", err)
	}
}

func TestSubprocessFactoryBridgeRedactsFactoryAPIKey(t *testing.T) {
	const secret = "factory-secret-that-must-not-leak"
	t.Setenv("FACTORY_API_KEY", secret)
	bridge := &subprocessFactoryBridge{
		config:  config.FactoryConfig{PythonCommand: "ignored"},
		command: factoryBridgeHelperCommand(t, "leak"),
	}
	_, err := bridge.ListModels(context.Background())
	if err == nil {
		t.Fatal("ListModels() error = nil")
	}
	if strings.Contains(err.Error(), secret) || !strings.Contains(err.Error(), "[REDACTED]") {
		t.Fatalf("ListModels() error was not redacted: %v", err)
	}
}

func TestSubprocessFactoryBridgeHonorsCancelledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	bridge := &subprocessFactoryBridge{
		config:  config.FactoryConfig{PythonCommand: "ignored"},
		command: factoryBridgeHelperCommand(t, "models"),
	}
	_, err := bridge.ListModels(ctx)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("ListModels() error = %v, want context.Canceled", err)
	}
}

func TestSubprocessFactoryBridgeRun(t *testing.T) {
	bridge := &subprocessFactoryBridge{
		config:  config.FactoryConfig{PythonCommand: "ignored"},
		command: factoryBridgeHelperCommand(t, "run"),
	}
	run, err := bridge.Run(context.Background(), factoryRunRequest{Operation: "run", Model: "model-a", Prompt: "hello"})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	var got []factoryBridgeEvent
	for event := range run.Events {
		got = append(got, event)
	}
	<-run.Done
	if len(got) != 2 || got[0].Type != "text_delta" || got[0].Text != "hi" || got[1].Type != "result" || !got[1].Success {
		t.Fatalf("events = %#v", got)
	}
}

func TestSubprocessFactoryBridgeCancellationClosesInput(t *testing.T) {
	marker := t.TempDir() + "/cancelled"
	t.Setenv("FACTORY_BRIDGE_CANCEL_MARKER", marker)
	ctx, cancel := context.WithCancel(context.Background())
	bridge := &subprocessFactoryBridge{
		config:  config.FactoryConfig{PythonCommand: "ignored"},
		command: factoryBridgeHelperCommand(t, "wait-cancel"),
	}
	run, err := bridge.Run(ctx, factoryRunRequest{Operation: "run", Model: "model-a", Prompt: "wait"})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	cancel()
	for range run.Events {
	}
	<-run.Done
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("cancel marker: %v", err)
	}
}

func factoryBridgeHelperCommand(t *testing.T, mode string) func(context.Context, string, string) *exec.Cmd {
	t.Helper()
	return func(ctx context.Context, _, _ string) *exec.Cmd {
		cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=TestFactoryBridgeHelperProcess")
		cmd.Env = append(os.Environ(), "GO_WANT_FACTORY_BRIDGE_HELPER="+mode)
		return cmd
	}
}

func TestFactoryBridgeHelperProcess(t *testing.T) {
	mode := os.Getenv("GO_WANT_FACTORY_BRIDGE_HELPER")
	if mode == "" {
		return
	}
	reader := bufio.NewReader(os.Stdin)
	_, _ = reader.ReadString('\n')
	switch mode {
	case "models":
		fmt.Print(`{"type":"models","models":[{"id":"factory-model","display_name":"Factory Model","provider":"factory","reasoning_efforts":["off","high"],"default_reasoning_effort":"high"}]}`)
		os.Exit(0)
	case "run":
		fmt.Println(`{"type":"text_delta","text":"hi"}`)
		fmt.Println(`{"type":"result","success":true,"subtype":"success","text":"hi"}`)
		os.Exit(0)
	case "wait-cancel":
		_, _ = reader.ReadString('\n')
		if err := os.WriteFile(os.Getenv("FACTORY_BRIDGE_CANCEL_MARKER"), []byte("cancelled"), 0o600); err != nil {
			os.Exit(3)
		}
		os.Exit(0)
	case "leak":
		fmt.Fprint(os.Stderr, "bridge exposed "+os.Getenv("FACTORY_API_KEY"))
		os.Exit(1)
	default:
		os.Exit(2)
	}
}
