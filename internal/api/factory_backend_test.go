package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
	runtimeexecutor "github.com/router-for-me/CLIProxyAPI/v7/internal/runtime/executor"
	_ "github.com/router-for-me/CLIProxyAPI/v7/internal/translator"
	sdkaccess "github.com/router-for-me/CLIProxyAPI/v7/sdk/access"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	sdkconfig "github.com/router-for-me/CLIProxyAPI/v7/sdk/config"
	"github.com/tidwall/gjson"
)

func TestFactoryBackendOpenAIRoutesWithFakeSDK(t *testing.T) {
	server, marker := newFactoryBackendTestServer(t)

	models := factoryBackendRequest(t, server, http.MethodGet, "/v1/models", "")
	if models.Code != http.StatusOK || gjson.GetBytes(models.Body.Bytes(), `data.#(id=="factory/fake-model").id`).String() != "factory/fake-model" {
		t.Fatalf("models response = %d %s", models.Code, models.Body.String())
	}

	tests := []struct {
		name     string
		path     string
		body     string
		contains []string
	}{
		{
			name:     "chat non-stream",
			path:     "/v1/chat/completions",
			body:     `{"model":"factory/fake-model","messages":[{"role":"user","content":"hello"}]}`,
			contains: []string{`"object":"chat.completion"`, `"content":"fake reply"`},
		},
		{
			name:     "chat stream",
			path:     "/v1/chat/completions",
			body:     `{"model":"factory/fake-model","messages":[{"role":"user","content":"hello"}],"stream":true,"stream_options":{"include_usage":true}}`,
			contains: []string{`"object":"chat.completion.chunk"`, `"content":"fake "`, `"content":"reply"`, `data: [DONE]`},
		},
		{
			name:     "completion non-stream",
			path:     "/v1/completions",
			body:     `{"model":"factory/fake-model","prompt":"hello"}`,
			contains: []string{`"object":"text_completion"`, `"text":"fake reply"`},
		},
		{
			name:     "completion stream",
			path:     "/v1/completions",
			body:     `{"model":"factory/fake-model","prompt":"hello","stream":true}`,
			contains: []string{`"object":"text_completion"`, `"text":"fake "`, `"text":"reply"`, `data: [DONE]`},
		},
		{
			name:     "responses non-stream",
			path:     "/v1/responses",
			body:     `{"model":"factory/fake-model","input":"hello"}`,
			contains: []string{`"object":"response"`, `"status":"completed"`, `"text":"fake reply"`},
		},
		{
			name:     "responses stream",
			path:     "/v1/responses",
			body:     `{"model":"factory/fake-model","input":"hello","stream":true}`,
			contains: []string{`event: response.created`, `event: response.output_text.delta`, `event: response.completed`},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			response := factoryBackendRequest(t, server, http.MethodPost, test.path, test.body)
			if response.Code != http.StatusOK {
				t.Fatalf("status = %d; body=%s", response.Code, response.Body.String())
			}
			for _, want := range test.contains {
				if !strings.Contains(response.Body.String(), want) {
					t.Fatalf("response missing %q: %s", want, response.Body.String())
				}
			}
		})
	}

	before, err := os.ReadFile(marker)
	if err != nil {
		t.Fatalf("read lifecycle marker: %v", err)
	}
	invalid := factoryBackendRequest(t, server, http.MethodPost, "/v1/chat/completions", `{"model":"factory/fake-model","messages":[{"role":"user","content":"hello"}],"tools":[]}`)
	if invalid.Code != http.StatusBadRequest || !strings.Contains(invalid.Body.String(), `"code":"unsupported_parameter"`) {
		t.Fatalf("unsupported request = %d %s", invalid.Code, invalid.Body.String())
	}
	after, err := os.ReadFile(marker)
	if err != nil {
		t.Fatalf("read lifecycle marker after invalid request: %v", err)
	}
	if string(after) != string(before) {
		t.Fatal("unsupported request reached the Factory SDK bridge")
	}
	if opens, closes := strings.Count(string(after), "open\n"), strings.Count(string(after), "close\n"); opens != len(tests) || closes != opens {
		t.Fatalf("session lifecycle opens=%d closes=%d marker=%q", opens, closes, after)
	}
}

