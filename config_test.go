package main

import (
	"errors"
	"io/fs"
	"os"
	"slices"
	"strings"
	"testing"
)

// envMap turns a map into the lookup function loadSettings takes, so tests set
// the environment without touching the real process environment.
func envMap(pairs map[string]string) func(string) string {
	return func(key string) string { return pairs[key] }
}

// What it tests: an empty environment.
// Expected: the documented defaults (listen on :8080, PostgreSQL at
// 127.0.0.1:5432 with verified TLS, 32 connections, 8 concurrent HTTP queries,
// metrics on the main listener, no direct TLS).
func TestLoadSettingsDefaults(t *testing.T) {
	s, err := loadSettings(envMap(nil), nil, false)
	if err != nil {
		t.Fatal(err)
	}
	if s.cfg.Listen != ":8080" || s.cfg.PGAddr != "127.0.0.1:5432" || s.cfg.PGSSLMode != "require" {
		t.Fatalf("unexpected defaults: listen=%q pg=%q sslmode=%q", s.cfg.Listen, s.cfg.PGAddr, s.cfg.PGSSLMode)
	}
	if cap(s.cfg.UpstreamSlots) != 32 || cap(s.cfg.HTTPSlots) != 8 || cap(s.cfg.CancelSlots) != 8 {
		t.Fatal("unexpected default slot counts")
	}
	if s.maxClientConnections != 256 || s.metricsMaxConnections != 16 {
		t.Fatal("unexpected listener connection limits")
	}
	if !s.metricsEnabled || s.metricsListen != "" || s.tlsConfig != nil {
		t.Fatal("unexpected metrics or TLS defaults")
	}
}

// What it tests: the four variables a PostgreSQL node's service manager writes
// for Barnacle (loopback listener behind HAProxy, loopback PostgreSQL without
// TLS). Expected: they load and are applied. If a change makes this fail, the
// managed-node setup stops working, so update that setup together with it.
func TestLoadSettingsManagedNodeEnvironment(t *testing.T) {
	s, err := loadSettings(envMap(map[string]string{
		"BARNACLE_LISTEN":          "127.0.0.1:9682",
		"BARNACLE_MAX_CONNECTIONS": "32",
		"BARNACLE_PG_ADDR":         "127.0.0.1:5432",
		"BARNACLE_PG_SSLMODE":      "disable",
	}), nil, false)
	if err != nil {
		t.Fatal(err)
	}
	if s.cfg.Listen != "127.0.0.1:9682" || s.cfg.PGAddr != "127.0.0.1:5432" || s.cfg.PGSSLMode != "disable" {
		t.Fatalf("unexpected config: %+v", s.cfg)
	}
}

// What it tests: several invalid values at once (bad duration, zero
// connections, unknown SSL mode, a boolean spelled "yes", bad metrics value,
// malformed address). Expected: one error that names every variable, so an
// operator fixes them in a single pass instead of one restart per mistake.
func TestLoadSettingsReportsEveryProblem(t *testing.T) {
	_, err := loadSettings(envMap(map[string]string{
		"BARNACLE_QUERY_TIMEOUT":       "soon",
		"BARNACLE_MAX_CONNECTIONS":     "0",
		"BARNACLE_PG_SSLMODE":          "prefer",
		"BARNACLE_WS_ALLOW_TRUST_AUTH": "yes",
		"BARNACLE_METRICS":             "maybe",
		"BARNACLE_PG_ADDR":             "not-an-address",
	}), nil, false)
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

// What it tests: values that are individually valid but wrong in combination or
// at their limits. Expected: each is refused. Cases: more HTTP queries than
// connections; a timeout so large that adding the write margin overflows; a
// memory size that overflows when converted to bytes; a default PostgreSQL
// address missing from the allowlist; an unknown query mode; a certificate
// without its key; a CA file that does not exist.
func TestLoadSettingsRejections(t *testing.T) {
	for name, env := range map[string]map[string]string{
		"http queries above connections": {"BARNACLE_MAX_CONNECTIONS": "4", "BARNACLE_MAX_HTTP_QUERIES": "5"},
		"zero client connections":        {"BARNACLE_MAX_CLIENT_CONNECTIONS": "0"},
		"negative client connections":    {"BARNACLE_MAX_CLIENT_CONNECTIONS": "-1"},
		"invalid metrics connections":    {"BARNACLE_METRICS_MAX_CONNECTIONS": "unlimited"},
		"zero metrics connections":       {"BARNACLE_METRICS_MAX_CONNECTIONS": "0"},
		"timeout overflow":               {"BARNACLE_QUERY_TIMEOUT": "2562047h47m16s"},
		"memory overflow":                {"BARNACLE_HTTP_MAX_MEMORY_MIB": "9223372036854775807"},
		"default outside allowlist":      {"BARNACLE_PG_ADDR": "10.0.0.1:5432", "BARNACLE_PG_ALLOWED_ADDRS": "10.0.0.2:5432"},
		"unknown exec mode":              {"BARNACLE_PG_QUERY_EXEC_MODE": "simple"},
		"half a TLS pair":                {"BARNACLE_TLS_CERT_FILE": "cert.pem"},
		"missing CA file":                {"BARNACLE_PG_CA_FILE": "/nonexistent/ca.pem"},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := loadSettings(envMap(env), nil, false); err == nil {
				t.Fatal("invalid environment accepted")
			}
		})
	}
}

