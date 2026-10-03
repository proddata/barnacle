package gateway

import "testing"

func TestPostgreSQLRouting(t *testing.T) {
	allowed, err := ParseAllowedPGAddrs("db.internal:5432, [::1]:6543")
	if err != nil {
		t.Fatal(err)
	}
	cfg := Config{PGAllowedAddrs: allowed, PGSSLMode: "require"}
	for _, test := range []struct {
		name, connectionString, want string
	}{
		{"host", "postgres://app:secret@DB.INTERNAL/app", "db.internal:5432"},
		{"IPv6", "postgres://app:secret@[::1]:6543/app", "[::1]:6543"},
		{"disallowed", "postgres://app:secret@other.internal/app", ""},
		{"different port", "postgres://app:secret@db.internal:6543/app", ""},
		{"missing host", "postgres:///app", ""},
	} {
		t.Run(test.name, func(t *testing.T) {
			addr, err := cfg.HTTPUpstreamAddr(test.connectionString)
			if test.want == "" {
				if err == nil {
					t.Fatalf("accepted %q as %q", test.connectionString, addr)
				}
				return
			}
			if err != nil || addr != test.want {
				t.Fatalf("address = %q, %v; want %q", addr, err, test.want)
			}
		})
	}
	for _, test := range []struct {
		address, want string
	}{
		{"db.internal:5432", "db.internal:5432"},
		{"[::1]:6543", "[::1]:6543"},
		{"db.internal:6543", ""},
		{"127.0.0.1:5432", ""},
		{"", ""},
	} {
		got, err := cfg.UpstreamAddr(test.address)
		if test.want == "" && err == nil || test.want != "" && (err != nil || got != test.want) {
			t.Fatalf("WebSocket address %q = %q, %v; want %q", test.address, got, err, test.want)
		}
	}
}

func TestPostgreSQLRoutingFallbackAndFixedMode(t *testing.T) {
	allowed, err := ParseAllowedPGAddrs("db.internal:5432")
	if err != nil {
		t.Fatal(err)
	}
	for _, cfg := range []Config{
		{PGAddr: "default.internal:5432", PGAllowedAddrs: allowed},
		{PGAddr: "default.internal:5432"},
	} {
		addr, err := cfg.UpstreamAddr("")
		if err != nil || addr != "default.internal:5432" {
			t.Fatalf("fallback = %q, %v", addr, err)
		}
	}
	fixed := Config{PGAddr: "default.internal:5432"}
	addr, err := fixed.UpstreamAddr("malicious.internal:5432")
	if err != nil || addr != fixed.PGAddr {
		t.Fatalf("fixed routing = %q, %v", addr, err)
	}
	for _, value := range []string{"", "db.internal", "db.internal:0", "db.internal:65536", "db.internal:5432,localhost:5432", "http://db.internal:5432", "user@db.internal:5432"} {
		if value == "" {
			continue
		}
		if _, err := CanonicalPGAddr(value); err == nil {
			t.Fatalf("accepted invalid address %q", value)
		}
	}
}
