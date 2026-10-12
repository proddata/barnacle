package main

import (
	"strings"
	"testing"
)

func envMap(pairs map[string]string) func(string) string {
	return func(key string) string { return pairs[key] }
}

func TestLoadSettingsDefaults(t *testing.T) {
	s, err := loadSettings(envMap(nil))
	if err != nil {
		t.Fatal(err)
	}
	if s.cfg.Listen != ":8080" || s.cfg.PGAddr != "127.0.0.1:5432" || s.cfg.PGSSLMode != "require" {
		t.Fatalf("unexpected defaults: listen=%q pg=%q sslmode=%q", s.cfg.Listen, s.cfg.PGAddr, s.cfg.PGSSLMode)
	}
	if cap(s.cfg.UpstreamSlots) != 32 || cap(s.cfg.HTTPSlots) != 8 || cap(s.cfg.CancelSlots) != 8 {
		t.Fatal("unexpected default slot counts")
	}
	if !s.metricsEnabled || s.metricsListen != "" || s.tlsConfig != nil {
		t.Fatal("unexpected metrics or TLS defaults")
	}
}

// The environment a PostgreSQL node's service manager writes for Barnacle:
// loopback listener behind HAProxy, loopback PostgreSQL without TLS.
func TestLoadSettingsManagedNodeEnvironment(t *testing.T) {
	s, err := loadSettings(envMap(map[string]string{
		"BARNACLE_LISTEN":          "127.0.0.1:9682",
		"BARNACLE_MAX_CONNECTIONS": "32",
		"BARNACLE_PG_ADDR":         "127.0.0.1:5432",
		"BARNACLE_PG_SSLMODE":      "disable",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if s.cfg.Listen != "127.0.0.1:9682" || s.cfg.PGAddr != "127.0.0.1:5432" || s.cfg.PGSSLMode != "disable" {
		t.Fatalf("unexpected config: %+v", s.cfg)
	}
}

func TestLoadSettingsReportsEveryProblem(t *testing.T) {
	_, err := loadSettings(envMap(map[string]string{
		"BARNACLE_QUERY_TIMEOUT":       "soon",
		"BARNACLE_MAX_CONNECTIONS":     "0",
		"BARNACLE_PG_SSLMODE":          "prefer",
		"BARNACLE_WS_ALLOW_TRUST_AUTH": "yes",
		"BARNACLE_METRICS":             "maybe",
		"BARNACLE_PG_ADDR":             "not-an-address",
	}))
	if err == nil {
		t.Fatal("invalid environment accepted")
	}
	for _, want := range []string{
		"BARNACLE_QUERY_TIMEOUT", "BARNACLE_MAX_CONNECTIONS", "BARNACLE_PG_SSLMODE",
		"BARNACLE_WS_ALLOW_TRUST_AUTH", "BARNACLE_METRICS", "BARNACLE_PG_ADDR",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error does not mention %s: %v", want, err)
		}
	}
}

func TestLoadSettingsRejections(t *testing.T) {
	for name, env := range map[string]map[string]string{
		"http queries above connections": {"BARNACLE_MAX_CONNECTIONS": "4", "BARNACLE_MAX_HTTP_QUERIES": "5"},
		"timeout overflow":               {"BARNACLE_QUERY_TIMEOUT": "2562047h47m16s"},
		"memory overflow":                {"BARNACLE_HTTP_MAX_MEMORY_MIB": "9223372036854775807"},
		"default outside allowlist":      {"BARNACLE_PG_ADDR": "10.0.0.1:5432", "BARNACLE_PG_ALLOWED_ADDRS": "10.0.0.2:5432"},
		"unknown exec mode":              {"BARNACLE_PG_QUERY_EXEC_MODE": "simple"},
		"half a TLS pair":                {"BARNACLE_TLS_CERT_FILE": "cert.pem"},
		"missing CA file":                {"BARNACLE_PG_CA_FILE": "/nonexistent/ca.pem"},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := loadSettings(envMap(env)); err == nil {
				t.Fatal("invalid environment accepted")
			}
		})
	}
}

func TestLoadSettingsAcceptsBooleanCase(t *testing.T) {
	s, err := loadSettings(envMap(map[string]string{"BARNACLE_WS_ALLOW_TRUST_AUTH": "TRUE", "BARNACLE_METRICS": "False"}))
	if err != nil {
		t.Fatal(err)
	}
	if !s.cfg.AllowTrustLogin || s.metricsEnabled {
		t.Fatal("boolean values not applied")
	}
}
