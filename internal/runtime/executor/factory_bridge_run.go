package executor

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
)

type factoryBridgeUsage struct {
	InputTokens         int64 `json:"input_tokens"`
	OutputTokens        int64 `json:"output_tokens"`
	CacheCreationTokens int64 `json:"cache_creation_tokens"`
	CacheReadTokens     int64 `json:"cache_read_tokens"`
	ThinkingTokens      int64 `json:"thinking_tokens"`
}

type factoryBridgeEvent struct {
	Type             string              `json:"type"`
	Code             string              `json:"code,omitempty"`
	Message          string              `json:"message,omitempty"`
	Text             string              `json:"text,omitempty"`
	Success          bool                `json:"success,omitempty"`
	Subtype          string              `json:"subtype,omitempty"`
	Usage            *factoryBridgeUsage `json:"usage,omitempty"`
	StructuredOutput map[string]any      `json:"structured_output,omitempty"`
	SessionID        string              `json:"session_id,omitempty"`
}

type factoryBridgeRun struct {
	Events <-chan factoryBridgeEvent
	Done   <-chan struct{}
}

func (b *subprocessFactoryBridge) Run(ctx context.Context, request factoryRunRequest) (*factoryBridgeRun, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	payload, errMarshal := json.Marshal(request)
	if errMarshal != nil {
		return nil, fmt.Errorf("encode Factory SDK run request: %w", errMarshal)
	}
	pythonCommand := strings.TrimSpace(b.config.PythonCommand)
	if pythonCommand == "" {
		pythonCommand = "python3"
	}
	command := b.command
	if command == nil {
		command = func(ctx context.Context, executable, source string) *exec.Cmd {
			return exec.CommandContext(ctx, executable, "-c", source)
		}
	}
	cmd := command(ctx, pythonCommand, factoryBridgeSource)
	stdin, errStdin := cmd.StdinPipe()
	if errStdin != nil {
		return nil, fmt.Errorf("open Factory SDK bridge input: %w", errStdin)
	}
	stdout, errStdout := cmd.StdoutPipe()
	if errStdout != nil {
		_ = stdin.Close()
		return nil, fmt.Errorf("open Factory SDK bridge output: %w", errStdout)
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	cmd.Cancel = stdin.Close
	cmd.WaitDelay = 5 * time.Second
	if errStart := cmd.Start(); errStart != nil {
		_ = stdin.Close()
		return nil, factoryBridgeStartError(pythonCommand, errStart)
	}
	cliproxyexecutor.MarkUpstreamAttempt(ctx)
	if _, errWrite := stdin.Write(append(payload, '\n')); errWrite != nil {
		_ = stdin.Close()
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		return nil, fmt.Errorf("write Factory SDK bridge request: %w", errWrite)
	}

	events := make(chan factoryBridgeEvent, 16)
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer close(events)
		defer func() { _ = stdin.Close() }()
		scanner := bufio.NewScanner(stdout)
		scanner.Buffer(make([]byte, 64*1024), 10*1024*1024)
		terminal := false
		abortBridge := false
		for scanner.Scan() {
			var event factoryBridgeEvent
			if errDecode := json.Unmarshal(scanner.Bytes(), &event); errDecode != nil {
				event = factoryBridgeEvent{Type: "error", Code: "invalid_bridge_response", Message: errDecode.Error()}
				terminal = true
				abortBridge = true
			}
			if event.Type != "text_delta" && event.Type != "result" && event.Type != "error" {
				event = factoryBridgeEvent{Type: "error", Code: "invalid_bridge_response", Message: fmt.Sprintf("unexpected event type %q", event.Type)}
				terminal = true
				abortBridge = true
			}
			if event.Type == "result" || event.Type == "error" {
				event.Message = redactFactorySecret(event.Message)
				terminal = true
			}
			select {
			case events <- event:
			case <-ctx.Done():
				_ = cmd.Wait()
				return
			}
			if terminal {
				if abortBridge {
					_ = stdin.Close()
				}
				break
			}
		}
		errScan := scanner.Err()
		if errScan != nil {
			_ = stdin.Close()
		}
		errWait := cmd.Wait()
		if ctx.Err() != nil {
			return
		}
		if terminal {
			return
		}
		var event factoryBridgeEvent
		switch {
		case errScan != nil:
			event = factoryBridgeEvent{Type: "error", Code: "bridge_read_error", Message: errScan.Error()}
		case errWait != nil:
			detail := strings.TrimSpace(stderr.String())
			if detail == "" {
				detail = errWait.Error()
			}
			event = factoryBridgeEvent{Type: "error", Code: "bridge_exit", Message: redactFactorySecret(detail)}
		default:
			event = factoryBridgeEvent{Type: "error", Code: "bridge_eof", Message: "Factory SDK bridge exited before a terminal result"}
		}
		select {
		case events <- event:
		case <-ctx.Done():
		}
	}()
	return &factoryBridgeRun{Events: events, Done: done}, nil
}

func factoryBridgeStartError(pythonCommand string, err error) error {
	var execErr *exec.Error
	if errors.As(err, &execErr) || errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("Factory Python executable %q is unavailable: %w", pythonCommand, err)
	}
	return fmt.Errorf("start Factory SDK bridge: %w", err)
}
