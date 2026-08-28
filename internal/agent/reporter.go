package agent

import (
	"context"
	"errors"
	"log/slog"
	"math/rand"
	"time"

	"metrics-agent/internal/metrics"
	"metrics-agent/internal/transport"
)

// UnauthorizedRetryInterval is how often we retry sending after a 401.
// We neither exit nor hammer the endpoint: the token may be reissued and the config updated.
const UnauthorizedRetryInterval = 15 * time.Minute

// SuspendedInterval is the minimum report interval while intake is suspended by the plan.
const SuspendedInterval = 15 * time.Minute

// SuspendedLogInterval is how often we remind the log about the suspension: once an hour, not on every tick.
const SuspendedLogInterval = time.Hour

// Bounds of the interval the agent is willing to accept from the server.
const (
	MinReportInterval = 10 * time.Second
	MaxReportInterval = 24 * time.Hour
)

// Retry parameters for 5xx, timeouts and network errors.
const (
	defaultAttempts    = 3
	defaultBaseBackoff = 2 * time.Second
)

// Buffer is the local buffer of samples that did not reach the backend.
type Buffer interface {
	// Add stores samples until better times.
	Add(samples []*metrics.Sample)
	// Take returns up to limit of the oldest samples and a function that removes them
	// from the buffer after a successful send. An empty buffer yields a nil slice and a nil function.
	Take(limit int) ([]*metrics.Sample, func(), error)
}

// Reporter delivers a sample to the backend and handles the response: applies the interval,
// decides the fate of the data and keeps the sparse mode after a 401 or 429.
//
// Report is not thread-safe: it is meant to be called from a single agent loop.
type Reporter struct {
	log          *slog.Logger
	client       *transport.Client
	loop         *Loop
	buffer       Buffer
	host         metrics.Host
	agentVersion string

	attempts    int
	baseBackoff time.Duration
	now         func() time.Time
	sleep       func(ctx context.Context, d time.Duration) bool

	// notBefore: we do not attempt a send before this moment (Retry-After or sparse mode).
	notBefore time.Time
	// unauthorized distinguishes the sparse mode after a 401 from the pause after a 429:
	// in the first case the sample is dropped, in the second it is kept.
	unauthorized     bool
	lastSuspendedLog time.Time
}

// NewReporter assembles the reporter on top of the client and the loop.
func NewReporter(log *slog.Logger, client *transport.Client, loop *Loop, buffer Buffer, host metrics.Host, agentVersion string) *Reporter {
	return &Reporter{
		log:          log,
		client:       client,
		loop:         loop,
		buffer:       buffer,
		host:         host,
		agentVersion: agentVersion,
		attempts:     defaultAttempts,
		baseBackoff:  defaultBaseBackoff,
		now:          time.Now,
		sleep:        sleepCtx,
	}
}

// Report sends a single sample. nil means a skipped sample and is silently ignored.
func (r *Reporter) Report(ctx context.Context, sample *metrics.Sample) {
	if sample == nil {
		return
	}
	samples := []*metrics.Sample{sample}

	if now := r.now(); now.Before(r.notBefore) {
		if r.unauthorized {
			// The token was rejected: the server will not accept this sample either, no point keeping it.
			r.log.Debug("sample dropped: token rejected by the server")
			return
		}
		r.log.Debug("sending postponed by the server, sample buffered", "until", r.notBefore.Format(time.RFC3339))
		r.buffer.Add(samples)
		return
	}
	if r.send(ctx, samples) {
		// Flush the backlog only after the current sample got through:
		// it must be on time even with a large buffer.
		r.drain(ctx)
	}
}

// send performs the batch send attempts and decides what to do with the data.
// The key distinction: 4xx other than 429 drops the data, 5xx and network errors keep it.
// Returns true if the server accepted the whole batch.
func (r *Reporter) send(ctx context.Context, samples []*metrics.Sample) bool {
	backoff := r.baseBackoff
	for attempt := 1; ; attempt++ {
		resp, err := r.trySend(ctx, samples)
		if err == nil {
			return r.applyResponse(resp, samples)
		}

		var sendErr *transport.Error
		if !errors.As(err, &sendErr) {
			sendErr = &transport.Error{Kind: transport.KindRetry, Err: err}
		}

		switch sendErr.Kind {
		case transport.KindUnauthorized:
			r.unauthorized = true
			r.notBefore = r.now().Add(UnauthorizedRetryInterval)
			r.log.Error("the server rejected the token, switching to sparse retries",
				"status", sendErr.Status, "retry_in", UnauthorizedRetryInterval.String())
			return false

		case transport.KindDrop:
			r.log.Error("the server will never accept this sample, dropping it",
				"status", sendErr.Status, "err", sendErr.Err)
			return false

		case transport.KindTooLarge:
			// Only an indivisible remainder reaches this point: a single sample that on its own
			// does not fit the limit. It cannot be kept in the buffer forever.
			r.log.Error("sample does not fit the request body limit, dropping it", "err", sendErr.Err)
			return false

		case transport.KindRateLimit:
			delay := sendErr.RetryAfter
			if delay <= 0 {
				delay = backoff
			}
			r.notBefore = r.now().Add(delay)
			r.log.Warn("the server rate-limited us, sample buffered", "retry_after", delay.String())
			r.buffer.Add(samples)
			return false

		default:
			if attempt >= r.attempts || ctx.Err() != nil {
				r.log.Warn("send failed, sample buffered", "attempts", attempt, "err", err)
				r.buffer.Add(samples)
				return false
			}
			delay := jitter(backoff)
			if limit := r.loop.Interval(); delay > limit {
				delay = limit
			}
			r.log.Debug("retrying the send", "in", delay.String(), "err", err)
			if !r.sleep(ctx, delay) {
				r.buffer.Add(samples)
				return false
			}
			backoff *= 2
		}
	}
}

