# Factory Droid backend

CLIProxyAPI can expose Factory models through its existing OpenAI-compatible HTTP server. The backend uses the official Python `droid-sdk`; it does not call Factory's private Droid protocol or run a second proxy.

## Prerequisites

- Go 1.26 or newer to build CLIProxyAPI
- Python 3.10 or newer
- `droid` on `PATH`, or an absolute `droid-command`
- an authenticated local Droid session or `FACTORY_API_KEY` in the daemon environment

The implementation was exercised with `droid-sdk` 0.4.0 and Droid 0.212.1.

## Install the SDK runtime

Use a dedicated virtual environment so the daemon always invokes the intended SDK version:

```bash
python3 -m venv ~/.local/share/cli-proxy-api/factory-python
~/.local/share/cli-proxy-api/factory-python/bin/pip install 'droid-sdk>=0.4.0'
~/.local/share/cli-proxy-api/factory-python/bin/python -c 'import droid_sdk; print(droid_sdk.__version__)'
droid --version
```

An SDK checkout may be installed instead:

```bash
~/.local/share/cli-proxy-api/factory-python/bin/pip install /path/to/droid-sdk-python
```

Authenticate with the local Droid CLI, or arrange for the service supervisor to provide `FACTORY_API_KEY`. Do not place the Factory key in `config.yaml` or in command-line arguments.

## Configure CLIProxyAPI

Start from `config.example.yaml`. A minimal local-only configuration is:

```yaml
host: "127.0.0.1"
port: 8317

# This authenticates callers of the proxy; it is not the Factory key.
api-keys:
  - "replace-with-a-long-random-local-key"

factory:
  enabled: true
  python-command: "/home/me/.local/share/cli-proxy-api/factory-python/bin/python"
  droid-command: "droid"
  cwd: "/path/to/default/workspace"
  prefix: "factory"
  excluded-models: []

  # Safe defaults: Droid cannot invoke native tools or built-in skills.
  autonomy: "off"
  tools: []
  enable-builtin-skills: false
```

`prefix` defaults to `factory` and is always applied to the advertised catalog, so Factory Router is addressed as `factory/auto` and a discovered model as `factory/<model-id>`. The prefix prevents an identically named model from another configured provider from receiving the request.

### Native Droid tools

OpenAI client-defined `tools` are not equivalent to Droid's server-side tools and are rejected. Native Droid tools can instead be enabled explicitly in backend configuration:

```yaml
factory:
  enabled: true
  autonomy: "high"
  tools: ["Read"]
  enable-builtin-skills: true
```

`tools` is a restrictive allowlist. `autonomy` accepts `off`, `low`, `medium`, or `high`. Enabling tools lets remote prompts act with the daemon user's filesystem and process permissions; keep the proxy on loopback, retain frontend API-key authentication, and grant only tools required for the deployment.

## Build and run

```bash
go build -o cli-proxy-api ./cmd/server
./cli-proxy-api --config /absolute/path/to/config.yaml
```

The server handles `SIGINT` and `SIGTERM` and performs graceful shutdown. For unattended operation, run the same command under launchd, systemd, or another supervisor, set its working directory deliberately, and provide `FACTORY_API_KEY` through the supervisor's protected environment when local Droid authentication is not used. Stop it through the supervisor or send `SIGTERM`; do not kill the Python/Droid children independently.

Each inference request creates one short-lived Python bridge and one Droid SDK session. This deliberately favors cancellation and failure isolation over a persistent multiplexed helper. Concurrent requests use independent sessions.

## Verify the daemon

Set the frontend proxy key, then check health and model discovery:

```bash
export CLIPROXY_API_KEY='replace-with-a-long-random-local-key'
curl -fsS http://127.0.0.1:8317/healthz
curl -fsS http://127.0.0.1:8317/v1/models \
  -H "Authorization: Bearer $CLIPROXY_API_KEY"
```

Make a non-streaming request:

```bash
curl -fsS http://127.0.0.1:8317/v1/chat/completions \
  -H "Authorization: Bearer $CLIPROXY_API_KEY" \
  -H 'Content-Type: application/json' \
  -d '{"model":"factory/auto","messages":[{"role":"user","content":"Reply with exactly: proxy ready"}]}'
```

Verify incremental SSE delivery:

