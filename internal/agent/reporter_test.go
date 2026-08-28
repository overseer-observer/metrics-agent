package agent

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"io"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"metrics-agent/internal/logging"
	"metrics-agent/internal/metrics"
	"metrics-agent/internal/transport"
)

const reportToken = "9f2c1f8a-1111-4222-8333-444455556666"

// fakeBuffer replaces the disk buffer: it accumulates samples in memory and returns them
// in the same order as the disk one.
type fakeBuffer struct {
	samples []*metrics.Sample
	commits int
}

func (b *fakeBuffer) Add(samples []*metrics.Sample) { b.samples = append(b.samples, samples...) }

func (b *fakeBuffer) Take(limit int) ([]*metrics.Sample, func(), error) {
	if len(b.samples) == 0 {
		return nil, nil, nil
	}
	if limit > len(b.samples) {
		limit = len(b.samples)
	}
	batch := b.samples[:limit]
	return batch, func() {
		b.samples = b.samples[len(batch):]
		b.commits++
	}, nil
}

type harness struct {
	reporter *Reporter
	loop     *Loop
	buffer   *fakeBuffer
	log      *bytes.Buffer

	calls  int
	bodies [][]byte
	// clock is the controllable time; slept accumulates retry delays.
	clock time.Time
	slept []time.Duration
}

func newHarness(t *testing.T, handler func(w http.ResponseWriter, r *http.Request)) *harness {
	t.Helper()
	h := &harness{
		buffer: &fakeBuffer{},
		log:    &bytes.Buffer{},
		clock:  time.Date(2026, 8, 29, 12, 0, 0, 0, time.UTC),
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.calls++
		body, _ := io.ReadAll(r.Body)
		h.bodies = append(h.bodies, body)
		handler(w, r)
	}))
	t.Cleanup(srv.Close)

	log := logging.New("debug", h.log)
	h.loop = New(log, reportToken, DefaultReportInterval, func(context.Context) {})
	client := transport.NewClient(log, srv.URL, reportToken, "metrics-agent/test")
	h.reporter = NewReporter(log, client, h.loop, h.buffer, metrics.Host{Hostname: "web-01"}, "metrics-agent test")

	h.reporter.now = func() time.Time { return h.clock }
	h.reporter.baseBackoff = time.Millisecond
	h.reporter.sleep = func(_ context.Context, d time.Duration) bool {
		h.slept = append(h.slept, d)
		return true
	}
	return h
}

func (h *harness) report(ts int64) {
	h.reporter.Report(context.Background(), &metrics.Sample{TS: ts})
}

func (h *harness) warnings(substr string) int {
	n := 0
	for _, line := range strings.Split(strings.TrimSpace(h.log.String()), "\n") {
		if line == "" {
			continue
		}
		var rec map[string]any
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			continue
		}
		if msg, _ := rec["msg"].(string); strings.Contains(msg, substr) {
			n++
		}
	}
	return n
}

// sentTS extracts ts from the i-th request that reached the server.
func (h *harness) sentTS(t *testing.T, i int) int64 {
	t.Helper()
	var req transport.Request
	if err := json.Unmarshal(h.bodies[i], &req); err != nil {
		t.Fatalf("request body %d did not parse: %v", i, err)
	}
	if len(req.Samples) != 1 {
		t.Fatalf("request %d has %d samples, want one", i, len(req.Samples))
	}
	return req.Samples[0].TS
}

func respond(w http.ResponseWriter, body string) {
	w.Header().Set("Content-Type", "application/json")
	_, _ = io.WriteString(w, body)
}

func TestNextReportInChangesTick(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, _ *http.Request) {
		respond(w, `{"ok":true,"next_report_in":300}`)
	})
	h.report(1755000000)

	if got := h.loop.Interval(); got != 300*time.Second {
		t.Fatalf("interval = %v, want 300s", got)
	}
	if len(h.buffer.samples) != 0 {
		t.Fatalf("an accepted sample ended up in the buffer")
	}
}

