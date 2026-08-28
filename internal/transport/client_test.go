package transport_test

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"metrics-agent/internal/logging"
	"metrics-agent/internal/metrics"
	"metrics-agent/internal/transport"
)

const token = "9f2c1f8a-1111-4222-8333-444455556666"

func quiet() *slog.Logger { return logging.New("none", io.Discard) }

// sample returns a measurement fully populated per the contract.
func sample() *metrics.Sample {
	return &metrics.Sample{
		TS:     1755000000,
		Uptime: 864000,
		CPU:    metrics.CPU{Cores: 8, User: 12.4, System: 3.1, IOWait: 0.4, Idle: 84.1},
		LA:     [3]float64{0.8, 1.2, 1.1},
		Mem:    metrics.Mem{Total: 16777216000, Free: 1048576000, Available: 4194304000},
		Swap:   metrics.Swap{Total: 2147483648, Used: 104857600},
		FS: []metrics.Filesystem{
			{Mount: "/", FSType: "ext4", Total: 100, Used: 50, InodesTotal: 10, InodesUsed: 5},
		},
	}
}

func request(s ...*metrics.Sample) *transport.Request {
	return &transport.Request{
		AgentVersion: "metrics-agent 1.0.0 (abc1234)",
		Hostname:     "web-01",
		OS:           "Ubuntu 22.04",
		BootID:       "9f2c1f8a-0000-4000-8000-000000000000",
		Samples:      s,
	}
}

// capture starts a stub that remembers the last request.
type capture struct {
	headers http.Header
	body    []byte
	calls   int
}

func serve(t *testing.T, handler func(w http.ResponseWriter, r *http.Request)) (*transport.Client, *capture) {
	t.Helper()
	c := &capture{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c.calls++
		c.headers = r.Header.Clone()
		c.body, _ = io.ReadAll(r.Body)
		handler(w, r)
	}))
	t.Cleanup(srv.Close)
	return transport.NewClient(quiet(), srv.URL, token, "metrics-agent/1.0.0"), c
}

func ok(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_, _ = io.WriteString(w, `{"ok":true,"next_report_in":60,"server_time":1755000001}`)
}

func TestSendShapeAndHeaders(t *testing.T) {
	client, c := serve(t, ok)

	resp, err := client.Send(context.Background(), request(sample()))
	if err != nil {
		t.Fatalf("Send returned an error: %v", err)
	}
	if !resp.OK || resp.NextReportIn != 60 {
		t.Fatalf("the response was parsed incorrectly: %+v", resp)
	}

	if got := c.headers.Get("Authorization"); got != "Bearer "+token {
		t.Fatalf("Authorization = %q", got)
	}
	if got := c.headers.Get("Content-Type"); got != "application/json" {
		t.Fatalf("Content-Type = %q", got)
	}
	if got := c.headers.Get("User-Agent"); got != "metrics-agent/1.0.0" {
		t.Fatalf("User-Agent = %q", got)
	}
	if got := c.headers.Get("Content-Encoding"); got != "" {
		t.Fatalf("a small body was sent compressed: Content-Encoding = %q", got)
	}

	var body map[string]any
	if err := json.Unmarshal(c.body, &body); err != nil {
		t.Fatalf("the body did not parse: %v", err)
	}
	for _, key := range []string{"agent_version", "hostname", "os", "boot_id", "samples"} {
		if _, ok := body[key]; !ok {
			t.Fatalf("the body has no field %q: %s", key, c.body)
		}
	}
	samples, _ := body["samples"].([]any)
	if len(samples) != 1 {
		t.Fatalf("samples has %d elements, want one", len(samples))
	}
	first, _ := samples[0].(map[string]any)
	for _, key := range []string{"ts", "uptime", "cpu", "la", "mem", "swap", "fs"} {
		if _, ok := first[key]; !ok {
			t.Fatalf("the sample has no field %q: %s", key, c.body)
		}
	}
	if first["ts"].(float64) != 1755000000 {
		t.Fatalf("ts was changed: %v", first["ts"])
	}
}

