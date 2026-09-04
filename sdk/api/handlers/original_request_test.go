package handlers

import (
	"context"
	"testing"

	coreexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
)

func TestOriginalEndpointRequestMetadataIsCloned(t *testing.T) {
	body := []byte(`{"prompt":"hello"}`)
	ctx := WithOriginalEndpointRequest(context.Background(), body)
	body[0] = 'x'

	first, ok := requestExecutionMetadata(ctx)[coreexecutor.OriginalEndpointRequestMetadataKey].([]byte)
	if !ok || string(first) != `{"prompt":"hello"}` {
		t.Fatalf("first original request = %q", first)
	}
	first[0] = 'x'
	second, ok := requestExecutionMetadata(ctx)[coreexecutor.OriginalEndpointRequestMetadataKey].([]byte)
	if !ok || string(second) != `{"prompt":"hello"}` {
		t.Fatalf("second original request = %q", second)
	}
}
