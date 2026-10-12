package main

import (
	"os"
	"slices"
	"strings"
	"testing"
)

func envMap(pairs map[string]string) func(string) string {
	return func(key string) string { return pairs[key] }
}

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
	}), nil, false)
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
			if _, err := loadSettings(envMap(env), nil, false); err == nil {
				t.Fatal("invalid environment accepted")
			}
		})
	}
}

func TestLoadSettingsAcceptsBooleanCase(t *testing.T) {
	s, err := loadSettings(envMap(map[string]string{"BARNACLE_WS_ALLOW_TRUST_AUTH": "TRUE", "BARNACLE_METRICS": "False"}), nil, false)
	if err != nil {
		t.Fatal(err)
	}
	if !s.cfg.AllowTrustLogin || s.metricsEnabled {
		t.Fatal("boolean values not applied")
	}
}

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
