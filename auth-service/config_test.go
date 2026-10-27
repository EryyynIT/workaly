package main

import (
	"strings"
	"testing"
)

func TestConfigValidation(t *testing.T) {
	for _, tc := range []struct {
		name string
		env  map[string]string
		ok   bool
	}{
		{"missing secret", nil, false},
		{"short secret", map[string]string{"JWT_SECRET": "short"}, false},
		{"defaults", map[string]string{"JWT_SECRET": strings.Repeat("s", 32), "AUTH_DATABASE_URL": "postgres://u:p@localhost/db"}, true},
		{"invalid boolean", map[string]string{"JWT_SECRET": strings.Repeat("s", 32), "COOKIE_SECURE": "maybe"}, false},
		{"invalid origin", map[string]string{"JWT_SECRET": strings.Repeat("s", 32), "PUBLIC_ORIGIN": "https://example.org/path"}, false},
		{"missing database", map[string]string{"JWT_SECRET": strings.Repeat("s", 32)}, false},
		{"local", map[string]string{"JWT_SECRET": strings.Repeat("s", 32), "COOKIE_SECURE": "false", "HTTP_ADDR": "127.0.0.1:9000", "USER_SERVICE_ADDR": "user:50051", "AUTH_DATABASE_URL": "postgres://u:p@localhost/db"}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg, err := loadConfig(func(key string) string { return tc.env[key] })
			if (err == nil) != tc.ok {
				t.Fatalf("loadConfig error = %v; want success %v", err, tc.ok)
			}
			if tc.name == "defaults" && (!cfg.CookieSecure || cfg.HTTPAddr != "127.0.0.1:8080") {
				t.Fatalf("unsafe or incorrect defaults: %+v", cfg)
			}
			if tc.name == "local" && (cfg.CookieSecure || cfg.HTTPAddr != "127.0.0.1:9000" || cfg.UserServiceAddr != "user:50051") {
				t.Fatalf("overrides ignored: %+v", cfg)
			}
		})
	}
}
