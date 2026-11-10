package server

import (
	"bytes"
	"compress/gzip"
	"io"
	"net/http"
	"strings"

	"github.com/klauspost/compress/zstd"
)

// maxDecompressedBody caps JSON request bodies after Content-Encoding
// decompression so a small compressed bomb cannot exhaust memory. 32 MiB is
// far above any real client payload (tool-heavy prompts top out near 1 MiB).
const maxDecompressedBody = 32 << 20

// decompress transparently inflates request bodies sent with
// Content-Encoding: gzip or zstd (Codex CLI compresses /v1/responses with
// zstd). Requests without a Content-Encoding pass through untouched.
func (s *Server) decompress(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		enc := strings.ToLower(strings.TrimSpace(r.Header.Get("Content-Encoding")))
		switch enc {
		case "", "identity":
			next(w, r)
			return
		case "gzip", "zstd":
		default:
			http.Error(w, "Unsupported Content-Encoding: "+enc, http.StatusUnsupportedMediaType)
			return
		}

		var rc io.ReadCloser
		var err error
		if enc == "gzip" {
			rc, err = gzip.NewReader(r.Body)
		} else {
			var zr *zstd.Decoder
			zr, err = zstd.NewReader(r.Body)
			rc = zr.IOReadCloser()
		}
		if err != nil {
			http.Error(w, "Invalid "+enc+" request body", http.StatusBadRequest)
			return
		}
		defer rc.Close()

		data, err := io.ReadAll(io.LimitReader(rc, maxDecompressedBody+1))
		if err != nil {
			http.Error(w, "Invalid "+enc+" request body", http.StatusBadRequest)
			return
		}
		if len(data) > maxDecompressedBody {
			http.Error(w, "Request body too large", http.StatusRequestEntityTooLarge)
			return
		}

		r.Body = io.NopCloser(bytes.NewReader(data))
		r.ContentLength = int64(len(data))
		r.Header.Del("Content-Encoding")
		next(w, r)
	}
}