// trySend performs a single batch send attempt, splitting the batch in half until the body
// fits into MaxBodyBytes. A size encoding error is not a server rejection:
// the server never saw the data, so it must not be dropped.
//
// Returns the response for the last chunk sent. If one chunk got through and the next
// did not, the caller keeps the whole batch and the delivered part will be sent
// again: a duplicate on intake is cheaper than a lost sample.
func (r *Reporter) trySend(ctx context.Context, samples []*metrics.Sample) (*transport.Response, error) {
	resp, err := r.client.Send(ctx, r.request(samples))

	var sendErr *transport.Error
	if !errors.As(err, &sendErr) || sendErr.Kind != transport.KindTooLarge || len(samples) < 2 {
		return resp, err
	}

	half := len(samples) / 2
	r.log.Warn("batch does not fit the request body limit, splitting it in half",
		"count", len(samples), "err", sendErr.Err)
	if _, err := r.trySend(ctx, samples[:half]); err != nil {
		return nil, err
	}
	return r.trySend(ctx, samples[half:])
}

// applyResponse handles the fields of a successful response.
// Returns false if the server accepted the batch partially and the data must be kept.
func (r *Reporter) applyResponse(resp *transport.Response, samples []*metrics.Sample) bool {
	r.applyControl(resp)
	if resp.Degraded {
		r.log.Warn("the server accepted the sample partially, keeping it for a later flush")
		r.buffer.Add(samples)
		return false
	}
	return true
}

// applyControl applies the control fields of the response: they are the same for a normal
// send and for a flush. What to do with the batch itself is up to the caller:
// during a flush the data is already in the buffer and must not be added again.
func (r *Reporter) applyControl(resp *transport.Response) {
	// Any accepted response clears the sparse mode: the agent returns
	// to normal operation without a restart.
	r.unauthorized = false
	r.notBefore = time.Time{}

	if resp.ClockSkew != nil && *resp.ClockSkew != 0 {
		// We do not touch the system time and do not adjust ts: the backend computes received_at.
		r.log.Warn("the agent clock diverged from the server, not correcting the time", "clock_skew", *resp.ClockSkew)
	}

	interval := time.Duration(resp.NextReportIn) * time.Second
	if resp.Suspended {
		if interval < SuspendedInterval {
			interval = SuspendedInterval
		}
		if now := r.now(); now.Sub(r.lastSuspendedLog) >= SuspendedLogInterval {
			r.lastSuspendedLog = now
			r.log.Warn("intake suspended by the server, interval increased", "interval", interval.String())
		}
	} else {
		r.lastSuspendedLog = time.Time{}
	}
	if interval > 0 {
		r.loop.SetInterval(clampInterval(interval))
	}
}

// request builds the request body around the batch.
func (r *Reporter) request(samples []*metrics.Sample) *transport.Request {
	return &transport.Request{
		AgentVersion: r.agentVersion,
		Hostname:     r.host.Hostname,
		OS:           r.host.OS,
		BootID:       r.host.BootID,
		Samples:      samples,
	}
}

// drain flushes one batch from the buffer in a separate request. At most one batch
// per tick: after a long downtime a thousand agents must not spike the intake.
// There are no retries here: the data is already on disk, the next attempt is on the next tick.
func (r *Reporter) drain(ctx context.Context) {
	samples, commit, err := r.buffer.Take(transport.MaxSamples)
	if err != nil {
		r.log.Warn("failed to read the buffer", "err", err)
		return
	}
	if len(samples) == 0 {
		return
	}

	resp, err := r.trySend(ctx, samples)
	if err != nil {
		var sendErr *transport.Error
		if !errors.As(err, &sendErr) {
			sendErr = &transport.Error{Kind: transport.KindRetry, Err: err}
		}
		switch sendErr.Kind {
		case transport.KindUnauthorized:
			r.unauthorized = true
			r.notBefore = r.now().Add(UnauthorizedRetryInterval)
			r.log.Error("the server rejected the token during a flush", "status", sendErr.Status)
		case transport.KindRateLimit:
			delay := sendErr.RetryAfter
			if delay <= 0 {
				delay = r.baseBackoff
			}
			r.notBefore = r.now().Add(delay)
			r.log.Warn("the server rate-limited us during a flush", "retry_after", delay.String())
		case transport.KindDrop:
			// The server will never accept this batch: it cannot be kept in the buffer forever.
			r.log.Error("the server will not accept the buffered samples, dropping them",
				"status", sendErr.Status, "count", len(samples))
			commit()
		case transport.KindTooLarge:
			// An indivisible remainder: a single sample that does not fit the limit on its own.
			// Leaving it in the buffer would block flushing forever.
			r.log.Error("buffered sample does not fit the request body limit, dropping it",
				"count", len(samples), "err", sendErr.Err)
			commit()
		default:
			r.log.Warn("flush failed, samples stay in the buffer", "count", len(samples), "err", err)
		}
		return
	}

	r.applyControl(resp)
	if resp.Degraded {
		r.log.Warn("the server accepted the flush partially, samples stay in the buffer", "count", len(samples))
		return
	}
	commit()
	r.log.Info("buffered samples flushed", "count", len(samples))
}

func clampInterval(d time.Duration) time.Duration {
	if d < MinReportInterval {
		return MinReportInterval
	}
	if d > MaxReportInterval {
		return MaxReportInterval
	}
	return d
}

// jitter spreads the retry within +/-20% so that a thousand agents do not arrive at once.
func jitter(d time.Duration) time.Duration {
	return time.Duration(float64(d) * (0.8 + 0.4*rand.Float64()))
}

// sleepCtx waits for d or for ctx cancellation. false means cancellation.
func sleepCtx(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}
