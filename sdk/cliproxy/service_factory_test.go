package cliproxy

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
	runtimeexecutor "github.com/router-for-me/CLIProxyAPI/v7/internal/runtime/executor"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	coreexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
)

type serviceFactoryModelExecutor struct {
	models []*registry.ModelInfo
	err    error
}

func (*serviceFactoryModelExecutor) Identifier() string { return "factory" }
func (*serviceFactoryModelExecutor) Execute(context.Context, *coreauth.Auth, coreexecutor.Request, coreexecutor.Options) (coreexecutor.Response, error) {
	return coreexecutor.Response{}, nil
}
func (*serviceFactoryModelExecutor) ExecuteStream(context.Context, *coreauth.Auth, coreexecutor.Request, coreexecutor.Options) (*coreexecutor.StreamResult, error) {
	return &coreexecutor.StreamResult{}, nil
}
func (*serviceFactoryModelExecutor) Refresh(_ context.Context, auth *coreauth.Auth) (*coreauth.Auth, error) {
	return auth, nil
}
func (*serviceFactoryModelExecutor) CountTokens(context.Context, *coreauth.Auth, coreexecutor.Request, coreexecutor.Options) (coreexecutor.Response, error) {
	return coreexecutor.Response{}, nil
}
func (*serviceFactoryModelExecutor) HttpRequest(context.Context, *coreauth.Auth, *http.Request) (*http.Response, error) {
	return nil, nil
}
func (e *serviceFactoryModelExecutor) ListModels(context.Context) ([]*registry.ModelInfo, error) {
	return e.models, e.err
}

func TestRegisterModelsForFactoryAuthUsesDiscoveredCatalog(t *testing.T) {
	const authID = "factory-service-model-test-auth"
	modelRegistry := GlobalModelRegistry()
	modelRegistry.UnregisterClient(authID)
	t.Cleanup(func() { modelRegistry.UnregisterClient(authID) })

	manager := coreauth.NewManager(nil, nil, nil)
	manager.RegisterExecutor(&serviceFactoryModelExecutor{models: []*registry.ModelInfo{
		{ID: "factory-service-model-a", Object: "model", Type: "factory"},
		{ID: "factory-service-model-b", Object: "model", Type: "factory"},
	}})
	service := &Service{cfg: &config.Config{}, coreManager: manager}
	auth := &coreauth.Auth{
		ID:       authID,
		Provider: "factory",
		Prefix:   "factory-test",
		Status:   coreauth.StatusActive,
		Attributes: map[string]string{
			"excluded_models": "factory-service-model-b",
		},
	}

	service.registerModelsForAuth(context.Background(), auth)
	models := modelRegistry.GetModelsForClient(authID)
	if len(models) != 1 || models[0].ID != "factory-test/factory-service-model-a" {
		t.Fatalf("registered Factory models = %#v", models)
	}
}

func TestRegisterModelsForFactoryAuthDropsStaleCatalogOnDiscoveryError(t *testing.T) {
	const authID = "factory-service-model-error-auth"
	modelRegistry := GlobalModelRegistry()
	modelRegistry.RegisterClient(authID, "factory", []*registry.ModelInfo{{ID: "stale-factory-model"}})
	t.Cleanup(func() { modelRegistry.UnregisterClient(authID) })

	manager := coreauth.NewManager(nil, nil, nil)
	manager.RegisterExecutor(&serviceFactoryModelExecutor{err: errors.New("not authenticated")})
	service := &Service{cfg: &config.Config{}, coreManager: manager}
	service.registerModelsForAuth(context.Background(), &coreauth.Auth{ID: authID, Provider: "factory", Status: coreauth.StatusActive})

	if models := modelRegistry.GetModelsForClient(authID); len(models) != 0 {
		t.Fatalf("stale Factory models remain registered: %#v", models)
	}
}

func TestRegisterExecutorForFactoryAuth(t *testing.T) {
	manager := coreauth.NewManager(nil, nil, nil)
	service := &Service{
		cfg:         &config.Config{Factory: config.FactoryConfig{Enabled: true}},
		coreManager: manager,
	}
	service.ensureExecutorsForAuth(&coreauth.Auth{ID: "factory-auth", Provider: "factory", Status: coreauth.StatusActive})

	executor, ok := manager.Executor("factory")
	if !ok {
		t.Fatal("Factory executor was not registered")
	}
	if _, ok := executor.(*runtimeexecutor.FactoryExecutor); !ok {
		t.Fatalf("Factory executor type = %T", executor)
	}
}
