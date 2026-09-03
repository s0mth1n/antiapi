# Anti-API Gateway

A lightweight, zero-dependency, high-performance API gateway built in **Go** that exposes Google Antigravity as a standard OpenAI (`/v1/chat/completions`, `/v1/models`) and Anthropic (`/v1/messages`) compatible server.

Designed for NixOS and local agent workflows (Hermes, Cline, Cursor, Continue, etc.) with zero runtime dependencies.

---

## ✨ Features

- **Single Standalone Static Binary**: Compiled with pure Go standard library (~10MB binary, ~15MB RAM footprint).
- **Zero-Config Auto Discovery**: Automatically locates your Antigravity refresh token from `~/.anti-api/accounts.json`, Antigravity IDE SQLite DB, or `ANTIGRAVITY_REFRESH_TOKEN`.
- **All Antigravity Models Supported**:
  - `claude-sonnet-4-6` (Claude Sonnet 4.6 with Thinking)
  - `claude-opus-4-6-thinking` (Claude Opus 4.6 with Thinking)
  - `gemini-3.8-flash-high` & `gemini-3.7-flash-high`
  - `gemini-3.7-flash-medium` & `gemini-3.6-flash-medium`
  - `gemini-3.1-pro-high` & `gemini-3.1-pro-low`
  - `gpt-oss-120b-medium`
- **Smart Model Resolution**: Never crashes with `400: No account routing configured`. Automatically maps aliases (`claude-sonnet-4.6` -> `claude-sonnet-4-6`, `gemini-3.8` -> `gemini-3.7-flash-high`) and falls back gracefully for unknown model names.
- **True SSE Streaming**: Native HTTP chunking with instant Time-To-First-Token (TTFT).
- **Built-in Web Dashboard**: Modern dark-mode UI with live quota progress bars, prompt playground, and 1-click configuration snippets.

---

## 🚀 Quick Start

### 1. Launch the Server
```bash
cd ~/antiapi
./start.sh
```
The server will start on port **8964** by default.

Web Dashboard: [http://localhost:8964/](http://localhost:8964/)

---

## 🤖 Hermes Agent Integration

To connect Hermes to Antigravity:

### Option A: Configuration File (`~/.hermes/config.yaml`)
```yaml
openai_base_url: "http://127.0.0.1:8964/v1"
openai_api_key: "antigravity"
model: "claude-sonnet-4-6"  # or "gemini-3.7-flash-high"
```

### Option B: Environment Variables
```bash
export OPENAI_BASE_URL="http://127.0.0.1:8964/v1"
export OPENAI_API_KEY="antigravity"
export HERMES_MODEL="claude-sonnet-4-6"
```

### Option C: Hermes CLI Flags
```bash
hermes --base-url http://127.0.0.1:8964/v1 --model claude-sonnet-4-6
```

---

## 📡 API Endpoints

| Method | Endpoint | Description |
|---|---|---|
| `POST` | `/v1/chat/completions` | OpenAI compatible chat completions (streaming & non-streaming) |
| `POST` | `/v1/messages` | Anthropic Claude compatible messages API |
| `GET` | `/v1/models` | List all available Antigravity models |
| `GET` | `/api/status` or `/quota` | Live quota percentages and account info |
| `GET` | `/health` | Health check endpoint |
| `GET` | `/` | Web dashboard & interactive playground |

---

## 🧪 Testing Examples

### 1. OpenAI Streaming Completion
```bash
curl -N http://localhost:8964/v1/chat/completions \
  -H "Content-Type: application/json" \
  -d '{
    "model": "claude-sonnet-4-6",
    "messages": [{"role": "user", "content": "Write a haiku about programming."}],
    "stream": true
  }'
```

### 2. Gemini 3.8 / 3.7 Flash High
```bash
curl -N http://localhost:8964/v1/chat/completions \
  -H "Content-Type: application/json" \
  -d '{
    "model": "gemini-3.8-flash-high",
    "messages": [{"role": "user", "content": "Hello world"}],
    "stream": true
  }'
```

### 3. Anthropic Messages API
```bash
curl http://localhost:8964/v1/messages \
  -H "Content-Type: application/json" \
  -d '{
    "model": "claude-sonnet-4-6",
    "messages": [{"role": "user", "content": "Explain quantum computing in one sentence."}]
  }'
```

### 4. Python OpenAI Client
```python
from openai import OpenAI

client = OpenAI(
    base_url="http://localhost:8964/v1",
    api_key="antigravity"
)

response = client.chat.completions.create(
    model="claude-sonnet-4-6",
    messages=[{"role": "user", "content": "Say hello!"}],
    stream=True
)

for chunk in response:
    content = chunk.choices[0].delta.content
    if content:
        print(content, end="", flush=True)
print()
```

---

## 🛠️ Building From Source

To rebuild the binary using Nix:
```bash
cd ~/antiapi
nix-shell -p go --run "go build -o antiapi ."
```
