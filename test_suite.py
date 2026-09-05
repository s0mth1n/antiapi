#!/usr/bin/env python3
import json
import urllib.request
import urllib.error
import sys
import time
from concurrent.futures import ThreadPoolExecutor

BASE_URL = "http://localhost:8964"

def log_test(name, success, detail=""):
    status = "PASS" if success else "FAIL"
    color = "\033[92m" if success else "\033[91m"
    reset = "\033[0m"
    print(f"{color}{status}{reset} | {name} {('- ' + detail) if detail else ''}")
    if not success:
        sys.exit(1)

def test_health_and_status():
    req = urllib.request.Request(f"{BASE_URL}/health")
    with urllib.request.urlopen(req) as resp:
        data = json.loads(resp.read().decode())
        log_test("GET /health", data.get("status") == "ok", str(data))

    req2 = urllib.request.Request(f"{BASE_URL}/api/status")
    with urllib.request.urlopen(req2) as resp:
        data2 = json.loads(resp.read().decode())
        accounts = data2.get("accounts", [])
        total = data2.get("total", 0)
        log_test("GET /api/status", total >= 1 and len(accounts) >= 1, f"Accounts: {accounts}, Quotas: {data2.get('quotas')}")

def test_models_list():
    req = urllib.request.Request(f"{BASE_URL}/v1/models")
    with urllib.request.urlopen(req) as resp:
        data = json.loads(resp.read().decode())
        models = [m["id"] for m in data.get("data", [])]
        expected = ["claude-sonnet-4-6", "gemini-3.7-flash-high", "gemini-3.8-flash-high", "gpt-oss-120b-medium"]
        all_found = all(any(e in m for m in models) for e in expected)
        log_test("GET /v1/models", all_found, f"Found {len(models)} models: {models[:4]}...")

def test_streaming(model_name):
    payload = {
        "model": model_name,
        "messages": [{"role": "user", "content": "Reply with 'PING_OK' in one word."}],
        "stream": True
    }
    req = urllib.request.Request(
        f"{BASE_URL}/v1/chat/completions",
        data=json.dumps(payload).encode(),
        headers={"Content-Type": "application/json"},
        method="POST"
    )
    tokens = []
    done = False
    with urllib.request.urlopen(req, timeout=30) as resp:
        for line in resp:
            l = line.decode().strip()
            if l.startswith("data: ") and l != "data: [DONE]":
                try:
                    c = json.loads(l[6:])
                    delta = c["choices"][0]["delta"].get("content", "")
                    if delta:
                        tokens.append(delta)
                except (json.JSONDecodeError, KeyError, IndexError):
                    pass
            elif l == "data: [DONE]":
                done = True
    full_text = "".join(tokens).strip()
    success = done and len(full_text) > 0
    log_test(f"Stream OpenAI: {model_name}", success, f"Response: {repr(full_text[:40])}")

def test_openai_non_streaming():
    payload = {
        "model": "gemini-3.7-flash-high",
        "messages": [
            {"role": "system", "content": "You are a concise calculator assistant."},
            {"role": "user", "content": "What is 12 + 13? Return only the number."}
        ],
        "stream": False
    }
    req = urllib.request.Request(
        f"{BASE_URL}/v1/chat/completions",
        data=json.dumps(payload).encode(),
        headers={"Content-Type": "application/json"},
        method="POST"
    )
    with urllib.request.urlopen(req, timeout=20) as resp:
        data = json.loads(resp.read().decode())
        content = data["choices"][0]["message"]["content"].strip()
        finish = data["choices"][0]["finish_reason"]
        log_test("Non-stream OpenAI (system + multi-turn)", "25" in content and finish == "stop", f"Content: {content}")

