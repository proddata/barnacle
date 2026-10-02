package main

import (
	"compress/gzip"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestGzipSQLCompressesLargeResponses(t *testing.T) {
	handler := gzipSQL(func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"payload": strings.Repeat("x", 8192)})
	})
	request := httptest.NewRequest(http.MethodPost, "/sql", nil)
	request.Header.Set("Accept-Encoding", "gzip")
	response := httptest.NewRecorder()
	handler(response, request)
	if response.Code != http.StatusOK || response.Header().Get("Content-Encoding") != "gzip" {
		t.Fatalf("status %d, encoding %q", response.Code, response.Header().Get("Content-Encoding"))
	}
	reader, err := gzip.NewReader(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := io.ReadAll(reader)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(decoded), strings.Repeat("x", 8192)) {
		t.Fatal("compressed response did not round trip")
	}
}

func TestGzipSQLLeavesSmallResponsesPlain(t *testing.T) {
	handler := gzipSQL(func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"message": "small error"})
	})
	for _, accept := range []string{"gzip", "gzip;q=0"} {
		request := httptest.NewRequest(http.MethodPost, "/sql", nil)
		request.Header.Set("Accept-Encoding", accept)
		response := httptest.NewRecorder()
		handler(response, request)
		if response.Code != http.StatusBadRequest || response.Header().Get("Content-Encoding") != "" {
			t.Fatalf("accept %q: status %d, encoding %q", accept, response.Code, response.Header().Get("Content-Encoding"))
		}
		if !strings.Contains(response.Body.String(), "small error") {
			t.Fatalf("accept %q: body %q", accept, response.Body.String())
		}
	}
}
