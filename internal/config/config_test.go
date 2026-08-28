package config_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"metrics-agent/internal/config"
)

const validYAML = `endpoint: https://example.test/api/agent/v1/metrics
token: 9f2c1f8a-1111-4222-8333-444455556666
log_level: info
buffer_path: /var/lib/metrics-agent/buffer
`

func write(t *testing.T, body string, perm os.FileMode) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(body), perm); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestLoadValid(t *testing.T) {
	cfg, err := config.Load(write(t, validYAML, 0o600))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Token != "9f2c1f8a-1111-4222-8333-444455556666" {
		t.Errorf("token: %q", cfg.Token)
	}
	if cfg.WorldReadable {
		t.Error("a config with mode 0600 must not be treated as world-readable")
	}
}

func TestLoadWorldReadable(t *testing.T) {
	cfg, err := config.Load(write(t, validYAML, 0o644))
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.WorldReadable {
		t.Error("want a warning about mode 0644")
	}
}

func TestLoadBadToken(t *testing.T) {
	cases := map[string]string{
		"missing":  strings.Replace(validYAML, "token: 9f2c1f8a-1111-4222-8333-444455556666\n", "", 1),
		"empty":    strings.Replace(validYAML, "9f2c1f8a-1111-4222-8333-444455556666", `""`, 1),
		"not UUID": strings.Replace(validYAML, "9f2c1f8a-1111-4222-8333-444455556666", "not-a-uuid", 1),
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := config.Load(write(t, body, 0o600)); err == nil {
				t.Fatal("want a validation error")
			}
		})
	}
}

func TestTokenFromEnv(t *testing.T) {
	t.Setenv(config.TokenEnv, "aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee")
	body := strings.Replace(validYAML, "9f2c1f8a-1111-4222-8333-444455556666", "not-a-uuid", 1)
	cfg, err := config.Load(write(t, body, 0o600))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Token != "aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee" {
		t.Errorf("the environment variable did not override the config: %q", cfg.Token)
	}
}

func TestBadLogLevel(t *testing.T) {
	body := strings.Replace(validYAML, "log_level: info", "log_level: loud", 1)
	if _, err := config.Load(write(t, body, 0o600)); err == nil {
		t.Fatal("want an error for an unknown log_level")
	}
}

func TestMissingFile(t *testing.T) {
	if _, err := config.Load(filepath.Join(t.TempDir(), "missing.yaml")); err == nil {
		t.Fatal("want an error for a missing file")
	}
}