func TestSuspendedKeepsRunningAndDoesNotSpamLog(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, _ *http.Request) {
		respond(w, `{"ok":true,"suspended":true}`)
	})

	// An hour of consecutive ticks: there must be exactly one message.
	for i := 0; i < 60; i++ {
		h.report(1755000000 + int64(i)*60)
		h.clock = h.clock.Add(time.Minute)
	}
	if h.calls != 60 {
		t.Fatalf("the agent stopped contacting the server: %d requests out of 60", h.calls)
	}
	if n := h.warnings("intake suspended"); n != 1 {
		t.Fatalf("%d suspension messages, want one per hour", n)
	}
	if got := h.loop.Interval(); got < SuspendedInterval {
		t.Fatalf("interval = %v, want an increase to %v", got, SuspendedInterval)
	}

	// The next hour brings one more reminder, but no more.
	h.clock = h.clock.Add(time.Hour)
	h.report(1755010000)
	if n := h.warnings("intake suspended"); n != 2 {
		t.Fatalf("%d suspension messages, want two over two hours", n)
	}
}

func TestClockSkewWarnsAndLeavesTSAlone(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, _ *http.Request) {
		respond(w, `{"ok":true,"clock_skew":42}`)
	})
	h.report(1755000000)
	h.report(1755000060)

	if n := h.warnings("the agent clock diverged"); n != 2 {
		t.Fatalf("%d divergence warnings, want two", n)
	}
	if !strings.Contains(h.log.String(), `"clock_skew":42`) {
		t.Fatalf("the log has no divergence value: %s", h.log.String())
	}
	if got := h.sentTS(t, 1); got != 1755000060 {
		t.Fatalf("ts of the next sample = %d, want it unchanged at 1755000060", got)
	}
}

func TestUnauthorizedGoesSparseAndRecovers(t *testing.T) {
	deny := true
	h := newHarness(t, func(w http.ResponseWriter, _ *http.Request) {
		if deny {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		respond(w, `{"ok":true,"next_report_in":60}`)
	})

	h.report(1755000000)
	if h.calls != 1 {
		t.Fatalf("401 led to %d requests, want one", h.calls)
	}

	// For the next 14 minutes of ticks the agent neither touches the endpoint nor exits.
	for i := 1; i <= 14; i++ {
		h.clock = h.clock.Add(time.Minute)
		h.report(1755000000 + int64(i)*60)
	}
	if h.calls != 1 {
		t.Fatalf("%d requests were sent in sparse mode, want one", h.calls)
	}
	if len(h.buffer.samples) != 0 {
		t.Fatalf("samples accumulate in the buffer with an invalid token: %d", len(h.buffer.samples))
	}

	// The token has been reissued: the next sparse attempt succeeds.
	h.clock = h.clock.Add(2 * time.Minute)
	deny = false
	h.report(1755001000)
	if h.calls != 2 {
		t.Fatalf("no request was sent after the pause: %d", h.calls)
	}
	if got := h.loop.Interval(); got != DefaultReportInterval {
		t.Fatalf("interval after recovery = %v", got)
	}

	// And the agent returned to its normal pace without a restart.
	h.clock = h.clock.Add(time.Minute)
	h.report(1755001060)
	if h.calls != 3 {
		t.Fatalf("the agent did not return to the normal pace: %d requests", h.calls)
	}
}

func TestRateLimitRespectsRetryAfter(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Retry-After", "30")
		w.WriteHeader(http.StatusTooManyRequests)
	})

	h.report(1755000000)
	if h.calls != 1 {
		t.Fatalf("429 was retried: %d requests", h.calls)
	}
	if len(h.buffer.samples) != 1 {
		t.Fatalf("the sample was not kept on 429: %d", len(h.buffer.samples))
	}

	// There are no attempts within 30 seconds.
	h.clock = h.clock.Add(29 * time.Second)
	h.report(1755000030)
	if h.calls != 1 {
		t.Fatalf("an attempt before Retry-After: %d requests", h.calls)
	}
	if len(h.buffer.samples) != 2 {
		t.Fatalf("the postponed sample was not kept: %d", len(h.buffer.samples))
	}

	h.clock = h.clock.Add(2 * time.Second)
	h.report(1755000060)
	if h.calls != 2 {
		t.Fatalf("no attempt after Retry-After: %d requests", h.calls)
	}
}

func TestPermanentRejectionIsNotRetried(t *testing.T) {
	for _, status := range []int{http.StatusBadRequest, http.StatusRequestEntityTooLarge} {
		h := newHarness(t, func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(status)
		})
		h.report(1755000000)

		if h.calls != 1 {
			t.Fatalf("HTTP %d was retried: %d requests", status, h.calls)
		}
		if len(h.buffer.samples) != 0 {
			t.Fatalf("HTTP %d left the sample in the buffer", status)
		}
		if len(h.slept) != 0 {
			t.Fatalf("HTTP %d caused a delay before a retry", status)
		}
	}
}

