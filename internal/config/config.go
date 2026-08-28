// Package config reads and validates the agent configuration.
package config

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"

	"metrics-agent/internal/logging"
)

// DefaultPath is the config path unless overridden by the --config flag.
const DefaultPath = "/etc/metrics-agent/config.yaml"

// TokenEnv is the environment variable the token can be taken from instead of the config.
const TokenEnv = "METRICS_AGENT_TOKEN"

// Config is the agent configuration.
type Config struct {
	Endpoint   string `yaml:"endpoint"`
	Token      string `yaml:"token"`
	LogLevel   string `yaml:"log_level"`
	BufferPath string `yaml:"buffer_path"`

	// WorldReadable is set on load if the config file is readable by everyone.
	WorldReadable bool `yaml:"-"`
}

// LogValue implements slog.LogValuer: the token never reaches the log.
func (c Config) LogValue() slog.Value {
	return slog.GroupValue(
		slog.String("endpoint", c.Endpoint),
		slog.String("log_level", c.LogLevel),
		slog.String("buffer_path", c.BufferPath),
	)
}

var uuidRe = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// Load reads the config from disk, applies the environment variable and validates the result.
func Load(path string) (*Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("failed to read config %s: %w", path, err)
	}

	cfg := &Config{LogLevel: "info"}
	if err := yaml.Unmarshal(raw, cfg); err != nil {
		return nil, fmt.Errorf("failed to parse config %s: %w", path, err)
	}

	if env := os.Getenv(TokenEnv); env != "" {
		cfg.Token = env
	}

	if st, err := os.Stat(path); err == nil && st.Mode().Perm()&0o004 != 0 {
		cfg.WorldReadable = true
	}

	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return cfg, nil
}

// Validate checks the required fields.
func (c *Config) Validate() error {
	c.Token = strings.TrimSpace(c.Token)
	if c.Token == "" {
		return errors.New("token is not set: specify token in the config or " + TokenEnv)
	}
	if !uuidRe.MatchString(c.Token) {
		return errors.New("token is not a UUID")
	}
	if strings.TrimSpace(c.Endpoint) == "" {
		return errors.New("endpoint is not set")
	}
	if strings.TrimSpace(c.BufferPath) == "" {
		return errors.New("buffer_path is not set")
	}
	if _, ok := logging.Levels[c.LogLevel]; !ok {
		return fmt.Errorf("unknown log_level: %q", c.LogLevel)
	}
	return nil
}
