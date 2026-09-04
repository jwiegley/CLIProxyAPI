package synthesizer

import (
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

func TestConfigSynthesizerFactory(t *testing.T) {
	now := time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC)
	cfg := &config.Config{Factory: config.FactoryConfig{
		Enabled:        true,
		PythonCommand:  "/venv/bin/python",
		DroidCommand:   "/bin/droid",
		CWD:            "/workspace",
		Prefix:         "factory",
		ExcludedModels: []string{"disabled-model"},
	}}
	auths, err := NewConfigSynthesizer().Synthesize(&SynthesisContext{
		Config:      cfg,
		Now:         now,
		IDGenerator: NewStableIDGenerator(),
	})
	if err != nil {
		t.Fatalf("Synthesize() error = %v", err)
	}
	if len(auths) != 1 {
		t.Fatalf("len(auths) = %d, want 1", len(auths))
	}
	auth := auths[0]
	if auth.Provider != "factory" || auth.Label != "factory-sdk" || auth.Prefix != "factory" {
		t.Fatalf("auth identity = %#v", auth)
	}
	if auth.Status != coreauth.StatusActive || !strings.HasPrefix(auth.ID, "factory:apikey:") {
		t.Fatalf("auth status/id = %q/%q", auth.Status, auth.ID)
	}
	if auth.Attributes["python_command"] != "/venv/bin/python" || auth.Attributes["droid_command"] != "/bin/droid" || auth.Attributes["cwd"] != "/workspace" {
		t.Fatalf("auth runtime attributes = %#v", auth.Attributes)
	}
	if auth.Attributes["auth_kind"] != coreauth.AuthKindAPIKey {
		t.Fatalf("auth_kind = %q", auth.Attributes["auth_kind"])
	}
	if auth.Attributes["excluded_models"] != "disabled-model" {
		t.Fatalf("excluded_models = %q", auth.Attributes["excluded_models"])
	}
	if _, ok := auth.Attributes["api_key"]; ok {
		t.Fatal("Factory auth must not persist FACTORY_API_KEY")
	}
}

func TestConfigSynthesizerDefaultsFactoryRuntime(t *testing.T) {
	auths, err := NewConfigSynthesizer().Synthesize(&SynthesisContext{
		Config:      &config.Config{Factory: config.FactoryConfig{Enabled: true}},
		Now:         time.Now(),
		IDGenerator: NewStableIDGenerator(),
	})
	if err != nil {
		t.Fatalf("Synthesize() error = %v", err)
	}
	if len(auths) != 1 {
		t.Fatalf("len(auths) = %d, want 1", len(auths))
	}
	auth := auths[0]
	if auth.Prefix != "factory" || auth.Attributes["python_command"] != "python3" || auth.Attributes["droid_command"] != "droid" {
		t.Fatalf("default Factory auth = %#v", auth)
	}
}

func TestConfigSynthesizerSkipsDisabledFactory(t *testing.T) {
	auths, err := NewConfigSynthesizer().Synthesize(&SynthesisContext{
		Config:      &config.Config{},
		Now:         time.Now(),
		IDGenerator: NewStableIDGenerator(),
	})
	if err != nil {
		t.Fatalf("Synthesize() error = %v", err)
	}
	if len(auths) != 0 {
		t.Fatalf("len(auths) = %d, want 0", len(auths))
	}
}