type factoryFlushRecorder struct {
	*httptest.ResponseRecorder
	flushed chan struct{}
	once    sync.Once
}

func (r *factoryFlushRecorder) Flush() {
	r.once.Do(func() { close(r.flushed) })
	r.ResponseRecorder.Flush()
}

func TestFactoryBackendClientCancellationClosesSDKSession(t *testing.T) {
	server, marker := newFactoryBackendTestServer(t)
	ctx, cancel := context.WithCancel(context.Background())
	request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"factory/fake-model","messages":[{"role":"user","content":"wait"}],"stream":true}`)).WithContext(ctx)
	request.Header.Set("Authorization", "Bearer test-key")
	request.Header.Set("Content-Type", "application/json")
	recorder := &factoryFlushRecorder{ResponseRecorder: httptest.NewRecorder(), flushed: make(chan struct{})}
	done := make(chan struct{})
	go func() {
		server.engine.ServeHTTP(recorder, request)
		close(done)
	}()

	select {
	case <-recorder.flushed:
	case <-time.After(10 * time.Second):
		t.Fatal("timed out waiting for first Factory stream chunk")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("Factory handler did not stop after client cancellation")
	}
	lifecycle := waitForFactorySessionClose(t, marker)
	if strings.Count(string(lifecycle), "open\n") != 1 || strings.Count(string(lifecycle), "close\n") != 1 {
		t.Fatalf("cancelled session lifecycle = %q", lifecycle)
	}
}

func waitForFactorySessionClose(t *testing.T, marker string) []byte {
	t.Helper()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	timeout := time.After(10 * time.Second)
	for {
		lifecycle, err := os.ReadFile(marker)
		if err == nil && strings.Contains(string(lifecycle), "close\n") {
			return lifecycle
		}
		select {
		case <-ticker.C:
		case <-timeout:
			t.Fatalf("timed out waiting for Factory session cleanup; marker=%q error=%v", lifecycle, err)
		}
	}
}

func factoryBackendTestPython(t *testing.T) string {
	t.Helper()
	for _, name := range []string{"python3", "python"} {
		python, err := exec.LookPath(name)
		if err != nil {
			continue
		}
		if err := exec.Command(python, "-c", "import sys; assert sys.version_info >= (3, 10)").Run(); err == nil {
			return python
		}
	}
	t.Skip("Python 3.10 or newer is required for the Factory bridge integration test")
	return ""
}

func newFactoryBackendTestServer(t *testing.T) (*Server, string) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	python := factoryBackendTestPython(t)
	tmp := t.TempDir()
	moduleDir := filepath.Join(tmp, "droid_sdk")
	if err := os.MkdirAll(moduleDir, 0o700); err != nil {
		t.Fatalf("create fake SDK module: %v", err)
	}
	if err := os.WriteFile(filepath.Join(moduleDir, "__init__.py"), []byte(fakeDroidSDKModule), 0o600); err != nil {
		t.Fatalf("write fake SDK module: %v", err)
	}
	marker := filepath.Join(tmp, "lifecycle.log")
	t.Setenv("PYTHONPATH", tmp)
	t.Setenv("FAKE_DROID_MARKER", marker)

	cfg := &config.Config{
		SDKConfig: sdkconfig.SDKConfig{APIKeys: []string{"test-key"}},
		Factory: config.FactoryConfig{
			Enabled:       true,
			PythonCommand: python,
			DroidCommand:  "fake-droid",
			CWD:           tmp,
			Prefix:        "factory",
		},
		AuthDir: filepath.Join(tmp, "auth"),
	}
	cfg.SanitizeFactory()
	manager := coreauth.NewManager(nil, nil, nil)
	executor := runtimeexecutor.NewFactoryExecutor(cfg)
	manager.RegisterExecutor(executor)
	auth := &coreauth.Auth{
		ID:         "factory-fake-sdk-auth",
		Provider:   "factory",
		Prefix:     "factory",
		Status:     coreauth.StatusActive,
		Attributes: map[string]string{"auth_kind": "apikey", "source": "config:factory[test]"},
	}
	if _, err := manager.Register(context.Background(), auth); err != nil {
		t.Fatalf("register Factory auth: %v", err)
	}
	models, err := executor.ListModels(context.Background())
	if err != nil {
		t.Fatalf("discover fake Factory models: %v", err)
	}
	for _, model := range models {
		model.ID = "factory/" + model.ID
	}
	modelRegistry := registry.GetGlobalRegistry()
	modelRegistry.RegisterClient(auth.ID, "factory", models)
	manager.RefreshSchedulerEntry(auth.ID)
	t.Cleanup(func() { modelRegistry.UnregisterClient(auth.ID) })

	accessManager := sdkaccess.NewManager()
	return NewServer(cfg, manager, accessManager, filepath.Join(tmp, "config.yaml")), marker
}

func factoryBackendRequest(t *testing.T, server *Server, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest(method, path, strings.NewReader(body))
	request.Header.Set("Authorization", "Bearer test-key")
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	server.engine.ServeHTTP(response, request)
	return response
}

const fakeDroidSDKModule = `
import asyncio
import os
from enum import Enum
from types import SimpleNamespace

