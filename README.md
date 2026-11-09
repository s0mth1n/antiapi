# AntiAPI Gateway

A lightweight, zero-dependency, high-performance API gateway written in **Go** that exposes Google Antigravity as a standard OpenAI (`/v1/chat/completions`, `/v1/models`) and Anthropic (`/v1/messages`) compatible server.

Runs natively on **macOS (Apple Silicon M1/M2/M3/M4 & Intel x86_64)** and **Linux (x86_64 & ARM64)** with zero runtime dependencies.

---

## Features

- **Native macOS & Linux Support**: Built-in Darwin and Linux terminal handling, platform-specific credential storage discovery, and cross-platform compilation.
- **Single Standalone Static Binary**: Pure Go standard library (~10MB binary, ~15MB RAM footprint).
- **Interactive Terminal UI (TUI)**: Live visual status displaying active accounts, rate limits, quota countdowns, and latency telemetry.
- **Zero-Config Token Discovery**: Automatically detects refresh tokens from:
  - macOS: `~/Library/Application Support/Antigravity/User/globalStorage/state.vscdb`
  - macOS: `~/Library/Application Support/Code/User/globalStorage/state.vscdb`
  - Linux: `~/.config/Antigravity/User/globalStorage/state.vscdb`
  - Linux: `~/.config/Code/User/globalStorage/state.vscdb`
  - Universal: `~/.anti-api/accounts.json`
  - Environment: `ANTIGRAVITY_REFRESH_TOKEN`
- **Pre-Warmed HTTP/2 Pipeline**: Reusable multiplexed H2 transport directly into Google Cloud Code PA with sub-millisecond TLS reuse.
- **High-Throughput SSE Streaming**: Pooled byte buffers with zero heap reallocation during token streaming.
- **Supported Models**:
  - `gemini-3.8-flash` (Canonical unified model with dynamic reasoning effort: minimal, low, med, high, xhigh, max, ultra)
  - `claude-sonnet-4-6` (Claude Sonnet 4.6 with Thinking)
  - `claude-opus-4-6-thinking` (Claude Opus 4.6 with Thinking)
  - `gpt-oss-120b-medium`
- **Built-in Web Dashboard**: Dark-mode UI with live quota gauges, prompt playground, and 1-click client configuration snippets.
- **Instant Cloudflare Tunnel**: Pass `--tunnel` for a zero-configuration public HTTPS URL to access your gateway remotely.

---

## Quick Start

### 1. Prerequisites

- **Go 1.22+**:
  - macOS: `brew install go`
  - Linux: `sudo apt install golang` or use your package manager / Nix.

### 2. Run the Gateway

Clone or unzip the project, open a terminal, and run:

```bash
cd antiapi
./start.sh
```

`start.sh` automatically compiles the binary on first launch and starts the interactive TUI on port **8964**.

Alternatively, run directly via Go:
```bash
go run .
# Or run with a public Cloudflare HTTPS tunnel:
go run . --tunnel
```