def test_claude_messages_streaming():
    payload = {
        "model": "claude-sonnet-4-6",
        "messages": [{"role": "user", "content": "Reply with 'CLAUDE_OK'"}],
        "stream": True
    }
    req = urllib.request.Request(
        f"{BASE_URL}/v1/messages",
        data=json.dumps(payload).encode(),
        headers={"Content-Type": "application/json"},
        method="POST"
    )
    tokens = []
    with urllib.request.urlopen(req, timeout=30) as resp:
        for line in resp:
            l = line.decode().strip()
            if l.startswith("data: "):
                try:
                    d = json.loads(l[6:])
                    if d.get("type") == "content_block_delta":
                        tokens.append(d["delta"].get("text", ""))
                except (json.JSONDecodeError, KeyError):
                    pass
    full_text = "".join(tokens).strip()
    log_test("Stream Claude (/v1/messages)", len(full_text) > 0, f"Response: {repr(full_text[:40])}")

def test_claude_messages_non_streaming():
    payload = {
        "model": "claude-sonnet-4-6",
        "messages": [{"role": "user", "content": "Say Paris"}],
        "stream": False
    }
    req = urllib.request.Request(
        f"{BASE_URL}/v1/messages",
        data=json.dumps(payload).encode(),
        headers={"Content-Type": "application/json"},
        method="POST"
    )
    with urllib.request.urlopen(req, timeout=20) as resp:
        data = json.loads(resp.read().decode())
        text = data["content"][0]["text"].strip()
        log_test("Non-stream Claude (/v1/messages)", len(text) > 0, f"Text: {text}")

def test_unknown_model_fallback():
    payload = {
        "model": "some-random-unknown-model-xyz",
        "messages": [{"role": "user", "content": "Say fallback"}],
        "stream": False
    }
    req = urllib.request.Request(
        f"{BASE_URL}/v1/chat/completions",
        data=json.dumps(payload).encode(),
        headers={"Content-Type": "application/json"},
        method="POST"
    )
    with urllib.request.urlopen(req, timeout=20) as resp:
        data = json.loads(resp.read().decode())
        content = data["choices"][0]["message"]["content"]
        log_test("Fuzzy / Unknown Model Fallback", len(content) > 0, f"Resolved and answered: {repr(content[:30])}")

def test_concurrency():
    models_to_test = [
        "gemini-3.7-flash-high",
        "gemini-3.8-flash-high",
        "claude-sonnet-4-6",
        "gemini-3.6-flash-medium"
    ]
    def query(m):
        payload = {"model": m, "messages": [{"role": "user", "content": f"Hi from {m}"}], "stream": False}
        req = urllib.request.Request(f"{BASE_URL}/v1/chat/completions", data=json.dumps(payload).encode(), headers={"Content-Type": "application/json"}, method="POST")
        with urllib.request.urlopen(req, timeout=30) as resp:
            return json.loads(resp.read().decode())

    with ThreadPoolExecutor(max_workers=4) as ex:
        results = list(ex.map(query, models_to_test))

    all_ok = all(len(r.get("choices", [{}])[0].get("message", {}).get("content", "")) > 0 for r in results)
    log_test("Concurrent Load Test (4 simultaneous queries)", all_ok, f"All {len(results)} concurrent requests finished successfully")

if __name__ == "__main__":
    print("=" * 65)
    print("  ANTIGRAVITY API GATEWAY COMPREHENSIVE TEST SUITE")
    print("=" * 65)
    test_health_and_status()
    test_models_list()
    
    print("\n--- Testing All Native & Tier Models ---")
    core_models = [
        "claude-sonnet-4-6",
        "claude-opus-4-6-thinking",
        "gemini-3.8-flash-high",
        "gemini-3.7-flash-high",
        "gemini-3.7-flash-medium",
        "gemini-3.6-flash-medium",
        "gemini-3.1-pro-high",
        "gpt-oss-120b-medium"
    ]
    for m in core_models:
        test_streaming(m)
        time.sleep(0.5)

    print("\n--- Testing Protocol Variations & Fallbacks ---")
    test_openai_non_streaming()
    test_claude_messages_streaming()
    test_claude_messages_non_streaming()
    test_unknown_model_fallback()

    print("\n--- Testing Concurrency ---")
    test_concurrency()

    print("\n" + "=" * 65)
    print("  ALL TESTS PASSED PERFECTLY!")
    print("=" * 65)
