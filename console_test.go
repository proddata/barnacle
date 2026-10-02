package main

import (
	"net/http/httptest"
	"strings"
	"testing"
)

func TestConsoleOffersBothTransports(t *testing.T) {
	response := httptest.NewRecorder()
	console(response, httptest.NewRequest("GET", "http://localhost/", nil))
	body := response.Body.String()
	for _, part := range []string{`value="http">Run via HTTP`, `value="ws">Run via WebSocket`, `src="/console.mjs"`} {
		if !strings.Contains(body, part) {
			t.Fatalf("console missing %q", part)
		}
	}
}
func TestConsoleScriptServed(t *testing.T) {
	response := httptest.NewRecorder()
	consoleScript(response, httptest.NewRequest("GET", "http://localhost/console.mjs", nil))
	if response.Code != 200 || !strings.Contains(response.Body.String(), "runWebSocketQuery") {
		t.Fatalf("console module was not served: status %d", response.Code)
	}
}
