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
		{PGAddr: "db.internal:5432", PGAllowedAddrs: allowed},
		{PGAddr: "default.internal:5432"},
	} {
		addr, err := cfg.UpstreamAddr("")
		if err != nil || addr != cfg.PGAddr {
			t.Fatalf("fallback = %q, %v", addr, err)
		}
	}
	if addr, err := (Config{PGAddr: "default.internal:5432", PGAllowedAddrs: allowed}).UpstreamAddr(""); err == nil {
		t.Fatalf("accepted unlisted fallback %q", addr)
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

func TestPostgreSQLRoutingWildcard(t *testing.T) {
	allowed, err := ParseAllowedPGAddrs("*")
	if err != nil {
		t.Fatal(err)
	}
	cfg := Config{PGAddr: "postgres:5432", PGAllowedAddrs: allowed}
	for _, tc := range []struct{ requested, want string }{
		{"", "postgres:5432"},
		{"external.example:26432", "external.example:26432"},
		{"EXTERNAL.EXAMPLE:26432", "external.example:26432"},
	} {
		got, err := cfg.UpstreamAddr(tc.requested)
		if err != nil || got != tc.want {
			t.Fatalf("UpstreamAddr(%q) = %q, %v; want %q", tc.requested, got, err, tc.want)
		}
	}
	got, err := cfg.HTTPUpstreamAddr("postgres://user:password@external.example:26432/db")
	if err != nil || got != "external.example:26432" {
		t.Fatalf("HTTPUpstreamAddr = %q, %v", got, err)
	}
	for _, invalid := range []string{"*", "external.example", "external.example:0"} {
		if _, err := cfg.UpstreamAddr(invalid); err == nil {
			t.Fatalf("accepted invalid address %q", invalid)
		}
	}
	if _, err := ParseAllowedPGAddrs("postgres:5432,*"); err == nil {
		t.Fatal("accepted wildcard mixed with an address")
	}
}
