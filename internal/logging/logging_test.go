package logging_test

import (
	"bytes"
	"strings"
	"testing"

	"metrics-agent/internal/config"
	"metrics-agent/internal/logging"
)

const token = "9f2c1f8a-1111-4222-8333-444455556666"

func TestTokenNeverLogged(t *testing.T) {
	cfg := config.Config{
		Endpoint:   "https://example.test/api/agent/v1/metrics",
		Token:      token,
		LogLevel:   "debug",
		BufferPath: "/var/lib/metrics-agent/buffer",
	}
	for level := range logging.Levels {
		var buf bytes.Buffer
		log := logging.New(level, &buf)
		log.Debug("start", "config", cfg)
		log.Info("start", "config", cfg)
		log.Warn("start", "config", cfg)
		log.Error("start", "config", cfg)
		if strings.Contains(buf.String(), token) {
			t.Fatalf("token leaked into the log at level %s: %s", level, buf.String())
		}
	}
}

func TestLevelNoneSilent(t *testing.T) {
	var buf bytes.Buffer
	log := logging.New("none", &buf)
	log.Error("should be silent")
	if buf.Len() != 0 {
		t.Fatalf("log_level: none writes to output: %s", buf.String())
	}
}

func TestLevelFiltering(t *testing.T) {
	var buf bytes.Buffer
	log := logging.New("warn", &buf)
	log.Info("should not pass")
	log.Warn("should pass")
	out := buf.String()
	if strings.Contains(out, "should not pass") || !strings.Contains(out, "should pass") {
		t.Fatalf("level filtering is broken: %s", out)
	}
}