func TestServerErrorRetriesThenBuffers(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	})
	h.report(1755000000)

	if h.calls != defaultAttempts {
		t.Fatalf("%d attempts, want %d", h.calls, defaultAttempts)
	}
	if len(h.slept) != defaultAttempts-1 {
		t.Fatalf("%d delays, want %d", len(h.slept), defaultAttempts-1)
	}
	// The +/-20% jitter must not break the growth: the next delay is noticeably larger than the previous one.
	for i := 1; i < len(h.slept); i++ {
		if h.slept[i] <= h.slept[i-1] {
			t.Fatalf("the delays do not grow: %v", h.slept)
		}
	}
	if len(h.buffer.samples) != 1 || h.buffer.samples[0].TS != 1755000000 {
		t.Fatalf("the sample was not handed to the buffer: %+v", h.buffer.samples)
	}
}

func TestRetryDelayCappedByInterval(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	})
	h.reporter.baseBackoff = time.Hour
	h.report(1755000000)

	for _, d := range h.slept {
		if d > h.loop.Interval() {
			t.Fatalf("delay %v is larger than report_interval %v", d, h.loop.Interval())
		}
	}
}

func TestDegradedKeepsSampleForResend(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, _ *http.Request) {
		respond(w, `{"ok":true,"degraded":true}`)
	})
	h.report(1755000000)

	if len(h.buffer.samples) != 1 {
		t.Fatalf("a partially accepted sample was not kept for a later flush: %d", len(h.buffer.samples))
	}
}

func TestHungServerDoesNotBlockLongerThanTimeout(t *testing.T) {
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	h := newHarness(t, func(_ http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
		}
	})
	h.reporter.attempts = 1

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()

	start := time.Now()
	h.reporter.Report(ctx, &metrics.Sample{TS: 1755000000})
	if d := time.Since(start); d > time.Second {
		t.Fatalf("the tick took %v with a hung server", d)
	}
	if len(h.buffer.samples) != 1 {
		t.Fatalf("the sample was not kept after a timeout: %d", len(h.buffer.samples))
	}
}

func TestReporterNeverLogsToken(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	})
	h.report(1755000000)

	h2 := newHarness(t, func(w http.ResponseWriter, _ *http.Request) {
		respond(w, `{"ok":true,"next_report_in":300,"clock_skew":5,"suspended":true,"degraded":true}`)
	})
	h2.report(1755000000)

	for _, out := range []string{h.log.String(), h2.log.String()} {
		if strings.Contains(out, reportToken) {
			t.Fatalf("the token leaked into the log: %s", out)
		}
	}
}

// sentSamples extracts the ts of all samples of the i-th request, decompressing the body if needed.
func (h *harness) sentSamples(t *testing.T, i int) []int64 {
	t.Helper()
	raw := h.bodies[i]
	if zr, err := gzip.NewReader(bytes.NewReader(raw)); err == nil {
		raw, err = io.ReadAll(zr)
		if err != nil {
			t.Fatalf("request body %d did not decompress: %v", i, err)
		}
	}
	var req transport.Request
	if err := json.Unmarshal(raw, &req); err != nil {
		t.Fatalf("request body %d did not parse: %v", i, err)
	}
	out := make([]int64, 0, len(req.Samples))
	for _, s := range req.Samples {
		out = append(out, s.TS)
	}
	return out
}

func TestResendGoesInSeparateRequest(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, _ *http.Request) {
		respond(w, `{"ok":true}`)
	})
	h.buffer.Add([]*metrics.Sample{{TS: 1754999880}, {TS: 1754999940}})
	h.report(1755000000)

	if h.calls != 2 {
		t.Fatalf("%d requests, want two: the current sample and the flush", h.calls)
	}
	if got := h.sentSamples(t, 0); len(got) != 1 || got[0] != 1755000000 {
		t.Fatalf("the current sample is mixed with the flush: %v", got)
	}
	if got := h.sentSamples(t, 1); len(got) != 2 || got[0] != 1754999880 || got[1] != 1754999940 {
		t.Fatalf("the flush is not in chronological order: %v", got)
	}
	if len(h.buffer.samples) != 0 {
		t.Fatalf("%d samples remain in the buffer after the flush", len(h.buffer.samples))
	}
}

