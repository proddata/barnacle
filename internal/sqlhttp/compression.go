package sqlhttp

import (
	"bytes"
	"compress/gzip"
	"net/http"
	"strconv"
	"strings"
)

const gzipThreshold = 1024

func acceptsGzip(header string) bool {
	for _, part := range strings.Split(header, ",") {
		parts := strings.Split(part, ";")
		if !strings.EqualFold(strings.TrimSpace(parts[0]), "gzip") {
			continue
		}
		for _, option := range parts[1:] {
			name, value, ok := strings.Cut(strings.TrimSpace(option), "=")
			if ok && strings.EqualFold(strings.TrimSpace(name), "q") {
				q, err := strconv.ParseFloat(strings.TrimSpace(value), 64)
				return err == nil && q > 0 && q <= 1
			}
		}
		return true
	}
	return false
}

type gzipResponseWriter struct {
	http.ResponseWriter
	buffer bytes.Buffer
	gzip   *gzip.Writer
	status int
}

func (w *gzipResponseWriter) WriteHeader(status int) {
	if w.status == 0 {
		w.status = status
	}
}

func (w *gzipResponseWriter) Write(p []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	if w.gzip != nil {
		return w.gzip.Write(p)
	}
	if w.buffer.Len()+len(p) < gzipThreshold {
		return w.buffer.Write(p)
	}
	w.Header().Set("Content-Encoding", "gzip")
	w.Header().Del("Content-Length")
	w.ResponseWriter.WriteHeader(w.status)
	w.gzip = gzip.NewWriter(w.ResponseWriter)
	if _, err := w.gzip.Write(w.buffer.Bytes()); err != nil {
		return 0, err
	}
	w.buffer.Reset()
	return w.gzip.Write(p)
}

func (w *gzipResponseWriter) close() error {
	if w.gzip != nil {
		return w.gzip.Close()
	}
	if w.status == 0 {
		w.status = http.StatusOK
	}
	w.ResponseWriter.WriteHeader(w.status)
	_, err := w.ResponseWriter.Write(w.buffer.Bytes())
	return err
}

func (w *gzipResponseWriter) Flush() {
	if w.gzip == nil {
		return
	}
	if err := w.gzip.Flush(); err != nil {
		return
	}
	if flusher, ok := w.ResponseWriter.(http.Flusher); ok {
		flusher.Flush()
	}
}

func Gzip(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Add("Vary", "Accept-Encoding")
		if !acceptsGzip(r.Header.Get("Accept-Encoding")) {
			next(w, r)
			return
		}
		compressed := &gzipResponseWriter{ResponseWriter: w}
		next(compressed, r)
		_ = compressed.close()
	}
}