func TestSendGzipsLargeBody(t *testing.T) {
	client, c := serve(t, ok)

	// Many filesystems make the body comfortably larger than the threshold while staying within the limit.
	s := sample()
	for i := 0; i < transport.MaxFilesystems; i++ {
		s.FS = append(s.FS, metrics.Filesystem{
			Mount:  "/mnt/very-long-mount-point-name-" + strings.Repeat("x", 40) + string(rune('a'+i)),
			FSType: "ext4", Total: 1 << 40, Used: 1 << 30, InodesTotal: 1 << 20, InodesUsed: 1 << 10,
		})
	}

	if _, err := client.Send(context.Background(), request(s)); err != nil {
		t.Fatalf("Send returned an error: %v", err)
	}
	if got := c.headers.Get("Content-Encoding"); got != "gzip" {
		t.Fatalf("a large body was sent uncompressed: Content-Encoding = %q", got)
	}

	zr, err := gzip.NewReader(bytes.NewReader(c.body))
	if err != nil {
		t.Fatalf("the body did not decompress: %v", err)
	}
	raw, err := io.ReadAll(zr)
	if err != nil {
		t.Fatalf("the body did not decompress: %v", err)
	}
	if len(raw) <= transport.GzipThreshold {
		t.Fatalf("the decompressed body is %d bytes, want more than the threshold", len(raw))
	}
	var body transport.Request
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatalf("the decompressed body did not parse: %v", err)
	}
	if n := len(body.Samples[0].FS); n != transport.MaxFilesystems {
		t.Fatalf("the sample has %d filesystems, want the limit of %d", n, transport.MaxFilesystems)
	}
}

func TestSendRejectsOversizedBatch(t *testing.T) {
	client, c := serve(t, ok)

	samples := make([]*metrics.Sample, transport.MaxSamples+1)
	for i := range samples {
		samples[i] = sample()
	}
	_, err := client.Send(context.Background(), request(samples...))
	if kind(t, err) != transport.KindDrop {
		t.Fatalf("an over-limit batch was not rejected: %v", err)
	}
	if c.calls != 0 {
		t.Fatalf("an over-limit batch went out over the network")
	}
}

func TestSendClassifiesStatuses(t *testing.T) {
	cases := []struct {
		status int
		want   transport.Kind
	}{
		{http.StatusUnauthorized, transport.KindUnauthorized},
		{http.StatusTooManyRequests, transport.KindRateLimit},
		{http.StatusBadRequest, transport.KindDrop},
		{http.StatusRequestEntityTooLarge, transport.KindDrop},
		{http.StatusInternalServerError, transport.KindRetry},
		{http.StatusBadGateway, transport.KindRetry},
	}
	for _, tc := range cases {
		client, _ := serve(t, func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(tc.status)
		})
		_, err := client.Send(context.Background(), request(sample()))
		if got := kind(t, err); got != tc.want {
			t.Fatalf("HTTP %d gave kind %d, want %d", tc.status, got, tc.want)
		}
	}
}

func TestSendReadsRetryAfter(t *testing.T) {
	client, _ := serve(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Retry-After", "30")
		w.WriteHeader(http.StatusTooManyRequests)
	})
	_, err := client.Send(context.Background(), request(sample()))
	var sendErr *transport.Error
	if !errors.As(err, &sendErr) {
		t.Fatalf("the error is not *transport.Error: %v", err)
	}
	if sendErr.RetryAfter != 30*time.Second {
		t.Fatalf("Retry-After = %v, want 30s", sendErr.RetryAfter)
	}
}

func TestSendGivesUpOnHungServer(t *testing.T) {
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	client, _ := serve(t, func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
		}
	})

	// The client timeout is comfortably smaller than report_interval; waiting longer is not allowed.
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()

	start := time.Now()
	_, err := client.Send(ctx, request(sample()))
	if kind(t, err) != transport.KindRetry {
		t.Fatalf("a hung server gave something other than KindRetry: %v", err)
	}
	if d := time.Since(start); d > time.Second {
		t.Fatalf("the request hung for %v, want a timeout interruption", d)
	}
	if transport.DefaultTimeout >= 60*time.Second {
		t.Fatalf("the request timeout %v is not smaller than report_interval", transport.DefaultTimeout)
	}
}

func TestTokenNeverLogged(t *testing.T) {
	var buf bytes.Buffer
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()

	client := transport.NewClient(logging.New("debug", &buf), srv.URL, token, "metrics-agent/1.0.0")
	_, err := client.Send(context.Background(), request(sample()))
	if err == nil {
		t.Fatal("want an error")
	}
	if strings.Contains(err.Error(), token) {
		t.Fatalf("the token leaked into the error text: %s", err)
	}
	if strings.Contains(buf.String(), token) {
		t.Fatalf("the token leaked into the log: %s", buf.String())
	}
}

func kind(t *testing.T, err error) transport.Kind {
	t.Helper()
	var sendErr *transport.Error
	if !errors.As(err, &sendErr) {
		t.Fatalf("the error is not *transport.Error: %v", err)
	}
	return sendErr.Kind
}