func TestResendOneBatchPerTick(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, _ *http.Request) {
		respond(w, `{"ok":true}`)
	})
	for i := 0; i < transport.MaxSamples+10; i++ {
		h.buffer.Add([]*metrics.Sample{{TS: int64(1754000000 + i)}})
	}
	h.report(1755000000)

	if h.calls != 2 {
		t.Fatalf("%d requests, want two: at most one flush batch per tick", h.calls)
	}
	if got := h.sentSamples(t, 1); len(got) != transport.MaxSamples {
		t.Fatalf("the flush batch has %d samples, want %d", len(got), transport.MaxSamples)
	}
	if len(h.buffer.samples) != 10 {
		t.Fatalf("%d samples remain in the buffer, want 10", len(h.buffer.samples))
	}
}

func TestFailedResendKeepsBuffer(t *testing.T) {
	seen := 0
	h := newHarness(t, func(w http.ResponseWriter, _ *http.Request) {
		seen++
		if seen == 1 {
			respond(w, `{"ok":true}`)
			return
		}
		w.WriteHeader(http.StatusInternalServerError)
	})
	h.buffer.Add([]*metrics.Sample{{TS: 1754999940}})
	h.report(1755000000)

	if h.calls != 2 {
		t.Fatalf("%d requests, want two", h.calls)
	}
	if h.buffer.commits != 0 {
		t.Fatal("a failed flush removed data from the buffer")
	}
	if len(h.buffer.samples) != 1 {
		t.Fatalf("%d samples in the buffer, want one", len(h.buffer.samples))
	}
}

func TestNoResendWhenCurrentSampleFails(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	})
	h.buffer.Add([]*metrics.Sample{{TS: 1754999940}})
	h.report(1755000000)

	if h.calls != defaultAttempts {
		t.Fatalf("%d requests: the flush ran while the server was unavailable", h.calls)
	}
}

// fatSample is a sample that barely compresses: long random paths.
// Such a batch hits MaxBodyBytes and must be split, not dropped.
func fatSample(rnd *rand.Rand, ts int64) *metrics.Sample {
	const alphabet = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789+-"
	s := &metrics.Sample{TS: ts}
	for i := 0; i < transport.MaxFilesystems; i++ {
		b := make([]byte, 200)
		for j := range b {
			b[j] = alphabet[rnd.Intn(len(alphabet))]
		}
		s.FS = append(s.FS, metrics.Filesystem{
			Mount:  "/mnt/" + string(b),
			FSType: "ext4",
			Total:  1 << 40,
			Used:   1 << 20,
		})
	}
	return s
}

func TestOversizeBatchIsSplitNotDropped(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, _ *http.Request) {
		respond(w, `{"ok":true}`)
	})

	rnd := rand.New(rand.NewSource(1))
	want := make(map[int64]int, transport.MaxSamples)
	fat := make([]*metrics.Sample, 0, transport.MaxSamples)
	for i := 1; i <= transport.MaxSamples; i++ {
		fat = append(fat, fatSample(rnd, int64(i)))
		want[int64(i)] = 0
	}
	h.buffer.Add(fat)

	// A successful send of the current sample triggers the buffer flush.
	h.report(1755000000)

	if len(h.buffer.samples) != 0 {
		t.Fatalf("%d samples remain in the buffer, want an empty buffer", len(h.buffer.samples))
	}
	if h.calls < 3 {
		t.Fatalf("%d requests: the batch was not split", h.calls)
	}
	for i := 1; i < len(h.bodies); i++ {
		for _, ts := range h.sentSamples(t, i) {
			want[ts]++
		}
	}
	for ts := int64(1); ts <= transport.MaxSamples; ts++ {
		if want[ts] != 1 {
			t.Fatalf("sample ts=%d was sent %d times, want exactly one", ts, want[ts])
		}
	}
}

// TestResendResponseAppliesInterval: the response to a flush is the same control
// channel as the response to a normal send. next_report_in from it is not lost.
func TestResendResponseAppliesInterval(t *testing.T) {
	n := 0
	h := newHarness(t, func(w http.ResponseWriter, _ *http.Request) {
		n++
		if n == 1 {
			respond(w, `{"ok":true}`)
			return
		}
		respond(w, `{"ok":true,"next_report_in":300}`)
	})
	h.buffer.Add([]*metrics.Sample{{TS: 1754999880}})
	h.report(1755000000)

	if h.calls != 2 {
		t.Fatalf("%d requests, want two", h.calls)
	}
	if got := h.loop.Interval(); got != 300*time.Second {
		t.Fatalf("interval %s, want 5m: the flush response was ignored", got)
	}
}
