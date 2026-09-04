# Factory Droid backend verification record

Date: 2026-09-04
Base revision: `2a6b87ac` (`v7.2.149`)
State verified: Factory backend changes on top of the base revision

No Factory API key, local Droid credential, bearer token, or authentication file is included below. Live checks used the existing authenticated Droid session of the local service user and a disposable frontend proxy key.

## Toolchain

```text
Go: go1.26.7 darwin/arm64
Python SDK: droid-sdk 0.4.0
Droid: 0.212.1
```

Commands:

```bash
go version
/path/to/factory-python/bin/python -c 'import droid_sdk; print(droid_sdk.__version__)'
droid --version
```

## Offline and build gates

Commands:

```bash
go test ./...
go vet ./...
go build -o test-output ./cmd/server && rm test-output
ruff check internal/runtime/executor/factory_bridge.py
ruff format --check internal/runtime/executor/factory_bridge.py
python -m py_compile internal/runtime/executor/factory_bridge.py
git diff --check
```

Result:

```text
All Go packages passed or reported [no test files]; exit 0.
go vet: exit 0.
Required server build: exit 0; test-output removed.
Ruff: All checks passed; file already formatted.
Python byte compilation: exit 0; __pycache__ removed.
git diff --check: exit 0.
Secret-pattern scan over Factory implementation, tests, configuration, and docs: passed.
```

The deterministic fake-SDK HTTP test exercises model discovery and all selected routes in both generation modes:

```bash
go test ./internal/api \
  -run 'TestFactoryBackend(ClientCancellationClosesSDKSession|OpenAIRoutesWithFakeSDK)' \
  -count=1
```

```text
ok github.com/router-for-me/CLIProxyAPI/v7/internal/api
```

Targeted race checks:

```bash
go test -race ./internal/runtime/executor \
  -run 'Test(SubprocessFactoryBridge|FactoryExecute|PrepareFactory)' -count=1
go test -race ./internal/api \
  -run 'TestFactoryBackend(ClientCancellationClosesSDKSession|OpenAIRoutesWithFakeSDK)' \
  -count=1
```

```text
ok github.com/router-for-me/CLIProxyAPI/v7/internal/runtime/executor
ok github.com/router-for-me/CLIProxyAPI/v7/internal/api
```

These tests verify Chat Completions, legacy Completions, Responses, model listing, endpoint-specific JSON/SSE framing, usage, strict unsupported-field errors, concurrent isolation, subprocess cancellation, and eventual SDK session close after client disconnect.

Specific strict-contract regressions verify that OpenAI JSON-schema `name` is carried into the SDK schema as `title` (and conflicts are rejected), and that a terminal Droid `interrupted` result becomes a tested non-success HTTP error rather than a completed response.

## Authenticated live daemon smoke test

A temporary loopback-only configuration enabled Factory with:

- the dedicated Python SDK virtual environment;
- the absolute local Droid executable;
- `prefix: factory`;
- `autonomy: off`, `tools: []`, and built-in skills disabled; and
- a disposable frontend API key omitted from this record.

The test built `./cmd/server` to a temporary directory, started it with `--config`, waited for `/healthz`, called `/v1/models`, made a non-streaming `factory/auto` Chat Completions request, read a `factory/auto` Responses stream line by line with timestamps, sent `SIGTERM`, and waited for process exit.

Credential-free output:

```json
{
  "health": "ok",
  "factory_models": 50,
  "chat": "proxy-live-ok",
  "stream_deltas": 19,
  "first_delta_s": 0.0,
  "completed_s": 2.413
}
```

```text
daemon_shutdown=clean
```

Assertions made by the live script:

- `/healthz` returned `{"status":"ok"}`;
- `/v1/models` contained `factory/auto` and 50 Factory-prefixed entries;
- the non-streaming response text was exactly `proxy-live-ok`;
- 19 `response.output_text.delta` events arrived before `response.completed`;
- the terminal Responses event was present; and
- the daemon exited after `SIGTERM`.

## Direct live SDK bridge check

The embedded bridge source was launched with the configured Python interpreter while stdin remained open, matching the Go subprocess contract. It ran a bounded `factory/auto` request through the official SDK.

```text
{"delta_count":5,"text":"bridge-live-ok-2","elapsed_s":13.993,"exit":0}
```

The terminal text matched exactly, five partial text events preceded it, and the Python bridge exited with status 0.