// What it tests: boolean settings are case-insensitive for "true" and "false".
// Expected: "TRUE" and "False" are accepted and applied. Other spellings are
// refused (see the reports-every-problem test), so a typo cannot relax a policy.
func TestLoadSettingsAcceptsBooleanCase(t *testing.T) {
	s, err := loadSettings(envMap(map[string]string{"BARNACLE_WS_ALLOW_TRUST_AUTH": "TRUE", "BARNACLE_METRICS": "False"}), nil, false)
	if err != nil {
		t.Fatal(err)
	}
	if !s.cfg.AllowTrustLogin || s.metricsEnabled {
		t.Fatal("boolean values not applied")
	}
}

func TestLoadSettingsListenerLimits(t *testing.T) {
	s, err := loadSettings(envMap(map[string]string{
		"BARNACLE_MAX_CLIENT_CONNECTIONS":  "64",
		"BARNACLE_METRICS_MAX_CONNECTIONS": "4",
	}), nil, true)
	if err != nil {
		t.Fatal(err)
	}
	if s.maxClientConnections != 64 || s.metricsMaxConnections != 4 {
		t.Fatal("configured listener limits not applied")
	}
}

// What it tests: strict mode with two misspelled BARNACLE_* names next to a
// valid one and an unrelated variable. Expected: without strict, they are
// ignored; with strict, both typos are reported by name, and neither the valid
// setting nor non-BARNACLE variables like PATH are mentioned.
func TestLoadSettingsStrictRejectsUnknownNames(t *testing.T) {
	values := map[string]string{"BARNACLE_PG_SSLMODE": "disable"}
	environ := []string{"PATH=/usr/bin", "BARNACLE_PG_SSLMODE=disable", "BARNACLE_PG_SSLMDOE=disable", "BARNACLE_LISTNE=:1"}

	if _, err := loadSettings(envMap(values), environ, false); err != nil {
		t.Fatalf("lenient mode rejected unknown names: %v", err)
	}
	_, err := loadSettings(envMap(values), environ, true)
	if err == nil {
		t.Fatal("strict mode accepted unknown names")
	}
	for _, want := range []string{"BARNACLE_PG_SSLMDOE", "BARNACLE_LISTNE"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error does not mention %s: %v", want, err)
		}
	}
	if strings.Contains(err.Error(), "BARNACLE_PG_SSLMODE") || strings.Contains(err.Error(), "PATH") {
		t.Errorf("error names a known or foreign variable: %v", err)
	}
}

// What it tests: strict mode with every registered setting present in the
// environment. Expected: no "unknown setting" error, so the registry cannot
// reject a setting the server actually supports.
func TestStrictKnowsEverySetting(t *testing.T) {
	var environ []string
	for _, name := range settingNames {
		environ = append(environ, name+"=")
	}
	if _, err := loadSettings(envMap(nil), environ, true); err != nil {
		t.Fatal(err)
	}
}

// Every registered name must be read, so a removed setting cannot linger in
// the list and be accepted silently by strict mode.
func TestSettingNamesAreAllRead(t *testing.T) {
	read := map[string]bool{}
	_, err := loadSettings(func(key string) string { read[key] = true; return "" }, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range settingNames {
		if !read[name] {
			t.Errorf("%s is registered but never read", name)
		}
	}
	if !slices.IsSorted(settingNames) {
		t.Error("settingNames must stay sorted")
	}
}

// The reference table in docs/deployment.md must list exactly the registered
// settings.
func TestSettingNamesMatchDeploymentDocs(t *testing.T) {
	doc, err := os.ReadFile("docs/deployment.md")
	if errors.Is(err, fs.ErrNotExist) {
		t.Skip("docs are not part of the source archive that packages build from")
	}
	if err != nil {
		t.Fatal(err)
	}
	documented := map[string]bool{}
	for _, line := range strings.Split(string(doc), "\n") {
		if row, ok := strings.CutPrefix(line, "| `BARNACLE_"); ok {
			documented["BARNACLE_"+strings.SplitN(row, "`", 2)[0]] = true
		}
	}
	for _, name := range settingNames {
		if !documented[name] {
			t.Errorf("%s is not in the docs/deployment.md settings table", name)
		}
		delete(documented, name)
	}
	for name := range documented {
		t.Errorf("docs/deployment.md documents %s, which the server does not read", name)
	}
}