class Value:
    def __init__(self, value): self.value = value

class Runtime:
    def __init__(self, **kwargs): self.kwargs = kwargs

class SessionConfig:
    def __init__(self, **kwargs): self.kwargs = kwargs

class Autonomy(str, Enum):
    OFF = "off"
    LOW = "low"
    MEDIUM = "medium"
    HIGH = "high"

class ReasoningEffort(str, Enum):
    NONE = "none"
    OFF = "off"
    LOW = "low"
    HIGH = "high"

class JsonSchema:
    def __init__(self, schema): self.schema = schema

class Image:
    @classmethod
    def from_bytes(cls, data, *, media_type): return (data, media_type)

class Document:
    @classmethod
    def from_bytes(cls, data, *, name=None): return ("pdf", data, name)
    @classmethod
    def from_text(cls, data, *, name=None, mime=None): return ("text", data, name, mime)

class TextDelta:
    def __init__(self, text): self.text = text

class FakeStream:
    def __init__(self, output, prompt):
        self.wait = prompt == "wait"
        self.index = 0
        self.events = iter([TextDelta("fake "), TextDelta("reply")])
        self.result = SimpleNamespace(
            success=True,
            subtype="success",
            text='{"ok":true}' if output else "fake reply",
            usage=SimpleNamespace(input_tokens=7, output_tokens=2, cache_creation_tokens=1, cache_read_tokens=3, thinking_tokens=1),
            structured_output={"ok": True} if output else None,
            session_id="fake-session",
            error=None,
            structured_output_error=None,
        )
    async def __aenter__(self): return self
    async def __aexit__(self, exc_type, exc, tb): return False
    def __aiter__(self): return self
    async def __anext__(self):
        if self.wait:
            if self.index == 0:
                self.index += 1
                return TextDelta("started")
            await asyncio.Event().wait()
        try: return next(self.events)
        except StopIteration: raise StopAsyncIteration

class Session:
    def __init__(self, **kwargs): self.kwargs = kwargs
    async def __aenter__(self):
        with open(os.environ["FAKE_DROID_MARKER"], "a") as marker: marker.write("open\n")
        return self
    async def __aexit__(self, exc_type, exc, tb):
        with open(os.environ["FAKE_DROID_MARKER"], "a") as marker: marker.write("close\n")
        return False
    def stream(self, prompt, **kwargs): return FakeStream(kwargs.get("output"), prompt)

async def list_models(**kwargs):
    return [SimpleNamespace(
        id="fake-model",
        display_name="Fake Factory Model",
        model_provider=Value("factory"),
        supported_reasoning_efforts=[Value("off"), Value("high")],
        default_reasoning_effort=Value("high"),
        no_image_support=False,
    )]
`
