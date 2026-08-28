package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestVersionStrings(t *testing.T) {
	if got := userAgent(); !strings.HasPrefix(got, "metrics-agent/") {
		t.Fatalf("userAgent() = %q", got)
	}
}

func TestRunVersionFlag(t *testing.T) {
	var out, errOut bytes.Buffer
	if err := run(context.Background(), []string{"--version"}, &out, &errOut); err != nil {
		t.Fatalf("run: %v", err)
	}
	if strings.TrimSpace(out.String()) != version {
		t.Fatalf("output = %q, want %q", out.String(), version)
	}
}

func TestRunUnknownFlag(t *testing.T) {
	var out, errOut bytes.Buffer
	if err := run(context.Background(), []string{"--nope"}, &out, &errOut); err == nil {
		t.Fatal("want a flag parsing error")
	}
}

func TestRunMissingConfig(t *testing.T) {
	var out, errOut bytes.Buffer
	path := filepath.Join(t.TempDir(), "missing.yaml")
	err := run(context.Background(), []string{"--config", path}, &out, &errOut)
	if err == nil || !strings.Contains(err.Error(), "configuration error") {
		t.Fatalf("err = %v, want a configuration error", err)
	}
}

func TestRunInvalidConfig(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	// The token is not a UUID: the config is read but fails validation.
	body := "endpoint: https://example.invalid/report\ntoken: not-a-uuid\nbuffer_path: " + dir + "/buffer\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	var out, errOut bytes.Buffer
	err := run(context.Background(), []string{"--config", path}, &out, &errOut)
	if err == nil || !strings.Contains(err.Error(), "configuration error") {
		t.Fatalf("err = %v, want a configuration error", err)
	}
}

// TestRunStopsOnCancelledContext checks the assembly of the whole chain: config,
// buffer, collector, loop. A cancelled context ends the loop on the very first tick.
func TestRunStopsOnCancelledContext(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	body := "endpoint: https://example.invalid/report\n" +
		"token: 11111111-2222-3333-4444-555555555555\n" +
		"log_level: error\n" +
		"buffer_path: " + filepath.Join(dir, "buffer") + "\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	var out, errOut bytes.Buffer
	if err := run(ctx, []string{"--config", path}, &out, &errOut); err != nil {
		t.Fatalf("run: %v", err)
	}
}
