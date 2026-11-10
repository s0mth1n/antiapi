package server

import (
	"bytes"
	"compress/gzip"
	"encoding/json"

	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/klauspost/compress/zstd"

	"antiapi/translator"
)

// jsonEcho decodes the request body as JSON and writes it back, so tests can
// verify the body a handler actually sees after middleware.
func jsonEcho(w http.ResponseWriter, r *http.Request) {
	var v map[string]any
	if err := json.NewDecoder(r.Body).Decode(&v); err != nil {
		http.Error(w, "bad json: "+err.Error(), http.StatusBadRequest)
		return
	}
	json.NewEncoder(w).Encode(v)
}

func gzipBody(t *testing.T, data []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	gw := gzip.NewWriter(&buf)
	if _, err := gw.Write(data); err != nil {
		t.Fatal(err)
	}
	if err := gw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func zstdBody(t *testing.T, data []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw, err := zstd.NewWriter(&buf)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := zw.Write(data); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestDecompressGzip(t *testing.T) {
	s := &Server{}
	payload := []byte(`{"model":"gpt-5","input":"hello"}`)
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(gzipBody(t, payload)))
	req.Header.Set("Content-Encoding", "gzip")
	rec := httptest.NewRecorder()

	s.decompress(jsonEcho)(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	if !json.Valid(rec.Body.Bytes()) || !strings.Contains(rec.Body.String(), `"hello"`) {
		t.Fatalf("handler did not see decompressed body: %s", rec.Body.String())
	}
}

func TestDecompressZstd(t *testing.T) {
	s := &Server{}
	payload := []byte(`{"model":"gpt-5","input":"hello from codex"}`)
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(zstdBody(t, payload)))
	req.Header.Set("Content-Encoding", "zstd")
	rec := httptest.NewRecorder()

	s.decompress(jsonEcho)(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "hello from codex") {
		t.Fatalf("handler did not see decompressed body: %s", rec.Body.String())
	}
}

func TestDecompressPassthrough(t *testing.T) {
	s := &Server{}
	payload := []byte(`{"plain":true}`)
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(payload))
	rec := httptest.NewRecorder()

	s.decompress(jsonEcho)(rec, req)

	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"plain":true`) {
		t.Fatalf("identity body should pass through, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestDecompressUnsupportedEncoding(t *testing.T) {
	s := &Server{}
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader("{}"))
	req.Header.Set("Content-Encoding", "br")
	rec := httptest.NewRecorder()

	s.decompress(jsonEcho)(rec, req)

	if rec.Code != http.StatusUnsupportedMediaType {
		t.Fatalf("expected 415, got %d", rec.Code)
	}
}

func TestDecompressCap(t *testing.T) {
	s := &Server{}
	// Highly compressible payload that decompresses past the cap.
	big := bytes.Repeat([]byte("a"), maxDecompressedBody+1)
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(gzipBody(t, big)))
	req.Header.Set("Content-Encoding", "gzip")
	rec := httptest.NewRecorder()

	s.decompress(jsonEcho)(rec, req)

	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("expected 413, got %d", rec.Code)
	}
}

func TestFastModeEmptyBodyToggles(t *testing.T) {
	s := &Server{}
	defer translator.SetGlobalFastMode(translator.IsGlobalFastMode())

	translator.SetGlobalFastMode(false)
	post := func(body string) map[string]any {
		req := httptest.NewRequest(http.MethodPost, "/api/fast_mode", strings.NewReader(body))
		rec := httptest.NewRecorder()
		s.handleFastMode(rec, req)
		var out map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatalf("bad response: %v", err)
		}
		return out
	}

	if got := post("{}"); got["fast_mode"] != true {
		t.Fatalf("empty object should toggle on, got %v", got)
	}
	if got := post("{}"); got["fast_mode"] != false {
		t.Fatalf("second empty object should toggle off, got %v", got)
	}
	if got := post(`{"toggle":true}`); got["fast_mode"] != true {
		t.Fatalf("toggle:true should flip on, got %v", got)
	}
	if got := post(`{"fast_mode":false}`); got["fast_mode"] != false {
		t.Fatalf("explicit fast_mode=false should win, got %v", got)
	}
	if got := post(`{"enabled":true}`); got["fast_mode"] != true {
		t.Fatalf("enabled=true should set, got %v", got)
	}
}

func TestDecompressMiddlewareOnMux(t *testing.T) {
	s := NewServer()
	// /api/fast_mode is wrapped with decompress on the real mux; a gzipped
	// toggle must reach the handler and flip the state.
	defer translator.SetGlobalFastMode(translator.IsGlobalFastMode())
	translator.SetGlobalFastMode(false)

	req := httptest.NewRequest(http.MethodPost, "/api/fast_mode", bytes.NewReader(gzipBody(t, []byte(`{}`))))
	req.Header.Set("Content-Encoding", "gzip")
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	if !translator.IsGlobalFastMode() {
		t.Fatal("gzipped toggle did not reach handleFastMode")
	}
}
