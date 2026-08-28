// Command metrics-agent is a Linux host telemetry agent.
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"
	"time"

	"metrics-agent/internal/agent"
	"metrics-agent/internal/buffer"
	"metrics-agent/internal/config"
	"metrics-agent/internal/logging"
	"metrics-agent/internal/metrics"
	"metrics-agent/internal/transport"
)

// Filled in via -ldflags at build time.
var version = "dev"

// userAgent goes into the User-Agent header of the request (G3).
func userAgent() string {
	return "metrics-agent/" + version
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()

	if err := run(ctx, os.Args[1:], os.Stdout, os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

// run is the command body without os.Exit and global streams, so tests can see it.
func run(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("metrics-agent", flag.ContinueOnError)
	fs.SetOutput(stderr)
	configPath := fs.String("config", config.DefaultPath, "path to the configuration file")
	showVersion := fs.Bool("version", false, "print version and exit")
	if err := fs.Parse(args); err != nil {
		return err
	}

	if *showVersion {
		fmt.Fprintln(stdout, version)
		return nil
	}

	cfg, err := config.Load(*configPath)
	if err != nil {
		return fmt.Errorf("configuration error: %w", err)
	}

	log := logging.New(cfg.LogLevel, stdout)
	if cfg.WorldReadable {
		log.Warn("configuration file is world-readable", "path", *configPath)
	}
	log.Info("agent is starting", "version", version, "config", cfg)

	collector := metrics.New(log)
	host := collector.Host()
	log.Info("machine characteristics", "host", host)

	buf, err := buffer.Open(log, cfg.BufferPath)
	if err != nil {
		return fmt.Errorf("buffer error: %w", err)
	}

	client := transport.NewClient(log, cfg.Endpoint, cfg.Token, userAgent())

	// The loop and the reporter know about each other: the server response changes
	// the tick interval. The reporter needs a ready loop, so the tick is set last.
	loop := agent.New(log, cfg.Token, agent.DefaultReportInterval, nil)
	reporter := agent.NewReporter(log, client, loop, buf, host, version)
	loop.SetTick(func(ctx context.Context) {
		sample := collector.Collect(ctx)
		if sample == nil {
			log.Debug("sample skipped", "at", time.Now().Format(time.RFC3339))
			return
		}
		log.Debug("sample collected", "ts", sample.TS, "fs", len(sample.FS))
		reporter.Report(ctx, sample)
	})

	if err := loop.Run(ctx); err != nil {
		return fmt.Errorf("loop finished with an error: %w", err)
	}

	// The buffer is written to disk immediately, there is nothing to flush on exit.
	log.Info("agent stopped")
	return nil
}