```bash
curl -N http://127.0.0.1:8317/v1/responses \
  -H "Authorization: Bearer $CLIPROXY_API_KEY" \
  -H 'Content-Type: application/json' \
  -d '{"model":"factory/auto","input":"Count from one to five, one number per line.","stream":true}'
```

## Compatibility matrix

A Factory request is either mapped faithfully or rejected with an OpenAI-style `invalid_request_error`. Unknown non-null fields are rejected instead of silently ignored.

| OpenAI surface | Status | Droid mapping or limitation |
| --- | --- | --- |
| `GET /v1/models` and `model` | Supported | Enabled models and reasoning levels come from `droid_sdk.list_models()`. Every returned, prefixed ID routes to Factory. |
| `/v1/chat/completions` | Supported with limits | Leading text `system`/`developer` instructions followed by exactly one final `user` message. |
| `/v1/completions` | Supported with limits | One string `prompt`; CLIProxyAPI performs its existing completion/chat response conversion. |
| `/v1/responses` | Supported with limits | Optional string `instructions` and a string input, direct content parts, or one final user message. |
| Non-streaming and HTTP SSE | Supported | SDK text deltas are flushed incrementally; terminal status, finish reason, and usage use endpoint-native schemas. |
| Cancellation/disconnect | Supported | Request cancellation closes bridge input, interrupts Droid, waits for child cleanup, and closes the SDK session. |
| Prior assistant/tool history | Rejected | The SDK cannot inject arbitrary external history. Flattening history into a prompt would not be faithful. |
| `previous_response_id`, `conversation`, storage/background mode | Rejected | This backend has no OpenAI response store. |
| Reasoning effort | Supported | Validated against the selected model's discovered effort list and passed to `Session`. |
| Reasoning summaries/raw thinking | Rejected | Droid thinking events are not OpenAI reasoning summaries and are not exposed as chain-of-thought. |
| JSON object and JSON Schema output | Supported | Mapped to `droid_sdk.JsonSchema`; OpenAI `name` becomes the schema `title` (a conflicting title is rejected), and validated output is returned as response text. |
| Image input | Supported with limits | Base64 PNG, JPEG, GIF, and WebP data URLs within SDK limits; remote URLs and detail levels other than `auto` are rejected. |
| Document input | Supported with limits | Inline base64 data URLs containing UTF-8 text or PDF data; remote URLs and OpenAI file IDs are rejected. |
| Client function/tool calling | Rejected | Droid tools execute server-side and cannot implement OpenAI's caller-executed tool round trip faithfully. |
| Factory-native tools and skills | Configuration only | Disabled by default; explicit restrictive allowlist and autonomy policy required. |
| Usage | Supported | Input, output, cache-read, cache-creation, and thinking counts feed CLIProxyAPI accounting; representable fields appear in OpenAI usage objects. |
| Multiple choices (`n`) | Rejected | One Droid turn produces one result. |
| Sampling/token shaping | Rejected | `temperature`, `top_p`, penalties, `logit_bias`, `seed`, `stop`, token limits, and logprobs have no public SDK equivalent. |
| Other OpenAI services/modalities | Out of scope | WebSocket Responses, response compaction, generated images, audio, embeddings, files, batches, fine-tuning, and assistants/threads are not implemented. |

## Verification

See the credential-free [Factory backend verification record](factory-droid-verification.md) for the exact offline, race, build, and authenticated live-smoke evidence.

## Troubleshooting

- **Factory models are absent:** run the configured Python executable with `-c 'import droid_sdk'`, verify `droid-command`, then run the SDK's `list_models()` under the same user and working directory.
- **`sdk_unavailable`:** install `droid-sdk` into the exact interpreter named by `python-command`.
- **Authentication failure:** authenticate `droid` as the service user or provide `FACTORY_API_KEY` to the service environment. The proxy frontend key is unrelated.
- **A request returns `unsupported_parameter`:** remove the named field or use a backend that implements that OpenAI semantic. Fields are never silently discarded.
- **A tool does not run:** confirm its exact Droid catalog ID is in `factory.tools` and that the selected autonomy level permits it.

Prompts and model output may appear in CLIProxyAPI request logs when request logging is enabled. Protect logs as sensitive data and keep debug/request logging off unless needed.