Web Dashboard: [http://localhost:8964](http://localhost:8964)

### 3. TUI Keyboard Controls

- `q` or `Ctrl+C`: Graceful shutdown
- `r`: Force quota & model refresh
- `l`: Reset rate-limit cooldowns

---

## Authentication Setup

AntiAPI automatically discovers your credentials if you already have Antigravity or VS Code logged in on your machine.

### If automatic discovery does not find an account:

#### Method A: Web UI (Easiest)
1. Open [http://localhost:8964](http://localhost:8964) in your browser.
2. Click **Account Settings** or **Add Account**.
3. Paste your Google refresh token or follow the on-screen prompt.

#### Method B: Environment Variable
Set the token in your shell before running:
```bash
export ANTIGRAVITY_REFRESH_TOKEN="1//04..."
./start.sh
```

#### Method C: Configuration File (`~/.anti-api/accounts.json`)
```json
{
  "accounts": [
    {
      "email": "user@example.com",
      "refreshToken": "1//04...",
      "projectId": "default"
    }
  ]
}
```

---

## Client Configurations

Any client supporting OpenAI or Anthropic format works out-of-the-box. Use `http://localhost:8964/v1` as the base URL and `antigravity` as the API key.

### Cursor

1. Open **Cursor Settings** -> **Models**.
2. Under **OpenAI API Key**, enter `antigravity`.
3. Check **Override OpenAI Base URL** and enter:
   ```text
   http://127.0.0.1:8964/v1
   ```
4. Add model name: `gemini-3.8-flash` or `claude-sonnet-4-6`.

### Cline (VS Code)

1. Open Cline Settings.
2. Select API Provider: **OpenAI Compatible**.
3. Base URL: `http://127.0.0.1:8964/v1`
4. API Key: `antigravity`
5. Model ID: `gemini-3.8-flash`

### Continue.dev (`~/.continue/config.json`)

```json
{
  "models": [
    {
      "title": "Gemini 3.8 Flash (AntiAPI)",
      "provider": "openai",
      "model": "gemini-3.8-flash",
      "apiBase": "http://127.0.0.1:8964/v1",
      "apiKey": "antigravity"
    },
    {
      "title": "Claude Sonnet 4.6 (AntiAPI)",
      "provider": "openai",
      "model": "claude-sonnet-4-6",
      "apiBase": "http://127.0.0.1:8964/v1",
      "apiKey": "antigravity"
    }
  ]
}
```

### Hermes Agent

Set the environment variables:
```bash
export OPENAI_BASE_URL="http://127.0.0.1:8964/v1"
export OPENAI_API_KEY="antigravity"
export HERMES_MODEL="claude-sonnet-4-6"
```

Or configure `~/.hermes/config.yaml`:
```yaml
openai_base_url: "http://127.0.0.1:8964/v1"
openai_api_key: "antigravity"
model: "claude-sonnet-4-6"
```

---

## Usage Examples

### 1. OpenAI Chat Completion (Streaming)
```bash
curl -N http://localhost:8964/v1/chat/completions \
  -H "Content-Type: application/json" \
  -d '{
    "model": "claude-sonnet-4-6",
    "messages": [{"role": "user", "content": "Explain goroutines in one sentence."}],
    "stream": true
  }'
```

### 2. Anthropic Messages API
```bash
curl http://localhost:8964/v1/messages \
  -H "Content-Type: application/json" \
  -d '{
    "model": "claude-sonnet-4-6",
    "messages": [{"role": "user", "content": "Hello Claude!"}]
  }'
```

### 3. Python SDK (`openai`)
```python
from openai import OpenAI

client = OpenAI(
    base_url="http://localhost:8964/v1",
    api_key="antigravity"
)

stream = client.chat.completions.create(
    model="claude-sonnet-4-6",
    messages=[{"role": "user", "content": "Write a 3-line poem about Go."}],
    stream=True
)

for chunk in stream:
    if chunk.choices[0].delta.content:
        print(chunk.choices[0].delta.content, end="", flush=True)
print()
```

---

## CLI Flags

| Flag | Default | Description |
|---|---|---|
| `-port` | `8964` | Port number to bind |
| `-host` | `0.0.0.0` | Host address to bind |
| `-no-tui` | `false` | Disable TUI and print raw streaming logs |
| `-tunnel` | `false` | Launch a Cloudflare tunnel and print the public HTTPS URL |
| `-api-key` | _(empty)_ | Require this key on all endpoints (or set `ANTIAPI_API_KEY`) |

### Endpoint Authentication

By default the gateway is open (local-only convenience). Set `-api-key` or the
`ANTIAPI_API_KEY` environment variable to require a shared secret on every
endpoint except `/health`:

```bash
ANTIAPI_API_KEY="my-secret" ./start.sh
curl http://localhost:8964/v1/models -H "Authorization: Bearer my-secret"
```

Clients send it as `Authorization: Bearer <key>` (OpenAI style) or `x-api-key`
(Anthropic style). The dashboard accepts `http://localhost:8964/?key=my-secret`
once and remembers it in the browser. **Always set a key when using `-tunnel`:**
a public tunnel URL without a key lets anyone spend your quota or add accounts.

Example:
```bash
./antiapi -port 9000 -tunnel
```

---

## Compilation

Build a standalone static binary for your target platform:

### macOS Apple Silicon (M1/M2/M3/M4)
```bash
CGO_ENABLED=0 GOOS=darwin GOARCH=arm64 go build -ldflags="-s -w" -o antiapi .
```

### macOS Intel (x86_64)
```bash
CGO_ENABLED=0 GOOS=darwin GOARCH=amd64 go build -ldflags="-s -w" -o antiapi .
```

### Linux (x86_64)
```bash
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -ldflags="-s -w" -o antiapi .
```

### Linux (ARM64)
```bash
CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -ldflags="-s -w" -o antiapi .
```
